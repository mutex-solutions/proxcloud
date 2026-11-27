//go:build integration

package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// kubeletSaid marks what the fake cluster says of a pod in its own words (the kubelet's message of a failed pull).
// It is for the operator's log alone: no answer, record or audit event may carry it.
const kubeletSaid = "kubelet-said"

// fnCluster is an in-memory function cluster behind function.K8sAPI: namespaces, Deployments and Secrets, with a
// Deployment controller that settles at once and a garbage collector (a Secret goes with the Deployment that owns
// it). An image starting with "bad" never becomes ready (its rollout reports the progress deadline as exceeded), nor
// does one in a private registry whose password none of the pod's pull secrets has: unless the node has the image
// already and the pod does not ask for a pull on every start (imagePullPolicy Always).
type fnCluster struct {
	mu         sync.Mutex
	namespaces map[string]map[string]string
	deps       map[string]map[string]any // "ns/name"
	secrets    map[string]map[string]any // "ns/name"
	uids       int
	fail       map[string]error // by method (ApplyCR, MergePatchCR, MergePatchCore, DeleteNamespace): what it fails with
	// patched are the labels and annotations a merge patch set ("ns/name/labels/key"): they have another field owner
	// than the apply, which therefore leaves them as they are.
	patched map[string]bool
	// registry holds the private registries: host -> the password it takes.
	registry map[string]string
	// cached are the images the node has pulled: it starts those without asking the registry.
	cached map[string]bool
	// moved are the tags that point at other bits than when they were first pulled: tag -> what they point at now.
	moved map[string]string
	// quotas are the ResourceQuotas as last applied, by namespace. calls counts the cluster calls by method.
	quotas map[string]map[string]any
	calls  map[string]int
	// hold makes EnsureNamespace wait until so many callers are in it at once (or 3 s pass), then lets them all go:
	// every one of them has by then passed whatever a request does before it.
	hold, held int
	release    chan struct{}
}

// barrier arms hold for n callers.
func (c *fnCluster) barrier(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hold, c.held, c.release = n, 0, make(chan struct{})
}

func newFnCluster() *fnCluster {
	return &fnCluster{namespaces: map[string]map[string]string{}, deps: map[string]map[string]any{}, secrets: map[string]map[string]any{}, calls: map[string]int{}}
}

// called counts a call and returns what the method is set to fail with. The caller holds the lock.
func (c *fnCluster) called(method string) error {
	c.calls[method]++
	return c.fail[method]
}

// pullError is what the kubelet says of a Deployment whose image cannot be pulled ("" when it can). Like the real one
// it looks credentials up by the image's registry.
func (c *fnCluster) pullError(dep map[string]any) string {
	image := depImage(dep)
	if strings.HasPrefix(image, "bad") {
		return kubeletSaid + ": manifest unknown on node worker-1"
	}
	host, _, _ := strings.Cut(image, "/")
	want, private := c.registry[host]
	if !private {
		return ""
	}
	pod := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if c.cached[image] && pod["containers"].([]any)[0].(map[string]any)["imagePullPolicy"] != "Always" {
		return ""
	}
	refs, _ := pod["imagePullSecrets"].([]any)
	for _, ref := range refs {
		sec := c.secrets[dep["metadata"].(map[string]any)["namespace"].(string)+"/"+ref.(map[string]any)["name"].(string)]
		data, _ := sec["data"].(map[string]any)
		raw, _ := data[".dockerconfigjson"].(string)
		b, _ := base64.StdEncoding.DecodeString(raw)
		var cfg struct {
			Auths map[string]struct{ Password string }
		}
		if sec["type"] != "kubernetes.io/dockerconfigjson" || json.Unmarshal(b, &cfg) != nil {
			continue
		}
		for server, auth := range cfg.Auths {
			server, _, _ = strings.Cut(strings.TrimPrefix(server, "https://"), "/")
			if server == host && auth.Password == want {
				return ""
			}
		}
	}
	return kubeletSaid + ": unauthorized: authentication required at " + host + " from node worker-1"
}

func cloneObj(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func depImage(dep map[string]any) string {
	tpl := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	return tpl["containers"].([]any)[0].(map[string]any)["image"].(string)
}

// imageID is what the container runtime reports of a started image: its repository and the digest it resolved.
func (c *fnCluster) imageID(image string) string {
	if repo, digest, byDigest := strings.Cut(image, "@"); byDigest {
		return repo + "@" + digest
	}
	repo := image
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	sum := sha256.Sum256([]byte(image + "|" + c.moved[image]))
	return repo + "@sha256:" + hex.EncodeToString(sum[:])
}

// settle is the controller: the status follows the spec immediately.
func (c *fnCluster) settle(dep map[string]any) {
	md := dep["metadata"].(map[string]any)
	gen := intAny(md["generation"]) + 1
	md["generation"] = gen
	replicas := intAny(dep["spec"].(map[string]any)["replicas"])
	st := map[string]any{"observedGeneration": gen, "replicas": replicas, "updatedReplicas": replicas}
	if replicas > 0 && c.pullError(dep) != "" {
		st["conditions"] = []any{map[string]any{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded"}}
	} else {
		st["availableReplicas"], st["readyReplicas"] = replicas, replicas
		if c.cached == nil {
			c.cached = map[string]bool{}
		}
		c.cached[depImage(dep)] = replicas > 0 || c.cached[depImage(dep)]
	}
	dep["status"] = st
}

func (c *fnCluster) EnsureNamespace(_ context.Context, name string, labels, _ map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hold > 0 {
		if c.held++; c.held == c.hold {
			close(c.release)
		}
		release := c.release
		c.mu.Unlock()
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		c.mu.Lock()
	}
	if err := c.called("EnsureNamespace"); err != nil {
		return err
	}
	c.namespaces[name] = labels
	return nil
}
func (c *fnCluster) GetNamespace(_ context.Context, name string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.namespaces[name]
	if !ok {
		return nil, nil
	}
	labels := map[string]any{}
	for k, v := range l {
		labels[k] = v
	}
	return map[string]any{"metadata": map[string]any{"name": name, "labels": labels}}, nil
}
func (c *fnCluster) ListNamespaces(context.Context, string) ([]map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for name := range c.namespaces {
		out = append(out, map[string]any{"metadata": map[string]any{"name": name}})
	}
	return out, nil
}
func (c *fnCluster) DeleteNamespace(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.called("DeleteNamespace"); err != nil {
		return err
	}
	delete(c.namespaces, name)
	delete(c.quotas, name)
	for k := range c.deps {
		if strings.HasPrefix(k, name+"/") {
			delete(c.deps, k)
		}
	}
	for k := range c.secrets {
		if strings.HasPrefix(k, name+"/") {
			delete(c.secrets, k)
		}
	}
	return nil
}
func (c *fnCluster) ApplyResourceQuota(_ context.Context, rq map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.quotas == nil {
		c.quotas = map[string]map[string]any{}
	}
	c.quotas[rq["metadata"].(map[string]any)["namespace"].(string)] = cloneObj(rq)
	return nil
}

// quota is the namespace's ResourceQuota as last written (nil before it ever was).
func (c *fnCluster) quota(ns string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	hard, _ := c.quotas[ns]["spec"].(map[string]any)["hard"].(map[string]any)
	return hard
}
func (c *fnCluster) ApplyCR(_ context.Context, _, _, _ string, obj map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.called("ApplyCR"); err != nil {
		return err
	}
	obj = cloneObj(obj)
	md := obj["metadata"].(map[string]any)
	key := md["namespace"].(string) + "/" + md["name"].(string)
	replicas := 1 // the API default of a Deployment applied without spec.replicas
	if old := c.deps[key]; old != nil {
		replicas = intAny(old["spec"].(map[string]any)["replicas"])
		md["generation"], md["uid"] = old["metadata"].(map[string]any)["generation"], old["metadata"].(map[string]any)["uid"]
		for _, kind := range []string{"labels", "annotations"} { // what the apply does not own stays
			have, _ := old["metadata"].(map[string]any)[kind].(map[string]any)
			for k, v := range have {
				if _, applied := md[kind].(map[string]any)[k]; !applied && c.patched[key+"/"+kind+"/"+k] {
					md[kind].(map[string]any)[k] = v
				}
			}
		}
	} else {
		c.uids++
		md["uid"] = fmt.Sprintf("uid-%d", c.uids)
	}
	obj["spec"].(map[string]any)["replicas"] = replicas
	c.settle(obj)
	c.deps[key] = obj
	return nil
}
func (c *fnCluster) GetCR(_ context.Context, _, _, _, ns, name string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneObj(c.deps[ns+"/"+name]), nil
}
func (c *fnCluster) DeleteCR(_ context.Context, _, _, _, ns, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dep := c.deps[ns+"/"+name]; dep != nil {
		for k, sec := range c.secrets { // the garbage collector
			if owners, _ := sec["metadata"].(map[string]any)["ownerReferences"].([]any); len(owners) == 1 && owners[0].(map[string]any)["uid"] == dep["metadata"].(map[string]any)["uid"] {
				delete(c.secrets, k)
			}
		}
	}
	delete(c.deps, ns+"/"+name)
	return nil
}
func (c *fnCluster) MergePatchCR(_ context.Context, _, _, _, ns, name string, patch map[string]any) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.called("MergePatchCR"); err != nil {
		return nil, err
	}
	dep := c.deps[ns+"/"+name]
	if dep == nil {
		return nil, &kamajik8s.APIError{Status: http.StatusNotFound}
	}
	if c.patched == nil {
		c.patched = map[string]bool{}
	}
	for _, kind := range []string{"labels", "annotations"} { // metadata is all it is used for
		have := dep["metadata"].(map[string]any)[kind].(map[string]any)
		keys, _ := patch["metadata"].(map[string]any)[kind].(map[string]any)
		for k, v := range keys {
			if c.patched[ns+"/"+name+"/"+kind+"/"+k] = v != nil; v == nil {
				delete(have, k)
			} else {
				have[k] = v
			}
		}
	}
	return cloneObj(dep), nil
}
func (c *fnCluster) ListCRs(_ context.Context, _, _, _, ns, _ string) ([]map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for k, v := range c.deps {
		if strings.HasPrefix(k, ns+"/") {
			out = append(out, cloneObj(v))
		}
	}
	return out, nil
}
func (c *fnCluster) ListCRsAllNamespaces(_ context.Context, _, _, _, _ string) ([]map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, v := range c.deps {
		out = append(out, cloneObj(v))
	}
	return out, nil
}
func (c *fnCluster) SetScale(_ context.Context, _, _, _, ns, name string, replicas int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.called("SetScale"); err != nil {
		return err
	}
	dep := c.deps[ns+"/"+name]
	if dep == nil {
		return &kamajik8s.APIError{Status: http.StatusNotFound}
	}
	dep["spec"].(map[string]any)["replicas"] = replicas
	c.settle(dep)
	return nil
}
func (c *fnCluster) ListPods(_ context.Context, ns, sel string) ([]map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dep := c.deps[ns+"/"+strings.TrimPrefix(sel, function.LabelID+"=")]
	if dep == nil || intAny(dep["spec"].(map[string]any)["replicas"]) == 0 {
		return nil, nil
	}
	ready, status := "True", map[string]any{"name": "main", "imageID": c.imageID(depImage(dep)), "state": map[string]any{"running": map[string]any{}}}
	if msg := c.pullError(dep); msg != "" {
		ready, status = "False", map[string]any{"name": "main", "state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff", "message": msg}}}
	}
	return []map[string]any{{
		"metadata": map[string]any{"name": "pod-1"}, "spec": map[string]any{"nodeName": "worker-1"},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ready}}, "containerStatuses": []any{status}},
	}}, nil
}
func (c *fnCluster) PodLogs(_ context.Context, _, pod, _ string, tail int) (string, error) {
	return fmt.Sprintf("log of %s tail=%d\n", pod, tail), nil
}
func (c *fnCluster) ApplyCore(_ context.Context, plural string, obj map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if md := obj["metadata"].(map[string]any); plural == "secrets" {
		c.secrets[md["namespace"].(string)+"/"+md["name"].(string)] = cloneObj(obj)
	}
	return nil
}
func (c *fnCluster) MergePatchCore(_ context.Context, _, ns, name string, patch map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.called("MergePatchCore"); err != nil {
		return err
	}
	sec := c.secrets[ns+"/"+name]
	if sec == nil {
		return &kamajik8s.APIError{Status: http.StatusNotFound}
	}
	for k, v := range patch { // {"data": null} and {"data": {key: value}} are all it is used for
		if v == nil {
			delete(sec, k)
		} else {
			sec[k] = cloneObj(v.(map[string]any))
		}
	}
	return nil
}

// secret returns a function's Secret (nil when there is none) and its values in the clear.
func (c *fnCluster) secret(ns, name string) (map[string]any, map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sec := cloneObj(c.secrets[ns+"/"+name])
	if sec == nil {
		return nil, nil
	}
	values := map[string]string{}
	data, _ := sec["data"].(map[string]any)
	for k, v := range data {
		b, _ := base64.StdEncoding.DecodeString(v.(string))
		values[k] = string(b)
	}
	return sec, values
}

// replicas of a function's Deployment, -1 when it does not exist.
func (c *fnCluster) replicas(ns, name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dep := c.deps[ns+"/"+name]; dep != nil {
		return intAny(dep["spec"].(map[string]any)["replicas"])
	}
	return -1
}

func (c *fnCluster) get(ns, name string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneObj(c.deps[ns+"/"+name])
}

func (c *fnCluster) secretCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.secrets)
}

// fnEnv is a function API over real PostgreSQL (users, projects, RBAC, records, cloud rows, the provider document
// the functions block is read from) with the cluster faked. The rollout poller is parked (functionPollEvery = 1 h) so
// tests settle rollouts themselves, deterministically.
type fnEnv struct {
	t       *testing.T
	h       *Handler
	ctx     context.Context
	db      *pgdoc.DB
	router  chi.Router
	cluster *fnCluster
	// gates is what the location's release gates answer.
	gates []sandbox.Gate
}

// fnBlock is the functions block the tests' provider starts with.
func fnBlock() map[string]any {
	return map[string]any{"enabled": true, "maxMinInstances": 1, "maxFunctions": 3}
}

func newFnEnv(t *testing.T) *fnEnv {
	t.Helper()
	h, ctx, router := consoleSessionFixture(t)
	db := h.sbTemplates.DB()
	for _, name := range []string{"auditEvent", "cloudResourceHistory"} {
		if err := db.C(name).Ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	h.audit = audit.NewService(audit.NewRepo(db), slog.Default())
	e := &fnEnv{t: t, h: h, ctx: ctx, db: db, router: router, cluster: newFnCluster(), gates: []sandbox.Gate{{Name: "test", OK: true}}}
	h.SetSandboxGates(func(context.Context, *externalservice.ExternalService) []sandbox.Gate { return e.gates })
	e.provider(externalservice.StatusPublic, fnBlock())
	h.SetFunctions(func(es *externalservice.ExternalService) (*function.Service, error) {
		return function.NewWithAPI(e.cluster, es.SandboxConfig(), es.ID), nil // like function.New: also when functions are off
	}, db.C("functions"), db.C("function_deployments"), db.C("function_hosts"), fnTestKeys(t))
	if err := h.EnsureFunctionIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	oldPoll, oldBound := functionPollEvery, functionRolloutBound
	functionPollEvery, functionRolloutBound = time.Hour, time.Hour
	t.Cleanup(func() { functionPollEvery, functionRolloutBound = oldPoll, oldBound })
	return e
}

// provider writes the sandbox provider's status and functions block, as an operator's save would leave them.
func (e *fnEnv) provider(status string, functions map[string]any) {
	e.t.Helper()
	config := map[string]any{"provider": "sandbox", "region": "az1", "sandbox": map[string]any{"domain": "fn.example", "functions": functions}}
	if _, err := e.db.C("externalService").SetByID(e.ctx, "svc", pgdoc.M{"status": status, "config": config}, nil); err != nil {
		e.t.Fatal(err)
	}
	e.h.gateMu.Lock()
	e.h.gateCache = nil // the verdict of the provider as it was
	e.h.gateMu.Unlock()
}

// fs is the function service of the tests' provider, as the routes build it.
func (e *fnEnv) fs() *function.Service {
	e.t.Helper()
	_, fs, err := e.h.functionSvc(e.ctx, "svc")
	if err != nil {
		e.t.Fatal(err)
	}
	return fs
}

// raw makes an API request as sub and returns the status and the whole body.
func (e *fnEnv) raw(sub, method, path, body string) (int, string) {
	e.t.Helper()
	w := consoleSessionRequest(e.router, e.ctx, sub, method, path, body)
	return w.Code, w.Body.String()
}

// call makes an API request as sub and returns the status and the decoded `data` of the envelope.
func (e *fnEnv) call(sub, method, path, body string) (int, map[string]any) {
	e.t.Helper()
	code, out := e.raw(sub, method, path, body)
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	var data map[string]any
	_ = json.Unmarshal(env.Data, &data)
	return code, data
}

func (e *fnEnv) fn(id string) functionDoc {
	e.t.Helper()
	var fn functionDoc
	if found, err := e.h.fns.Get(e.ctx, id, &fn); err != nil || !found {
		e.t.Fatalf("function %s: found=%v err=%v", id, found, err)
	}
	return fn
}

func (e *fnEnv) records() []functionDoc {
	e.t.Helper()
	var fns []functionDoc
	if err := e.h.fns.Find(e.ctx, pgdoc.M{}, &fns, pgdoc.Sort(pgdoc.Asc("name"))); err != nil {
		e.t.Fatal(err)
	}
	return fns
}

// row is the function's live cloud row, nil when it has none.
func (e *fnEnv) row(id string) *cloud.CloudResource {
	e.t.Helper()
	row, err := e.h.cloud.FindByServiceIDAndExternalID(e.ctx, "svc", id)
	if err != nil {
		e.t.Fatal(err)
	}
	return row
}

// archived are the archived rows of a function (all of them with id "").
func (e *fnEnv) archived(id string) []cloud.History {
	e.t.Helper()
	filter := pgdoc.M{"type": cloud.TypeFunction}
	if id != "" {
		filter["externalId"] = id
	}
	var rows []cloud.History
	if err := e.db.C("cloudResourceHistory").Find(e.ctx, filter, &rows); err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *fnEnv) deployments(id string) []functionDeployment {
	e.t.Helper()
	var deps []functionDeployment
	if err := e.h.fnDeps.Find(e.ctx, pgdoc.M{"functionId": id}, &deps, pgdoc.Sort(pgdoc.DescK("seq", pgdoc.KNum))); err != nil {
		e.t.Fatal(err)
	}
	return deps
}

// settle does what one poll of the rollout poller does (final = its last one).
func (e *fnEnv) settle(id string, final bool) bool {
	e.t.Helper()
	fn := e.fn(id)
	var dep functionDeployment
	if found, err := e.h.fnDeps.Get(e.ctx, fn.DeployingID, &dep); err != nil || !found {
		e.t.Fatalf("function %s has no deployment in progress (deployingId %q)", id, fn.DeployingID)
	}
	return e.h.settleDeployment(e.ctx, e.fs(), &dep, final)
}

// create makes a function and returns its id (the rollout is left in progress).
func (e *fnEnv) create(body string) string {
	e.t.Helper()
	code, out := e.raw("owner-p", "POST", "/project/p/functions", body)
	var env struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != 200 || env.Data["status"] != fnDeploying {
		e.t.Fatalf("create: %d %s", code, out)
	}
	return env.Data["id"].(string)
}

// ready makes a function and settles its first rollout.
func (e *fnEnv) ready(body string) string {
	e.t.Helper()
	id := e.create(body)
	if !e.settle(id, false) || e.fn(id).CurrentDeploymentID == "" {
		e.t.Fatalf("the first rollout of %s did not settle: %+v", id, e.fn(id))
	}
	return id
}

// project sets fields of the tests' project document.
func (e *fnEnv) project(set pgdoc.M) {
	e.t.Helper()
	if _, err := e.db.C("project").SetByID(e.ctx, "p", set, nil); err != nil {
		e.t.Fatal(err)
	}
}

// A function keeps what ran. Its arguments reach the container as written (the kubelet expands nothing). An update
// that changes no snapshot deploys nothing. A deployment that fails leaves the template that was serving. A rollback
// to a deployment that ran a tag deploys the digest that deployment ran, whatever the tag points at since.
func TestFunctionKeepsWhatRan(t *testing.T) {
	e := newFnEnv(t)
	path := "/project/p/functions"
	container := func(id string) map[string]any {
		tpl := e.cluster.get("fn-p", id)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		return tpl["containers"].([]any)[0].(map[string]any)
	}
	arg := func(id string) any { return container(id)["args"].([]any)[0] }
	id := e.create(`{"name":"api","image":"nginx:1","args":["--port=$(PORT)"]}`)
	// A rollout is not suspended under: at zero instances it would prove nothing and be recorded as working. Nor
	// is a second one started beside it.
	for _, c := range [][3]string{{"POST", path + "/" + id + "/action", `{"action":"suspend"}`}, {"PATCH", path + "/" + id, `{"image":"nginx:2"}`}} {
		if code, body := e.raw("owner-p", c[0], c[1], c[2]); code != 409 || !strings.Contains(body, "already in progress") {
			t.Fatalf("%s %s during a rollout: %d %s, want 409", c[0], c[1], code, body)
		}
	}
	if fn := e.fn(id); fn.Suspended || len(e.deployments(id)) != 1 || fn.Instances != 1 {
		t.Fatalf("a request refused during a rollout changed the function: %+v", fn)
	}
	if got := arg(id); got != "--port=$$(PORT)" || !e.settle(id, false) {
		t.Fatalf("a new function: args %v, record %+v", got, e.fn(id).Snapshot)
	}
	first := e.fn(id).CurrentDeploymentID
	ran := e.deployments(id)[0].ImageDigest
	if !strings.HasPrefix(ran, "sha256:") || len(ran) != 71 || container(id)["imagePullPolicy"] != "Always" {
		t.Fatalf("the first deployment: digest %q, pull policy %v", ran, container(id)["imagePullPolicy"])
	}

	// An update that deploys nothing changes nothing on the cluster.
	applies := e.cluster.calls["ApplyCR"]
	if code, data := e.call("owner-p", "PATCH", path+"/"+id, `{"idleSeconds":120}`); code != 200 || data["idleSeconds"] != float64(120) || len(e.deployments(id)) != 1 || e.cluster.calls["ApplyCR"] != applies {
		t.Fatalf("an update without a deployment: %d %v", code, data)
	}
	// A deployment that fails: the template that was serving comes back, and the record never took the failed one.
	if code, data := e.call("owner-p", "PATCH", path+"/"+id, `{"image":"bad:1","args":["--other"]}`); code != 200 || data["status"] != fnDeploying || arg(id) != "--other" {
		t.Fatalf("patch to a bad image: %d %v", code, data)
	}
	if !e.settle(id, true) {
		t.Fatal("the final poll must settle")
	}
	fn := e.fn(id)
	if arg(id) != "--port=$$(PORT)" || fn.CurrentDeploymentID != first || fn.Image != "nginx:1" || depImage(e.cluster.get("fn-p", id)) != "nginx:1" || fn.DeployingID != "" {
		t.Fatalf("restored after a failed deployment: args %v, record %+v", arg(id), fn.Snapshot)
	}
	// Why it failed is one of the fixed reasons; the cluster's own words are in no record.
	failed := e.deployments(id)[0]
	if failed.State != depFailed || !slices.Contains(function.Reasons, failed.Message) || fn.LastError != failed.Message || failed.ImageDigest != "" {
		t.Fatalf("the failed deployment: %+v (lastError %q)", failed, fn.LastError)
	}
	// One that succeeds is the function's configuration from then on.
	if code, _ := e.call("owner-p", "PATCH", path+"/"+id, `{"image":"nginx:2","args":["--v2"]}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("patch to another image: %d", code)
	}
	if fn = e.fn(id); fn.Image != "nginx:2" || fn.LastError != "" || arg(id) != "--v2" {
		t.Fatalf("after a deployment: %+v", fn.Snapshot)
	}
	// The tag of the first deployment moves. A rollback to that deployment runs the bits that ran, by digest.
	e.cluster.moved = map[string]string{"nginx:1": "other bits"}
	if code, _ := e.call("owner-p", "POST", path+"/"+id+"/rollback", `{"deploymentId":"`+first+`"}`); code != 200 {
		t.Fatalf("rollback: %d", code)
	}
	if got := depImage(e.cluster.get("fn-p", id)); got != "nginx@"+ran || arg(id) != "--port=$$(PORT)" || !e.settle(id, false) {
		t.Fatalf("a rollback to a deployment of a tag deploys %s, want nginx@%s", got, ran)
	}
	back := e.deployments(id)[0]
	if fn = e.fn(id); fn.Image != "nginx@"+ran || back.RollbackOf != 1 || back.ImageDigest != ran || back.State != depActive {
		t.Fatalf("after the rollback: %+v, deployment %+v", fn.Snapshot, back)
	}
	// Only a deployment that once served can be rolled back to; another function's cannot be named.
	if code, _ := e.call("owner-p", "POST", path+"/"+id+"/rollback", `{"deploymentId":"`+failed.ID+`"}`); code != 400 {
		t.Errorf("rollback to a failed deployment: %d, want 400", code)
	}
	other := e.ready(`{"name":"other","image":"nginx:1"}`)
	if code, _ := e.call("owner-p", "POST", path+"/"+other+"/rollback", `{"deploymentId":"`+first+`"}`); code != 404 {
		t.Errorf("rollback to another function's deployment: %d, want 404", code)
	}
	// Ten records are kept, and the one that serves.
	for i := range 12 {
		if code, _ := e.call("owner-p", "PATCH", path+"/"+other, fmt.Sprintf(`{"env":{"N":"%d"}}`, i)); code != 200 || !e.settle(other, false) {
			t.Fatalf("deployment %d: %d", i, code)
		}
	}
	if deps := e.deployments(other); len(deps) != keepDeployments || deps[0].Seq != 13 || deps[0].ID != e.fn(other).CurrentDeploymentID {
		t.Fatalf("%d deployment records kept, newest seq %d", len(deps), deps[0].Seq)
	}
}

// lockedBuffer collects log output from any goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fnLeakScan is a function API in which every write-only value of a test carries one marker. Whatever the API
// answers, whatever is recorded of a function, a deployment, a cloud row (live or archived) or an audit event,
// whatever is logged and every Deployment are searched for it. It may be in two places only: sealed in the function
// record, and in the data of a Secret on the cluster. The cluster's own words (kubeletSaid) may be in the log and
// nowhere else.
type fnLeakScan struct {
	t         *testing.T
	e         *fnEnv
	mark      string
	logs      *lockedBuffer
	responses []string
}

func newFnLeakScan(t *testing.T, mark string) *fnLeakScan {
	t.Helper()
	logs := &lockedBuffer{}
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLog) })
	return &fnLeakScan{t: t, e: newFnEnv(t), mark: mark, logs: logs}
}

// call is fnEnv.call, keeping the whole response.
func (s *fnLeakScan) call(sub, method, path, body string) (int, map[string]any) {
	s.t.Helper()
	code, out := s.e.raw(sub, method, path, body)
	s.responses = append(s.responses, method+" "+path+": "+out)
	var env struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	return code, env.Data
}

// leaks fails the test for every place the marker shows up where it must not.
func (s *fnLeakScan) leaks(stage string) {
	s.t.Helper()
	for _, r := range s.responses {
		if strings.Contains(r, s.mark) || strings.Contains(r, kubeletSaid) {
			s.t.Errorf("%s: a response carries a write-only value or the cluster's words: %s", stage, r)
		}
	}
	for _, c := range []string{"function_deployments", "function_hosts", "cloudResource", "cloudResourceHistory", "auditEvent", "functions", "project"} {
		var docs []map[string]any
		if err := s.e.db.C(c).Find(s.e.ctx, pgdoc.M{}, &docs); err != nil {
			s.t.Fatal(err)
		}
		if d, _ := json.Marshal(docs); strings.Contains(string(d), s.mark) || strings.Contains(string(d), kubeletSaid) ||
			strings.Contains(string(d), base64.StdEncoding.EncodeToString([]byte(s.mark))) {
			s.t.Errorf("%s: %s carries a write-only value or the cluster's words: %s", stage, c, d)
		}
	}
	s.e.cluster.mu.Lock()
	deps, _ := json.Marshal(s.e.cluster.deps)
	bare := cloneObj(map[string]any{"secrets": s.e.cluster.secrets})
	s.e.cluster.mu.Unlock()
	if strings.Contains(string(deps), s.mark) || strings.Contains(string(deps), base64.StdEncoding.EncodeToString([]byte(s.mark))) {
		s.t.Errorf("%s: a Deployment carries a write-only value: %s", stage, deps)
	}
	for _, sec := range bare["secrets"].(map[string]any) { // a Secret holds values in its data only
		delete(sec.(map[string]any), "data")
	}
	if b, _ := json.Marshal(bare); strings.Contains(string(b), s.mark) {
		s.t.Errorf("%s: a Secret carries a write-only value outside its data: %s", stage, b)
	}
	if l := s.logs.String(); strings.Contains(l, s.mark) {
		s.t.Errorf("%s: a log line carries a write-only value: %s", stage, l)
	}
}

// audited waits for the audit events written in the background and returns them.
func (s *fnLeakScan) audited(atLeast int) []map[string]any {
	s.t.Helper()
	var events []map[string]any
	for range 50 {
		events = nil
		if err := s.e.db.C("auditEvent").Find(s.e.ctx, pgdoc.M{}, &events); err != nil {
			s.t.Fatal(err)
		}
		if len(events) >= atLeast {
			return events
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("%d audit events recorded, want at least %d: the audit check would prove nothing", len(events), atLeast)
	return nil
}

// Function secrets are write-only: every secret value in this test carries the scan's marker.
func TestFunctionSecrets(t *testing.T) {
	const mark = "s3ntinel"
	scan := newFnLeakScan(t, mark)
	e, call, leaks := scan.e, scan.call, scan.leaks
	base := "/project/p/functions"
	names := func(v any) string { return fmt.Sprint(v) }
	open := func(fn functionDoc, set map[string]fnSecret) map[string]string {
		t.Helper()
		values, err := e.h.openSecrets(&fn, set)
		if err != nil {
			t.Fatalf("the stored secrets do not open: %v", err)
		}
		return values
	}
	podTemplate := func(id string) (envFrom any, version any) {
		tpl := e.cluster.get("fn-p", id)["spec"].(map[string]any)["template"].(map[string]any)
		return tpl["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["envFrom"],
			tpl["metadata"].(map[string]any)["annotations"].(map[string]any)[function.AnnotationSecrets]
	}

	// Refusals: a name in both, a reserved name, a value that is not a string, a body that is no JSON. None repeats
	// a value: a body that does not decode is answered in fixed words.
	for _, body := range []string{
		`{"name":"api","image":"nginx:1","env":{"TOKEN":"x"},"secrets":{"TOKEN":"` + mark + `-a"}}`,
		`{"name":"api","image":"nginx:1","secrets":{"PORT":"` + mark + `-a"}}`,
		`{"name":"api","image":"nginx:1","secrets":{"NOT A NAME":"` + mark + `-a"}}`,
		`{"name":"api","image":"nginx:1","secrets":{"TOKEN":{"value":"` + mark + `-a"}}}`,
		`{"name":"api","image":"nginx:1","secrets":{"TOKEN":"` + mark + `-a"}`,
		`{"name":"api","image":"nginx:1","port":"` + mark + `-a"}`,
	} {
		if code, _ := call("owner-p", "POST", base, body); code != 400 {
			t.Errorf("create %s: %d, want 400", body, code)
		}
	}
	if code, _ := call("reader", "POST", base, `{"name":"api","image":"nginx:1","secrets":{"TOKEN":"`+mark+`-a"}}`); code != 403 {
		t.Errorf("create by a reader: %d, want 403", code)
	}
	// A body that does not decode is answered in the same fixed words whatever is wrong with it: the decoder's own
	// say where in the body it stopped and what it found there.
	for _, body := range []string{`{"name":"api","secrets":{"TOKEN":"` + mark + `-a"}`, `{"name":"api","port":"` + mark + `-a"}`, mark, `{"secrets":{"TOKEN":` + mark + `}}`} {
		if code, out := e.raw("owner-p", "POST", base, body); code != 400 || !strings.Contains(out, `"message":"invalid request body"`) {
			t.Errorf("create with the undecodable body %s: %d %s, want the fixed words", body, code, out)
		}
	}
	if len(e.records()) != 0 {
		t.Fatalf("a refused create left a record: %+v", e.records())
	}

	// Create with secrets. What the body says of secretNames/secretsVersion is not taken.
	code, data := call("owner-p", "POST", base, `{"name":"api","image":"nginx:1","env":{"MODE":"env-v4lue"},"secretNames":["FAKE"],"secretsVersion":"fake",
		"secrets":{"TOKEN":"`+mark+`-token-1","KEY":"`+mark+`-key","KEEP":"`+mark+`-keep","NOTHING":null}}`)
	if code != 200 || names(data["secretNames"]) != "[KEEP KEY TOKEN]" || data["secrets"] != nil || data["secretsVersion"] == "fake" || data["secretsVersion"] == nil ||
		len(data["secretsSetAt"].(map[string]any)) != 3 || data["env"].(map[string]any)["MODE"] != "env-v4lue" {
		t.Fatalf("create: %d %v", code, data)
	}
	id, v1 := data["id"].(string), data["secretsVersion"]
	sec, values := e.cluster.secret("fn-p", id)
	want := map[string]string{"TOKEN": mark + "-token-1", "KEY": mark + "-key", "KEEP": mark + "-keep"}
	owner := sec["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(values, want) || owner["uid"] != e.cluster.get("fn-p", id)["metadata"].(map[string]any)["uid"] || owner["kind"] != "Deployment" || owner["name"] != id {
		t.Fatalf("the function's Secret after create: %v", sec)
	}
	if envFrom, version := podTemplate(id); version != v1 || !reflect.DeepEqual(envFrom, []any{map[string]any{"secretRef": map[string]any{"name": id}}}) {
		t.Fatalf("pod template after create: envFrom %v, version %v (want %v)", envFrom, version, v1)
	}
	fn := e.fn(id)
	if len(fn.Secrets) != 3 || fn.Secrets["TOKEN"].KeyID != "k1" || fn.Secrets["TOKEN"].Sealed == "" || !reflect.DeepEqual(open(fn, fn.Secrets), want) || fn.PendingSecrets != nil {
		t.Fatalf("secrets at rest: %+v", fn.Secrets)
	}
	if deps := e.deployments(id); len(deps) != 1 || names(deps[0].SecretNames) != "[KEEP KEY TOKEN]" || deps[0].SecretsVersion != v1 {
		t.Fatalf("deployment record: %+v", deps)
	}
	if !e.settle(id, false) {
		t.Fatal("the rollout must settle")
	}
	first := e.fn(id).CurrentDeploymentID
	leaks("create")

	// A manager too sees the names and when each was set, nothing more.
	if code, data = call("owner-p", "GET", base+"/"+id, ""); code != 200 || names(data["secretNames"]) != "[KEEP KEY TOKEN]" || len(data["secretsSetAt"].(map[string]any)) != 3 {
		t.Fatalf("get: %d %v", code, data)
	}
	call("owner-p", "GET", base, "")

	// PATCH merges: TOKEN is replaced, KEY removed, NEW added, KEEP (not named) kept as it is. It is a rollout: the
	// new set waits beside the serving one until it settles.
	stale := e.fn(id)
	code, data = call("owner-p", "PATCH", base+"/"+id, `{"secrets":{"TOKEN":"`+mark+`-token-2","KEY":null,"NEW":"`+mark+`-new","ABSENT":null}}`)
	if code != 200 || data["status"] != fnDeploying || names(data["secretNames"]) != "[KEEP KEY TOKEN]" {
		t.Fatalf("patch secrets: %d %v", code, data)
	}
	want = map[string]string{"TOKEN": mark + "-token-2", "NEW": mark + "-new", "KEEP": mark + "-keep"}
	if _, values = e.cluster.secret("fn-p", id); !reflect.DeepEqual(values, want) {
		t.Fatalf("the Secret during the rollout: %v", values)
	}
	fn = e.fn(id)
	if len(fn.Secrets) != 3 || fn.Secrets["KEY"].Sealed == "" || len(fn.PendingSecrets) != 3 || fn.SecretsVersion != v1 {
		t.Fatalf("record during a secrets rollout: %+v / %+v", fn.Secrets, fn.PendingSecrets)
	}
	deps := e.deployments(id)
	v2 := deps[0].SecretsVersion
	if names(deps[0].SecretNames) != "[KEEP NEW TOKEN]" || v2 == "" || v2 == v1 {
		t.Fatalf("deployment record of a secrets rollout: %+v", deps[0])
	}
	if _, version := podTemplate(id); version != v2 {
		t.Fatalf("a secrets change must change the pod template: version %v, want %v", version, v2)
	}
	if !e.settle(id, false) {
		t.Fatal("the rollout must settle")
	}
	kept := fn.Secrets["KEEP"]
	if fn = e.fn(id); !reflect.DeepEqual(open(fn, fn.Secrets), want) || fn.PendingSecrets != nil || fn.SecretsVersion != v2 || names(fn.SecretNames) != "[KEEP NEW TOKEN]" ||
		fn.Secrets["KEEP"] != kept || !fn.Secrets["TOKEN"].SetAt.After(kept.SetAt) {
		t.Fatalf("record after a secrets rollout: %+v", fn)
	}
	// A rollout built on the secrets as they were before that one would undo it: refused.
	if err := e.h.startDeployment(e.ctx, e.fs(), &stale, stale.Snapshot, 0, nil, nil, nil); !errors.Is(err, errFunctionBusy) {
		t.Fatalf("a rollout built on outdated secrets: %v, want errFunctionBusy", err)
	}
	leaks("merge")

	// A failed rollout takes its secrets with it: the cluster and the record are back at the serving set.
	code, _ = call("owner-p", "PATCH", base+"/"+id, `{"image":"bad:1","secrets":{"TOKEN":"`+mark+`-token-bad","EXTRA":"`+mark+`-extra"}}`)
	if _, values = e.cluster.secret("fn-p", id); code != 200 || values["EXTRA"] != mark+"-extra" {
		t.Fatalf("patch to a bad image with secrets: %d, Secret %v", code, values)
	}
	if !e.settle(id, true) {
		t.Fatal("the final poll must settle")
	}
	fn = e.fn(id)
	_, values = e.cluster.secret("fn-p", id)
	if _, version := podTemplate(id); version != v2 || !reflect.DeepEqual(values, want) || !reflect.DeepEqual(open(fn, fn.Secrets), want) ||
		fn.PendingSecrets != nil || fn.SecretsVersion != v2 || !slices.Contains(function.Reasons, fn.LastError) {
		t.Fatalf("after a failed secrets rollout: version %v, Secret %v, record %+v", version, values, fn)
	}
	call("owner-p", "GET", base+"/"+id, "")
	if !strings.Contains(scan.logs.String(), kubeletSaid) {
		t.Error("what the cluster said of the failed pull is in no log line: the operator has nothing to go by")
	}
	leaks("failed rollout")

	// What does not change the secrets: an env-only deploy runs with the same set and version; an empty map or the
	// removal of a name that is not there deploys nothing.
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"env":{"A":"1"}}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("patch env: %d", code)
	}
	deps = e.deployments(id)
	envOnly := deps[0].ID
	if _, values = e.cluster.secret("fn-p", id); deps[0].SecretsVersion != v2 || !reflect.DeepEqual(values, want) || len(deps) != 4 {
		t.Fatalf("an env-only deploy: %+v, Secret %v", deps[0], values)
	}
	for _, body := range []string{`{"secrets":{}}`, `{"secrets":{"ABSENT":null}}`, `{"secrets":null}`, `{"secretNames":["FAKE","FAKE","FAKE"],"secretsVersion":"fake"}`} {
		code, data = call("owner-p", "PATCH", base+"/"+id, body)
		if code != 200 || data["status"] != fnReady || len(e.deployments(id)) != 4 || names(data["secretNames"]) != "[KEEP NEW TOKEN]" || data["secretsVersion"] != v2 {
			t.Fatalf("patch %s: %d %v (%d deployments)", body, code, data, len(e.deployments(id)))
		}
	}
	for _, body := range []string{`{"env":{"TOKEN":"x"}}`, `{"secrets":{"A":"` + mark + `-a"}}`, `{"secrets":{"PORT":"` + mark + `-a"}}`, `{"secrets":"` + mark + `"}`, `{"secrets":{"X":"` + mark + `"`} {
		if code, _ = call("owner-p", "PATCH", base+"/"+id, body); code != 400 {
			t.Errorf("patch %s: %d, want 400", body, code)
		}
	}
	if code, _ = call("reader", "PATCH", base+"/"+id, `{"secrets":{"TOKEN":"`+mark+`-reader"}}`); code != 403 {
		t.Errorf("patch by a reader: %d, want 403", code)
	}

	// Rollback: the first deployment's image and env, the secrets of now.
	if code, _ = call("owner-p", "POST", base+"/"+id+"/rollback", `{"deploymentId":"`+first+`"}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("rollback: %d", code)
	}
	fn = e.fn(id)
	_, values = e.cluster.secret("fn-p", id)
	if fn.Env["MODE"] != "env-v4lue" || fn.Env["A"] != "" || names(fn.SecretNames) != "[KEEP NEW TOKEN]" || fn.SecretsVersion != v2 || !reflect.DeepEqual(values, want) {
		t.Fatalf("after rollback: record %+v, Secret %v", fn, values)
	}
	if _, data = call("owner-p", "GET", base+"/"+id+"/deployments", ""); len(scan.responses[len(scan.responses)-1]) == 0 || data != nil {
		t.Fatalf("deployments: %v", data) // a list, not an object
	}
	// A deployment whose env has a name that is a secret now cannot be rolled back to.
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"secrets":{"A":"`+mark+`-a"}}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("patch a secret named like an old env var: %d", code)
	}
	if code, _ = call("owner-p", "POST", base+"/"+id+"/rollback", `{"deploymentId":"`+envOnly+`"}`); code != 400 {
		t.Errorf("rollback to a deployment whose env clashes with a secret: %d, want 400", code)
	}

	// Suspend, resume, logs: nothing of these shows a value either.
	for _, action := range []string{"suspend", "resume"} {
		if code, _ = call("owner-p", "POST", base+"/"+id+"/action", `{"action":"`+action+`"}`); code != 200 {
			t.Fatalf("%s: %d", action, code)
		}
	}
	call("owner-p", "GET", base+"/"+id+"/logs", "")
	leaks("rollback and actions")

	// The last secret removed: the Secret is emptied, the pod template is the one of a function that never had
	// secrets, and the record says nothing of secrets any more.
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"secrets":{"TOKEN":null,"NEW":null,"KEEP":null,"A":null}}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("remove every secret: %d", code)
	}
	sec, values = e.cluster.secret("fn-p", id)
	envFrom, version := podTemplate(id)
	fn = e.fn(id)
	if sec == nil || len(values) != 0 || envFrom != nil || version != nil || fn.Secrets != nil || fn.SecretNames != nil || fn.SecretsVersion != "" {
		t.Fatalf("after removing every secret: Secret %v, envFrom %v, version %v, record %+v", sec, envFrom, version, fn)
	}
	if code, data = call("owner-p", "GET", base+"/"+id, ""); code != 200 || data["secretNames"] != nil || data["secretsSetAt"] != nil || data["secretsVersion"] != nil {
		t.Fatalf("get after removing every secret: %d %v", code, data)
	}
	// The function is deployable as before (the next rollout's claim finds no version).
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"secrets":{"TOKEN":"`+mark+`-token-3"}}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("a secret again: %d", code)
	}
	if _, values = e.cluster.secret("fn-p", id); values["TOKEN"] != mark+"-token-3" {
		t.Fatalf("the Secret after a secret is set again: %v", values)
	}

	// Delete: the Secret goes with the Deployment that owns it.
	if code, _ = call("owner-p", "DELETE", base+"/"+id, ""); code != 202 {
		t.Fatalf("delete: %d", code)
	}
	if sec, _ = e.cluster.secret("fn-p", id); sec != nil || e.cluster.replicas("fn-p", id) != -1 {
		t.Fatalf("after delete: Secret %v", sec)
	}

	// Teardown: a project's namespace takes every Secret with it.
	id = e.create(`{"name":"two","image":"nginx:1","secrets":{"TOKEN":"` + mark + `-two"}}`)
	if err := e.h.DeleteProjectFunctions(e.ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if sec, _ = e.cluster.secret("fn-p", id); sec != nil || e.cluster.secretCount() != 0 {
		t.Fatalf("after teardown: %v", e.cluster.secrets)
	}

	// The audit trail names the function, its image, its deployment and its secrets BY NAME.
	named := false
	for _, ev := range scan.audited(10) {
		meta, _ := ev["resourceMetadata"].(map[string]any)
		if ev["resourceType"] != cloud.TypeFunction {
			t.Errorf("an audit event of another resource type: %v", ev)
		}
		named = named || (meta["name"] == "api" && meta["image"] == "nginx:1" && fmt.Sprint(meta["secretNames"]) == "[KEEP NEW TOKEN]" && meta["verb"] == "DEPLOY" && meta["deploymentSeq"] != nil)
	}
	// Nor does the trail, a cloud row or a log line hold what else a request carried: the value of an env variable.
	for _, c := range []string{"auditEvent", "cloudResource", "cloudResourceHistory"} {
		var docs []map[string]any
		if err := e.db.C(c).Find(e.ctx, pgdoc.M{}, &docs); err != nil {
			t.Fatal(err)
		}
		if d, _ := json.Marshal(docs); strings.Contains(string(d), "env-v4lue") {
			t.Errorf("%s carries the value of an env variable: %s", c, d)
		}
	}
	if strings.Contains(scan.logs.String(), "env-v4lue") {
		t.Error("a log line carries the value of an env variable")
	}
	if !named {
		t.Error("no audit event of a deployment carries the function's name, image, deployment and secret names")
	}
	if len(scan.responses) < 30 {
		t.Fatalf("%d responses collected", len(scan.responses))
	}
	leaks("the end")
}

// A new function's Secret is written before the Deployment that will own it. When that apply fails, the values must
// not stay on the cluster with nothing left that would ever empty them.
func TestFunctionSecretsAfterFailedApply(t *testing.T) {
	e := newFnEnv(t)
	down := &kamajik8s.APIError{Status: http.StatusServiceUnavailable}
	body := `{"name":"api","image":"nginx:1","secrets":{"TOKEN":"hunter2"}}`
	secretOf := func(id string) map[string]string {
		_, values := e.cluster.secret("fn-p", id)
		return values
	}

	// The cleanup works: nothing of the function is left (no record, no deployment record, no live row), and its
	// Secret is empty.
	e.cluster.fail = map[string]error{"ApplyCR": down}
	if code, _ := e.call("owner-p", "POST", "/project/p/functions", body); code != 503 || len(e.records()) != 0 || e.cluster.secretCount() != 1 {
		t.Fatalf("failed create: %d, %d records, Secrets %v", code, len(e.records()), e.cluster.secrets)
	}
	for k := range e.cluster.secrets {
		if data := e.cluster.secrets[k]["data"]; data != nil {
			t.Fatalf("a failed create left secret values behind: %v", data)
		}
	}
	var deps, rows int64
	deps, _ = e.h.fnDeps.Count(e.ctx, pgdoc.M{})
	rows, _ = e.db.C("cloudResource").Count(e.ctx, pgdoc.M{"type": cloud.TypeFunction})
	if deps != 0 || rows != 0 {
		t.Fatalf("a failed create left %d deployment records and %d live rows", deps, rows)
	}
	// Its row is archived without a running interval: a function that never came to be is never billed.
	if gone := e.archived(""); len(gone) != 1 || gone[0].Data["status"] != functionRowDeleted || gone[0].Data["runningSince"] != nil || gone[0].Data["stoppedAt"] != nil ||
		gone[0].Data["endedIntervals"] != nil || intAny(gone[0].Data["instances"]) != 0 {
		t.Fatalf("the archived row of a create that could not be started: %+v", gone)
	}

	// A create whose instance was recorded as started before the cluster refused the start: the interval that
	// was opened is not archived either.
	e.cluster.fail = map[string]error{"SetScale": down}
	if code, _ := e.call("owner-p", "POST", "/project/p/functions", `{"name":"scale","image":"nginx:1"}`); code != 503 || len(e.records()) != 0 || len(e.cluster.deps) != 0 {
		t.Fatalf("a create whose start fails: %d, %d records, %d Deployments", code, len(e.records()), len(e.cluster.deps))
	}
	for _, gone := range e.archived("") {
		if gone.Data["runningSince"] != nil || gone.Data["stoppedAt"] != nil || gone.Data["endedIntervals"] != nil || gone.Data["status"] != functionRowDeleted {
			t.Fatalf("the archived row of a create whose start failed carries an interval: %v", gone.Data)
		}
	}
	if len(e.archived("")) != 2 {
		t.Fatalf("%d archived rows after two failed creates", len(e.archived("")))
	}

	// The cleanup fails too: the function keeps its record, FAILED, so that its delete empties the Secret later.
	e.cluster.fail = map[string]error{"ApplyCR": down, "MergePatchCore": down}
	code, _ := e.call("owner-p", "POST", "/project/p/functions", body)
	fns := e.records()
	if code == 200 || len(fns) != 1 {
		t.Fatalf("failed create with a failed cleanup: %d, %d records; the function must be kept", code, len(fns))
	}
	fn := fns[0]
	if _, data := e.call("owner-p", "GET", "/project/p/functions/"+fn.ID, ""); secretOf(fn.ID)["TOKEN"] != "hunter2" || data["status"] != fnFailed {
		t.Fatalf("kept function: status %v, Secret %v", data["status"], secretOf(fn.ID))
	}
	e.cluster.fail = nil
	if code, _ := e.call("owner-p", "DELETE", "/project/p/functions/"+fn.ID, ""); code != 202 || len(secretOf(fn.ID)) != 0 || len(e.records()) != 0 {
		t.Fatalf("delete of the kept function: %d, Secret %v", code, secretOf(fn.ID))
	}

	// A deploy that raced the function's delete and failed half way: no record is left to clean up after it, so the
	// apply does.
	e.cluster.fail = map[string]error{"ApplyCR": down}
	if err := e.h.applyFunction(e.ctx, e.fs(), &fn, fn.Snapshot); !errors.Is(err, function.ErrNotFound) || len(secretOf(fn.ID)) != 0 {
		t.Fatalf("failed apply of a deleted function: %v, Secret %v", err, secretOf(fn.ID))
	}
}

// The credentials of a private registry are write-only like secrets: every password in this test carries the scan's
// marker. It may be in the function's pull secret and nowhere else on the cluster; a rollout that cannot pull with
// new credentials goes back to the ones that served, and says why in fixed words.
func TestFunctionRegistryAuth(t *testing.T) {
	const mark = "s3ntinel"
	scan := newFnLeakScan(t, mark)
	e, call, leaks := scan.e, scan.call, scan.leaks
	e.cluster.registry = map[string]string{"private.example": mark + "-right"}
	base := "/project/p/functions"
	right := `"password":"` + mark + `-right"`
	lastResponse := func() string { return scan.responses[len(scan.responses)-1] }
	password := func(fn functionDoc) string {
		t.Helper()
		v, err := e.h.openValue(&fn, fnRegistryPasswordName, *fn.RegistryPassword)
		if err != nil {
			t.Fatalf("the stored registry password does not open: %v", err)
		}
		return v
	}
	// auths is the docker config of a function's pull secret (nil without one), by server.
	auths := func(id string) map[string]any {
		t.Helper()
		sec, values := e.cluster.secret("fn-p", id+"-registry")
		if sec == nil {
			return nil
		}
		var cfg struct {
			Auths map[string]any `json:"auths"`
		}
		if err := json.Unmarshal([]byte(values[".dockerconfigjson"]), &cfg); err != nil || sec["type"] != "kubernetes.io/dockerconfigjson" || len(values) != 1 || cfg.Auths == nil {
			t.Fatalf("pull secret of %s is not a docker config: %v", id, sec)
		}
		return cfg.Auths
	}
	// holders are the Secrets whose data has a password: only ever a pull secret.
	holders := func() []string {
		e.cluster.mu.Lock()
		defer e.cluster.mu.Unlock()
		var out []string
		for k, sec := range e.cluster.secrets {
			data, _ := sec["data"].(map[string]any)
			for _, v := range data {
				if b, _ := base64.StdEncoding.DecodeString(v.(string)); strings.Contains(string(b), mark) {
					out = append(out, k)
				}
			}
		}
		return out
	}
	podSpec := func(id string) (pull any, policy any, version any) {
		tpl := e.cluster.get("fn-p", id)["spec"].(map[string]any)["template"].(map[string]any)
		spec := tpl["spec"].(map[string]any)
		return spec["imagePullSecrets"], spec["containers"].([]any)[0].(map[string]any)["imagePullPolicy"],
			tpl["metadata"].(map[string]any)["annotations"].(map[string]any)[function.AnnotationRegistryAuth]
	}
	settle := func(what string, id string, final bool) {
		t.Helper()
		if !e.settle(id, final) {
			t.Fatalf("%s: the rollout must settle", what)
		}
	}

	// Refusals. None repeats the password.
	for _, body := range []string{
		`{"name":"api","image":"private.example/acme/api:1","registryAuth":{"username":"bot"}}`,
		`{"name":"api","image":"private.example/acme/api:1","registryAuth":{` + right + `}}`,
		`{"name":"api","image":"private.example/acme/api:1","registryAuth":{"server":"ghcr.io","username":"bot",` + right + `}}`,
		`{"name":"api","image":"private.example/acme/api:1","registryAuth":{"server":"private.example; x","username":"bot",` + right + `}}`,
		`{"name":"api","image":"private.example/acme/api:1","registryAuth":"` + mark + `"}`,
		`{"name":"api","image":"10.0.0.7/acme/api:1","registryAuth":{"username":"bot",` + right + `}}`,
	} {
		if code, _ := call("owner-p", "POST", base, body); code != 400 {
			t.Errorf("create %s: %d, want 400", body, code)
		}
	}
	if code, _ := call("reader", "POST", base, `{"name":"api","image":"private.example/acme/api:1","registryAuth":{"username":"bot",`+right+`}}`); code != 403 {
		t.Errorf("create by a reader: %d, want 403", code)
	}
	// A create that fails on the cluster leaves no password behind.
	e.cluster.fail = map[string]error{"ApplyCR": &kamajik8s.APIError{Status: http.StatusServiceUnavailable}}
	if code, _ := call("owner-p", "POST", base, `{"name":"api","image":"private.example/acme/api:1","registryAuth":{"username":"bot",`+right+`}}`); code == 200 || len(holders()) != 0 || e.cluster.secretCount() != 1 || len(e.records()) != 0 {
		t.Fatalf("failed create: %d, passwords left in %v", code, holders())
	}
	e.cluster.fail = nil

	// Create, next to a secret of the function: two Secrets, each with its own.
	code, data := call("owner-p", "POST", base, `{"name":"api","image":"private.example/acme/api:1","secrets":{"TOKEN":"tok"},
		"registryAuth":{"username":"bot","version":"fake",`+right+`}}`)
	shown, _ := data["registryAuth"].(map[string]any)
	if code != 200 || len(shown) != 3 || shown["server"] != "private.example" || shown["username"] != "bot" || shown["setAt"] == nil {
		t.Fatalf("create: %d %v", code, data)
	}
	id := data["id"].(string)
	fn := e.fn(id)
	v1 := fn.RegistryAuth.Version
	if v1 == "" || v1 == "fake" || fn.RegistryPassword == nil || fn.RegistryPassword.KeyID != "k1" || password(fn) != mark+"-right" || fn.PendingRegistryPassword != nil {
		t.Fatalf("credentials at rest: %+v / %+v", fn.RegistryAuth, fn.RegistryPassword)
	}
	sec, _ := e.cluster.secret("fn-p", id+"-registry")
	owner := sec["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)
	wantAuth := map[string]any{"private.example": map[string]any{"username": "bot", "password": mark + "-right"}}
	if !reflect.DeepEqual(auths(id), wantAuth) || owner["uid"] != e.cluster.get("fn-p", id)["metadata"].(map[string]any)["uid"] || owner["kind"] != "Deployment" {
		t.Fatalf("pull secret after create: %v", sec)
	}
	if _, values := e.cluster.secret("fn-p", id); !reflect.DeepEqual(values, map[string]string{"TOKEN": "tok"}) || !reflect.DeepEqual(holders(), []string{"fn-p/" + id + "-registry"}) {
		t.Fatalf("the function's Secret %v, passwords in %v", values, holders())
	}
	if pull, policy, version := podSpec(id); !reflect.DeepEqual(pull, []any{map[string]any{"name": id + "-registry"}}) || policy != "Always" || version != v1 {
		t.Fatalf("pod template after create: pull %v, policy %v, version %v (want %v)", pull, policy, version, v1)
	}
	if deps := e.deployments(id); len(deps) != 1 || !reflect.DeepEqual(deps[0].RegistryAuth, &function.RegistryAuth{Server: "private.example", Username: "bot", Version: v1}) {
		t.Fatalf("deployment record: %+v", deps)
	}
	settle("create", id, false)
	first := e.fn(id).CurrentDeploymentID
	leaks("create")

	// The node has the image now. A function that names it without credentials (it could be another project's) does
	// not get it from there: its pod asks the registry too, by tag and by the digest the first one ran.
	for name, image := range map[string]string{"leech": "private.example/acme/api:1", "leech-digest": "private.example/acme/api@" + e.deployments(id)[0].ImageDigest} {
		leech := e.create(`{"name":"` + name + `","image":"` + image + `"}`)
		settle("no credentials for a cached image", leech, true)
		if fn = e.fn(leech); fn.CurrentDeploymentID != "" || fn.LastError != function.ReasonPull {
			t.Fatalf("a private image without credentials (%s), from the node's cache: %+v", image, fn)
		}
		if code, _ = call("owner-p", "DELETE", base+"/"+leech, ""); code != 202 {
			t.Fatalf("delete: %d", code)
		}
	}
	if !e.cluster.cached["private.example/acme/api:1"] {
		t.Fatal("the node has not cached the image: the two above proved nothing")
	}

	// What is shown is where and who, never the password; a deployment adds the version.
	_, data = call("owner-p", "GET", base+"/"+id, "")
	if shown, _ = data["registryAuth"].(map[string]any); data["status"] != fnReady || len(shown) != 3 || shown["username"] != "bot" {
		t.Fatalf("get: %v", data)
	}
	call("owner-p", "GET", base, "")
	call("owner-p", "GET", base+"/"+id+"/deployments", "")
	if !strings.Contains(lastResponse(), `"registryAuth":{"server":"private.example","username":"bot","version":"`+v1+`"}`) {
		t.Fatalf("deployments: %s", lastResponse())
	}

	// A change of something else rolls out with the credentials as they are.
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"env":{"A":"1"}}`); code != 200 {
		t.Fatalf("patch env: %d", code)
	}
	settle("patch env", id, false)
	if fn = e.fn(id); fn.RegistryAuth.Version != v1 || fn.Env["A"] != "1" || !reflect.DeepEqual(auths(id), wantAuth) {
		t.Fatalf("after an env-only deploy: %+v, pull secret %v", fn.RegistryAuth, auths(id))
	}

	// A wrong password: the rollout cannot pull and says so in fixed words; the cluster and the record go back to
	// what served.
	set := fn.RegistryPassword.SetAt
	code, data = call("owner-p", "PATCH", base+"/"+id, `{"registryAuth":{"username":"bot2","password":"`+mark+`-wrong"}}`)
	shown, _ = data["registryAuth"].(map[string]any)
	if code != 200 || data["status"] != fnDeploying || shown["username"] != "bot" || data["reason"] != function.ReasonPullPrivate {
		t.Fatalf("patch to a wrong password: %d %v", code, data)
	}
	fn = e.fn(id)
	if fn.PendingRegistryPassword == nil || password(fn) != mark+"-right" || fn.RegistryAuth.Version != v1 ||
		!reflect.DeepEqual(auths(id), map[string]any{"private.example": map[string]any{"username": "bot2", "password": mark + "-wrong"}}) {
		t.Fatalf("during a credentials rollout: %+v, pull secret %v", fn, auths(id))
	}
	settle("wrong password", id, true)
	fn = e.fn(id)
	if _, _, version := podSpec(id); version != v1 || !reflect.DeepEqual(auths(id), wantAuth) || fn.PendingRegistryPassword != nil || fn.RegistryAuth.Username != "bot" ||
		!fn.RegistryPassword.SetAt.Equal(set) || fn.LastError != function.ReasonPullPrivate || e.cluster.get("fn-p", id)["status"].(map[string]any)["readyReplicas"] != float64(1) {
		t.Fatalf("after a failed credentials rollout: %+v, pull secret %v", fn, auths(id))
	}
	if deps := e.deployments(id); deps[0].State != depFailed || deps[0].RegistryAuth.Username != "bot2" || deps[0].RegistryAuth.Version == v1 || deps[0].Message != function.ReasonPullPrivate {
		t.Fatalf("the failed deployment: %+v", deps[0])
	}
	call("owner-p", "GET", base+"/"+id, "")
	leaks("wrong password")

	// Replacing takes the password again; with it, the new credentials are adopted.
	stale := e.fn(id)
	for _, body := range []string{`{"registryAuth":{"username":"bot2"}}`, `{"registryAuth":{}}`, `{"registryAuth":"` + mark + `"}`, `{"registryAuth":{"server":"ghcr.io","username":"bot",` + right + `}}`} {
		if code, _ = call("owner-p", "PATCH", base+"/"+id, body); code != 400 {
			t.Errorf("patch %s: %d, want 400", body, code)
		}
	}
	if code, _ = call("reader", "PATCH", base+"/"+id, `{"registryAuth":null}`); code != 403 {
		t.Errorf("patch by a reader: %d, want 403", code)
	}
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"registryAuth":{"username":"bot2",`+right+`}}`); code != 200 {
		t.Fatalf("replace the credentials: %d", code)
	}
	settle("replace", id, false)
	fn = e.fn(id)
	v2 := fn.RegistryAuth.Version
	wantAuth = map[string]any{"private.example": map[string]any{"username": "bot2", "password": mark + "-right"}}
	if v2 == v1 || fn.RegistryAuth.Username != "bot2" || fn.PendingRegistryPassword != nil || !fn.RegistryPassword.SetAt.After(set) || !reflect.DeepEqual(auths(id), wantAuth) {
		t.Fatalf("after replacing the credentials: %+v, pull secret %v", fn, auths(id))
	}
	// A rollout built on the credentials as they were before would undo that: refused.
	if err := e.h.startDeployment(e.ctx, e.fs(), &stale, stale.Snapshot, 0, nil, nil, nil); !errors.Is(err, errFunctionBusy) {
		t.Fatalf("a rollout built on outdated credentials: %v, want errFunctionBusy", err)
	}

	// An image in another registry is refused while the credentials are for this one; with null they go: the pull
	// secret is emptied and the pod template is that of a public function.
	n := len(e.deployments(id))
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"image":"nginx:2"}`); code != 400 || len(e.deployments(id)) != n ||
		!strings.Contains(lastResponse(), "the registry credentials are for private.example but the image is pulled from docker.io: send registryAuth for that registry, or null to remove the credentials") {
		t.Fatalf("an image in another registry: %d %s", code, lastResponse())
	}
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"image":"nginx:2","registryAuth":null}`); code != 200 {
		t.Fatalf("another registry without credentials: %d", code)
	}
	settle("remove", id, false)
	public := e.fn(id).CurrentDeploymentID
	fn = e.fn(id)
	pull, policy, version := podSpec(id)
	if _, values := e.cluster.secret("fn-p", id); fn.RegistryAuth != nil || fn.RegistryPassword != nil || pull != nil || policy != "Always" || version != nil ||
		len(auths(id)) != 0 || len(holders()) != 0 || values["TOKEN"] != "tok" {
		t.Fatalf("after removing the credentials: %+v, pull %v, pull secret %v, Secret %v", fn, pull, auths(id), values)
	}
	if _, data = call("owner-p", "GET", base+"/"+id, ""); data["registryAuth"] != nil || strings.Contains(lastResponse(), "registryAuth") {
		t.Fatalf("get after removing the credentials: %s", lastResponse())
	}
	if code, data = call("owner-p", "PATCH", base+"/"+id, `{"registryAuth":null}`); code != 200 || data["status"] != fnReady || len(e.deployments(id)) != n+1 {
		t.Fatalf("removing credentials that are not there must deploy nothing: %d %v", code, data)
	}

	// Back in the private registry, the server given as a URL. Removing the credentials there cannot work, although
	// the node has the image: that rollout fails, and they are back.
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"image":"private.example/acme/api:2","registryAuth":{"server":"https://private.example","username":"bot",`+right+`}}`); code != 200 {
		t.Fatalf("credentials with a server: %d", code)
	}
	settle("server", id, false)
	wantAuth = map[string]any{"https://private.example": map[string]any{"username": "bot", "password": mark + "-right"}}
	if fn = e.fn(id); fn.Image != "private.example/acme/api:2" || !reflect.DeepEqual(auths(id), wantAuth) {
		t.Fatalf("after credentials with a server: %+v, pull secret %v", fn, auths(id))
	}
	v3 := fn.RegistryAuth.Version
	if code, _ = call("owner-p", "PATCH", base+"/"+id, `{"registryAuth":null}`); code != 200 || len(holders()) != 0 {
		t.Fatalf("remove the credentials of a private image: %d, passwords in %v", code, holders())
	}
	settle("remove from a private image", id, true)
	if fn = e.fn(id); fn.RegistryAuth == nil || fn.RegistryAuth.Version != v3 || !reflect.DeepEqual(auths(id), wantAuth) || fn.LastError != function.ReasonPull {
		t.Fatalf("after a failed removal: %+v, pull secret %v", fn, auths(id))
	}

	// Rollback: the old deployment's image (the digest it ran) with the credentials of now; not to an image in
	// another registry.
	if code, _ = call("owner-p", "POST", base+"/"+id+"/rollback", `{"deploymentId":"`+first+`"}`); code != 200 {
		t.Fatalf("rollback: %d", code)
	}
	settle("rollback", id, false)
	if fn = e.fn(id); !strings.HasPrefix(fn.Image, "private.example/acme/api@sha256:") || fn.RegistryAuth.Version != v3 || e.deployments(id)[0].RegistryAuth.Version != v3 || !reflect.DeepEqual(auths(id), wantAuth) {
		t.Fatalf("after rollback: %+v, pull secret %v", fn, auths(id))
	}
	if code, _ = call("owner-p", "POST", base+"/"+id+"/rollback", `{"deploymentId":"`+public+`"}`); code != 400 || !strings.Contains(lastResponse(), "or null to remove the credentials") {
		t.Errorf("rollback to an image in another registry: %d %s", code, lastResponse())
	}
	// What that message asks for can be sent with the rollback: null, or other credentials. A registryAuth that
	// cannot be read is not null, and credentials for the wrong registry stay refused.
	n = len(e.deployments(id))
	for _, auth := range []string{`"` + mark + `"`, `{"username":"bot"}`, `{"server":"private.example","username":"bot",` + right + `}`} {
		if code, _ = call("owner-p", "POST", base+"/"+id+"/rollback", `{"deploymentId":"`+public+`","registryAuth":`+auth+`}`); code != 400 || len(e.deployments(id)) != n {
			t.Errorf("rollback with registryAuth %s: %d, want 400", auth, code)
		}
	}
	if code, _ = call("owner-p", "POST", base+"/"+id+"/rollback", `{"deploymentId":"`+public+`","registryAuth":null}`); code != 200 {
		t.Fatalf("rollback to another registry without credentials: %d %s", code, lastResponse())
	}
	settle("rollback without credentials", id, false)
	if fn = e.fn(id); !strings.HasPrefix(fn.Image, "nginx@sha256:") || fn.RegistryAuth != nil || fn.RegistryPassword != nil || len(auths(id)) != 0 || len(holders()) != 0 {
		t.Fatalf("after a rollback without credentials: %+v, pull secret %v", fn, auths(id))
	}
	if code, _ = call("owner-p", "POST", base+"/"+id+"/rollback", `{"deploymentId":"`+first+`","registryAuth":{"username":"bot3",`+right+`}}`); code != 200 {
		t.Fatalf("rollback with new credentials: %d %s", code, lastResponse())
	}
	settle("rollback with credentials", id, false)
	wantAuth = map[string]any{"private.example": map[string]any{"username": "bot3", "password": mark + "-right"}}
	if fn = e.fn(id); !strings.HasPrefix(fn.Image, "private.example/acme/api@sha256:") || fn.RegistryAuth == nil || fn.RegistryAuth.Version == v3 || fn.PendingRegistryPassword != nil ||
		password(fn) != mark+"-right" || !reflect.DeepEqual(auths(id), wantAuth) {
		t.Fatalf("after a rollback with new credentials: %+v, pull secret %v", fn, auths(id))
	}
	for _, action := range []string{"suspend", "resume"} {
		if code, _ = call("owner-p", "POST", base+"/"+id+"/action", `{"action":"`+action+`"}`); code != 200 {
			t.Fatalf("%s: %d", action, code)
		}
	}
	call("owner-p", "GET", base+"/"+id+"/logs", "")
	leaks("rollback and actions")

	// Docker Hub goes under the server the kubelet looks it up by.
	code, data = call("owner-p", "POST", base, `{"name":"hub","image":"acme/private:1","registryAuth":{"username":"bot","password":"`+mark+`-hub"}}`)
	hub, _ := data["id"].(string)
	if shown, _ = data["registryAuth"].(map[string]any); code != 200 || shown["server"] != function.DockerHubServer ||
		!reflect.DeepEqual(auths(hub), map[string]any{function.DockerHubServer: map[string]any{"username": "bot", "password": mark + "-hub"}}) {
		t.Fatalf("a Docker Hub image: %d %v, pull secret %v", code, data, auths(hub))
	}
	settle("hub", hub, false)

	// Delete: the pull secret goes with the Deployment that owns it; teardown takes the rest.
	if code, _ = call("owner-p", "DELETE", base+"/"+id, ""); code != 202 || auths(id) != nil || !reflect.DeepEqual(holders(), []string{"fn-p/" + hub + "-registry"}) {
		t.Fatalf("delete: %d, pull secret %v, passwords in %v", code, auths(id), holders())
	}
	if err := e.h.DeleteProjectFunctions(e.ctx, "p"); err != nil || e.cluster.secretCount() != 0 {
		t.Fatalf("teardown: %v, Secrets %v", err, e.cluster.secrets)
	}
	if len(scan.audited(10)) < 10 || len(scan.responses) < 30 {
		t.Fatalf("%d responses: the scan would prove nothing", len(scan.responses))
	}
	leaks("the end")
}

// The sealed row. Without the portal's encryption key a write of a secret or of a registry password answers 503 on
// every way in and stores nothing; with it, no route returns a value, and a blob moved to another function's record
// does not open there: that function is not deployed with it, and nothing of it reaches the cluster.
func TestFunctionSealedRow(t *testing.T) {
	const mark = "s3ntinel"
	scan := newFnLeakScan(t, mark)
	e, call := scan.e, scan.call
	base := "/project/p/functions"
	lastResponse := func() string { return scan.responses[len(scan.responses)-1] }
	stored := func() string {
		t.Helper()
		var out []byte
		for _, c := range []string{"functions", "function_deployments", "function_hosts", "cloudResource", "cloudResourceHistory"} {
			var docs []map[string]any
			if err := e.db.C(c).Find(e.ctx, pgdoc.M{}, &docs); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(docs)
			out = append(out, b...)
		}
		return string(out)
	}

	// No key: nothing that carries a value is taken, and nothing is stored or sent to the cluster.
	keys := e.h.fnKeys
	e.h.fnKeys = nil
	plain := e.ready(`{"name":"plain","image":"nginx:1"}`) // a function without secrets needs no key
	before, applies := stored(), e.cluster.calls["ApplyCR"]
	for _, c := range [][3]string{
		{"POST", base, `{"name":"nokey","image":"nginx:1","secrets":{"TOKEN":"` + mark + `-a"}}`},
		{"POST", base, `{"name":"nokey","image":"ghcr.io/acme/api:1","registryAuth":{"username":"bot","password":"` + mark + `-b"}}`},
		{"PATCH", base + "/" + plain, `{"secrets":{"TOKEN":"` + mark + `-c"}}`},
		{"PATCH", base + "/" + plain, `{"image":"ghcr.io/acme/api:1","registryAuth":{"username":"bot","password":"` + mark + `-d"}}`},
		{"POST", base + "/" + plain + "/rollback", `{"deploymentId":"` + e.fn(plain).CurrentDeploymentID + `","registryAuth":{"username":"bot","password":"` + mark + `-e"}}`},
	} {
		if code, _ := call("owner-p", c[0], c[1], c[2]); (code != 503 && !(c[0] == "POST" && strings.HasSuffix(c[1], "/rollback") && code == 400)) || (code == 503 && !strings.Contains(lastResponse(), "no encryption key")) {
			t.Errorf("%s %s without an encryption key: %d %s, want 503", c[0], c[1], code, lastResponse())
		}
	}
	// The rollback above names the serving deployment, which is refused before the key is looked at; one to a
	// superseded deployment reaches the key.
	if code, _ := call("owner-p", "PATCH", base+"/"+plain, `{"env":{"A":"1"}}`); code != 200 || !e.settle(plain, false) {
		t.Fatalf("an update without secrets needs no key: %d", code)
	}
	superseded := e.deployments(plain)[1].ID
	before, applies = stored(), e.cluster.calls["ApplyCR"]
	if code, _ := call("owner-p", "POST", base+"/"+plain+"/rollback", `{"deploymentId":"`+superseded+`","registryAuth":{"username":"bot","password":"`+mark+`-f"}}`); code != 503 || !strings.Contains(lastResponse(), "no encryption key") {
		t.Errorf("a rollback with credentials and no key: %d %s, want 503", code, lastResponse())
	}
	if after := stored(); after != before || e.cluster.calls["ApplyCR"] != applies || e.cluster.secretCount() != 0 || len(e.records()) != 1 || e.fn(plain).DeployingID != "" {
		t.Fatalf("a write refused for want of a key stored or deployed something:\nbefore %s\nafter  %s", before, after)
	}
	e.h.fnKeys = keys

	// Two functions with a secret and a registry password each. No route returns a value.
	e.cluster.registry = map[string]string{"private.example": mark + "-pull"}
	a := e.ready(`{"name":"a","image":"private.example/acme/a:1","secrets":{"TOKEN":"` + mark + `-of-a"},"registryAuth":{"username":"bot","password":"` + mark + `-pull"}}`)
	b := e.ready(`{"name":"b","image":"private.example/acme/b:1","secrets":{"TOKEN":"` + mark + `-of-b"},"registryAuth":{"username":"bot","password":"` + mark + `-pull"}}`)
	for _, path := range []string{base, base + "/" + a, base + "/" + a + "/deployments", base + "/" + a + "/logs", "/project/p/quota-usage?serviceId=svc"} {
		if code, _ := call("owner-p", "GET", path, ""); code != 200 {
			t.Errorf("GET %s: %d", path, code)
		}
	}
	for _, r := range scan.responses {
		for _, no := range []string{mark, e.fn(a).Secrets["TOKEN"].Sealed, e.fn(a).RegistryPassword.Sealed, `"sealed"`, `"keyId"`, `"pendingSecrets"`, `"registryPassword"`} {
			if strings.Contains(r, no) {
				t.Errorf("a response carries %q: %s", no, r)
			}
		}
	}

	// The blob of a's secret, moved into b's record, does not open there.
	_, valuesBefore := e.cluster.secret("fn-p", b)
	move := func(set pgdoc.M) {
		t.Helper()
		if _, err := e.h.fns.SetByID(e.ctx, b, set, nil); err != nil {
			t.Fatal(err)
		}
	}
	own := e.fn(b)
	move(pgdoc.M{"secrets": e.fn(a).Secrets})
	applies = e.cluster.calls["ApplyCR"]
	if code, _ := call("owner-p", "PATCH", base+"/"+b, `{"env":{"A":"1"}}`); code != 503 || !strings.Contains(lastResponse(), "cannot be opened") || strings.Contains(lastResponse(), mark) {
		t.Fatalf("a deployment of a function that holds another function's blob: %d %s, want 503", code, lastResponse())
	}
	_, valuesAfter := e.cluster.secret("fn-p", b)
	if got := e.fn(b); got.DeployingID != "" || len(e.deployments(b)) != 1 || e.cluster.calls["ApplyCR"] != applies || !reflect.DeepEqual(valuesAfter, valuesBefore) || valuesAfter["TOKEN"] != mark+"-of-b" {
		t.Fatalf("a blob that does not open was deployed: record %+v, Secret %v", got, valuesAfter)
	}
	// A merge needs the values it keeps: it is refused the same way and stores nothing.
	if code, _ := call("owner-p", "PATCH", base+"/"+b, `{"secrets":{"OTHER":"`+mark+`-x"}}`); code != 503 || len(e.fn(b).Secrets) != 1 {
		t.Fatalf("a secrets merge over a blob that does not open: %d %s", code, lastResponse())
	}
	// The same for the registry password of a, in the place of b's.
	move(pgdoc.M{"secrets": own.Secrets, "registryPassword": e.fn(a).RegistryPassword})
	if code, _ := call("owner-p", "PATCH", base+"/"+b, `{"env":{"A":"1"}}`); code != 503 || e.fn(b).DeployingID != "" || e.cluster.calls["ApplyCR"] != applies {
		t.Fatalf("a deployment with another function's registry password: %d %s, want 503", code, lastResponse())
	}
	// And a secret's blob does not open as a registry password of the same function.
	move(pgdoc.M{"registryPassword": own.Secrets["TOKEN"]})
	if code, _ := call("owner-p", "PATCH", base+"/"+b, `{"env":{"A":"1"}}`); code != 503 || e.fn(b).DeployingID != "" {
		t.Fatalf("a secret's blob in the place of the registry password: %d %s, want 503", code, lastResponse())
	}
	// With its own blobs back the function deploys again.
	move(pgdoc.M{"registryPassword": own.RegistryPassword})
	if code, _ := call("owner-p", "PATCH", base+"/"+b, `{"env":{"A":"1"}}`); code != 200 || !e.settle(b, false) || e.fn(b).Env["A"] != "1" {
		t.Fatalf("a deployment with the function's own blobs: %d %s", code, lastResponse())
	}
	// What is refused can still be removed: a delete needs no value.
	move(pgdoc.M{"secrets": e.fn(a).Secrets})
	if code, _ := call("owner-p", "DELETE", base+"/"+b, ""); code != 202 || e.cluster.replicas("fn-p", b) != -1 {
		t.Fatalf("delete of a function whose blobs do not open: %d", code)
	}
	scan.leaks("the end")
}

// What this release does not offer is refused by name on a create and on an update, with nothing created, changed or
// deployed; the routes of the later phases do not exist.
func TestFunctionRefusesWhatIsNotOffered(t *testing.T) {
	e := newFnEnv(t)
	base := "/project/p/functions"
	id := e.ready(`{"name":"api","image":"nginx:1"}`)
	for _, later := range []string{
		`"exposure":"public"`,
		`"gpu":{"model":"nvidia-3090","count":1}`,
		`"domains":["shop.example.com"]`,
		`"publicRateLimit":{"requestsPerSecond":10}`,
		`"source":{"repo":"acme/api","branch":"main"}`,
	} {
		for _, c := range [][3]string{{"POST", base, `{"name":"later","image":"nginx:1",` + later + `}`}, {"PATCH", base + "/" + id, `{` + later + `}`}, {"PATCH", base + "/" + id, `{"idleSeconds":120,` + later + `}`}} {
			code, body := e.raw("owner-p", c[0], c[1], c[2])
			if code != 400 || !strings.Contains(body, "not offered") {
				t.Errorf("%s %s: %d %s, want 400 not offered", c[0], c[2], code, body)
			}
		}
	}
	if fn := e.fn(id); len(e.records()) != 1 || len(e.deployments(id)) != 1 || fn.IdleSeconds != 300 || fn.Exposure != function.ExposurePrivate {
		t.Fatalf("a refused request changed something: %+v", e.records())
	}
	// What is offered, said out loud, is taken: a private exposure, a null for what is not there.
	if code, body := e.raw("owner-p", "PATCH", base+"/"+id, `{"exposure":"private","gpu":null,"domains":null,"source":null,"publicRateLimit":null,"idleSeconds":120}`); code != 200 || e.fn(id).IdleSeconds != 120 {
		t.Fatalf("an update that names only what is offered: %d %s", code, body)
	}
	// A function cannot be renamed, and an image host is a DNS name.
	for _, body := range []string{`{"name":"other"}`, `{"image":"10.0.0.7/acme/api:1"}`, `{"image":"localhost/acme/api:1"}`} {
		if code, _ := e.raw("owner-p", "PATCH", base+"/"+id, body); code != 400 {
			t.Errorf("patch %s: %d, want 400", body, code)
		}
	}
	for _, c := range [][2]string{
		{"POST", base + "/" + id + "/builds"}, {"POST", base + "/" + id + "/domains"}, {"DELETE", base + "/" + id + "/domains/shop.example.com"},
		{"GET", base + "/" + id + "/deployments/0123456789abcdef01234567/build-log"}, {"GET", "/project/p/github/installations"}, {"POST", "/github/callback"},
		{"POST", "/internal/functions/" + id + "/wake"}, {"POST", "/internal/functions/activity"},
	} {
		if code, _ := e.raw("owner-p", c[0], c[1], `{}`); code != 404 && code != 405 {
			t.Errorf("%s %s: %d, want no such route", c[0], c[1], code)
		}
	}
}

// The project's function quota. It is resolved key by key (the project's functionQuota, else the provider's
// functions.projectQuota, else the built-in default) and never unlimited; what a function holds of it is reserved
// (cpuMillis and memoryMi times maxInstances), so a wake the portal allows is one the namespace's own quota allows;
// of several creates at the limit only as many as fit succeed; and nothing of it is in, or changes, the project's
// quota document, which stays the Proxmox quota it is.
func TestFunctionQuota(t *testing.T) {
	e := newFnEnv(t)
	base := "/project/p/functions"
	usage := func() (limits, used map[string]any) {
		t.Helper()
		code, data := e.call("owner-p", "GET", "/project/p/quota-usage?serviceId=svc", "")
		fns, _ := data["functions"].(map[string]any)
		if code != 200 || fns == nil {
			t.Fatalf("quota usage: %d %v", code, data)
		}
		return fns["limits"].(map[string]any), fns["usage"].(map[string]any)
	}
	want := func(what string, m map[string]any, functions, vcpus, ramMb float64) {
		t.Helper()
		if m["functions"] != functions || m["vcpus"] != vcpus || m["ramMb"] != ramMb {
			t.Errorf("%s: %v, want functions %v, vcpus %v, ramMb %v", what, m, functions, vcpus, ramMb)
		}
	}

	// Resolution. The built-in default, held to the provider's maxFunctions (3 here) whatever the quota says.
	limits, used := usage()
	want("the built-in default", limits, 3, 8, 16384)
	want("no function yet", used, 0, 0, 0)
	// The provider's default takes the place of the built-in one, key by key.
	block := fnBlock()
	block["maxFunctions"], block["projectQuota"] = 10, map[string]any{"functions": 6, "vcpus": 4}
	e.provider(externalservice.StatusPublic, block)
	limits, _ = usage()
	want("the provider's default", limits, 6, 4, 16384)
	// The project's own values win, key by key; a key it does not name falls back, it does not lift the limit.
	e.project(pgdoc.M{"functionQuota": map[string]any{"vcpus": 1, "ramMb": 1024}, "quota": map[string]any{"sandboxes": 2}})
	limits, _ = usage()
	want("the project's own", limits, 6, 1, 1024)

	// Reserved usage: 500m at up to two instances holds a whole vCPU, running or not.
	a := e.ready(`{"name":"a","image":"nginx:1","cpuMillis":500,"memoryMi":256,"maxInstances":2}`)
	_, used = usage()
	want("one function of 500m at two instances", used, 1, 1, 512)
	if hard := e.cluster.quota("fn-p"); hard["count/deployments.apps"] != "6" || hard["requests.cpu"] != "1" || hard["requests.memory"] != "1024Mi" {
		t.Fatalf("the namespace's own quota does not follow the project's: %v", hard)
	}
	// The project is at its vCPU limit: nothing more fits, at any size, and nothing is left of the refused create.
	code, body := e.raw("owner-p", "POST", base, `{"name":"b","image":"nginx:1"}`)
	if code != 429 || !strings.Contains(body, "1 vCPUs") || len(e.records()) != 1 {
		t.Fatalf("a create over the vCPU quota: %d %s (%d records)", code, body, len(e.records()))
	}
	// Raising what a holds is refused, by its instances and by its size; lowering it is not.
	for _, body := range []string{`{"maxInstances":3}`, `{"cpuMillis":1000}`, `{"memoryMi":1024}`} {
		if code, out := e.raw("owner-p", "PATCH", base+"/"+a, body); code != 429 || e.fn(a).MaxInstances != 2 || e.fn(a).DeployingID != "" {
			t.Errorf("patch %s over the quota: %d %s", body, code, out)
		}
	}
	if code, body := e.raw("owner-p", "PATCH", base+"/"+a, `{"maxInstances":1}`); code != 200 || e.fn(a).MaxInstances != 1 {
		t.Fatalf("lowering maxInstances: %d %s", code, body)
	}
	_, used = usage()
	want("after lowering maxInstances", used, 1, 0.5, 256)
	// A smaller size: while it rolls out both run, so the larger one is held; once it serves, the smaller one.
	if code, body := e.raw("owner-p", "PATCH", base+"/"+a, `{"cpuMillis":250}`); code != 200 {
		t.Fatalf("a smaller size: %d %s", code, body)
	}
	if _, used = usage(); used["vcpus"] != 0.5 {
		t.Errorf("during the rollout to a smaller size the function holds %v vCPUs, want the larger 0.5", used["vcpus"])
	}
	if !e.settle(a, false) {
		t.Fatal("the rollout must settle")
	}
	_, used = usage()
	want("after the rollout to a smaller size", used, 1, 0.25, 256)
	// Memory is counted the same way: 256 + 768 is the project's 1024, one more MiB is not.
	if code, body := e.raw("owner-p", "POST", base, `{"name":"big","image":"nginx:1","memoryMi":1024}`); code != 429 || !strings.Contains(body, "1024 MiB") {
		t.Fatalf("a create over the memory quota: %d %s", code, body)
	}
	b := e.ready(`{"name":"b","image":"nginx:1","memoryMi":768}`)
	_, used = usage()
	want("two functions", used, 2, 0.5, 1024)

	// A quota lowered under what the project holds: a suspended function is not resumed into it, a function can
	// still be made smaller or deleted, and with room again the resume works.
	if code, _ := e.raw("owner-p", "POST", base+"/"+b+"/action", `{"action":"suspend"}`); code != 200 {
		t.Fatalf("suspend: %d", code)
	}
	e.project(pgdoc.M{"functionQuota": map[string]any{"vcpus": 1, "ramMb": 512}})
	if code, body := e.raw("owner-p", "POST", base+"/"+b+"/action", `{"action":"resume"}`); code != 429 || !e.fn(b).Suspended {
		t.Fatalf("a resume over a lowered quota: %d %s", code, body)
	}
	// The namespace's own quota does not go under what the project holds: the instances of what was admitted
	// before still start. It follows the project's quota down as the functions get smaller.
	if code, body := e.raw("owner-p", "PATCH", base+"/"+a, `{"idleSeconds":120}`); code != 200 {
		t.Fatalf("a setting under a lowered quota: %d %s", code, body)
	}
	if hard := e.cluster.quota("fn-p"); hard["requests.memory"] != "1024Mi" || hard["requests.cpu"] != "1" {
		t.Fatalf("the namespace's quota under a project quota lowered below what is held: %v", hard)
	}
	if code, body := e.raw("owner-p", "PATCH", base+"/"+b, `{"memoryMi":256}`); code != 200 || !e.settle(b, false) {
		t.Fatalf("making a function smaller under a lowered quota: %d %s", code, body)
	}
	// The operator raises the quota while the function is suspended: the resume is what tells the namespace.
	e.project(pgdoc.M{"functionQuota": map[string]any{"vcpus": 1, "ramMb": 640}})
	if code, body := e.raw("owner-p", "POST", base+"/"+b+"/action", `{"action":"resume"}`); code != 200 || e.fn(b).Suspended {
		t.Fatalf("a resume within the quota: %d %s", code, body)
	}
	if hard := e.cluster.quota("fn-p"); hard["requests.memory"] != "640Mi" {
		t.Fatalf("the namespace's quota after the project's changed: %v", hard)
	}

	// Concurrent creates at the limit: room for two more functions of the project's six, eight ask at once.
	e.project(pgdoc.M{"functionQuota": map[string]any{"functions": 4, "vcpus": 8, "ramMb": 8192}})
	// Each of the eight has asked whether it fits before any of them writes: all eight were told yes.
	e.cluster.barrier(8)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = e.raw("owner-p", "POST", base, fmt.Sprintf(`{"name":"burst-%d","image":"nginx:1"}`, i))
		}()
	}
	wg.Wait()
	if e.cluster.held != 8 {
		t.Fatalf("%d of the eight creates reached the cluster: the others were refused before the race", e.cluster.held)
	}
	e.cluster.barrier(0)
	slices.Sort(codes)
	if !slices.Equal(codes, []int{200, 200, 429, 429, 429, 429, 429, 429}) || len(e.records()) != 4 {
		t.Fatalf("eight creates with room for two: %v, %d records", codes, len(e.records()))
	}
	var rows int64
	if rows, _ = e.db.C("cloudResource").Count(e.ctx, pgdoc.M{"type": cloud.TypeFunction}); rows != 4 {
		t.Fatalf("%d live rows for 4 functions", rows)
	}
	limits, used = usage()
	want("at the limit", limits, 4, 8, 8192)
	want("at the limit", used, 4, 1, 1024)
	// The same for the vCPUs: room for one more instance's worth, four updates ask for it at once.
	e.project(pgdoc.M{"functionQuota": map[string]any{"functions": 4, "vcpus": 2, "ramMb": 8192}})
	fns := e.records()
	for i := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = e.raw("owner-p", "PATCH", base+"/"+fns[i].ID, `{"maxInstances":5}`)
		}()
	}
	wg.Wait()
	raised := 0
	for _, fn := range e.records() {
		if fn.MaxInstances == 5 {
			raised++
		}
	}
	// One of 250m to five instances is 1.25 vCPUs on top of the other three's 0.75: exactly the quota's 2.
	if _, used = usage(); raised != 1 || used["vcpus"] != float64(2) {
		t.Fatalf("four updates that each fit alone: %d raised, %v vCPUs held (codes %v)", raised, used["vcpus"], codes[:4])
	}

	// The project's quota document is as the operator wrote it: no function limit is in it, so it is still no
	// Proxmox quota, and a project that held only sandbox keys keeps its provider's Proxmox defaults.
	proj, err := e.h.svc.repo.FindByID(e.ctx, "p")
	if err != nil || !reflect.DeepEqual(proj.Quota, map[string]any{"sandboxes": float64(2)}) || hasPVEQuota(proj.Quota) {
		t.Fatalf("the project's quota document after function limits and usage: %v (%v)", proj.Quota, err)
	}
	// A save of the project (a rename, a member change) leaves the function quota where it is.
	if err := e.h.svc.repo.Save(e.ctx, proj); err != nil {
		t.Fatal(err)
	}
	if proj, _ = e.h.svc.repo.FindByID(e.ctx, "p"); fmt.Sprint(proj.FunctionQuota["vcpus"]) != "2" {
		t.Fatalf("a save of the project dropped its function quota: %v", proj.FunctionQuota)
	}
	// A member's view of the project does not carry it (the quota usage answer does).
	if out, _ := json.Marshal(proj); strings.Contains(string(out), "functionQuota") {
		t.Errorf("the project as the API answers it carries the function quota: %s", out)
	}
}

// A raised maxInstances is counted at the size the function holds when the quota is asked, and while a rollout is in
// progress that is the larger of its two sizes: the cap cannot be raised at the small size that still serves while a
// large one rolls out, and leave the project over its quota once the rollout settles.
func TestFunctionQuotaDuringRollout(t *testing.T) {
	e := newFnEnv(t)
	base := "/project/p/functions"
	e.project(pgdoc.M{"functionQuota": map[string]any{"vcpus": 2}})
	a := e.ready(`{"name":"a","image":"nginx:1","cpuMillis":250}`)
	// 2000m at one instance is the project's whole quota.
	if code, body := e.raw("owner-p", "PATCH", base+"/"+a, `{"cpuMillis":2000}`); code != 200 || e.fn(a).DeployingID == "" {
		t.Fatalf("a rollout to the size of the quota: %d %s", code, body)
	}
	// A second instance of it would be 4 vCPUs. The size that still serves (250m) is not what it is counted at.
	if code, body := e.raw("owner-p", "PATCH", base+"/"+a, `{"maxInstances":2}`); code != 429 || e.fn(a).MaxInstances != 1 {
		t.Fatalf("maxInstances raised while a larger size rolls out: %d %s (maxInstances %d)", code, body, e.fn(a).MaxInstances)
	}
	if !e.settle(a, false) {
		t.Fatal("the rollout must settle")
	}
	_, data := e.call("owner-p", "GET", "/project/p/quota-usage?serviceId=svc", "")
	fns, _ := data["functions"].(map[string]any)
	if used, _ := fns["usage"].(map[string]any); used["vcpus"] != float64(2) {
		t.Fatalf("the project holds %v vCPUs of its quota of 2", used["vcpus"])
	}
	// The other way round: while a smaller size rolls out the larger one still runs, and is what a second instance is
	// counted at. Once the smaller one serves, two of it are the same 2 vCPUs.
	if code, body := e.raw("owner-p", "PATCH", base+"/"+a, `{"cpuMillis":1000}`); code != 200 || e.fn(a).DeployingID == "" {
		t.Fatalf("a rollout to a smaller size: %d %s", code, body)
	}
	if code, body := e.raw("owner-p", "PATCH", base+"/"+a, `{"maxInstances":2}`); code != 429 || e.fn(a).MaxInstances != 1 {
		t.Fatalf("maxInstances raised while the larger size still runs: %d %s", code, body)
	}
	// A setting that holds nothing more is never refused, also during a rollout.
	if code, body := e.raw("owner-p", "PATCH", base+"/"+a, `{"idleSeconds":120}`); code != 200 || e.fn(a).IdleSeconds != 120 {
		t.Fatalf("a setting during a rollout: %d %s", code, body)
	}
	if !e.settle(a, false) {
		t.Fatal("the rollout must settle")
	}
	if code, body := e.raw("owner-p", "PATCH", base+"/"+a, `{"maxInstances":2}`); code != 200 || e.fn(a).MaxInstances != 2 {
		t.Fatalf("two instances of the smaller size, the same 2 vCPUs: %d %s", code, body)
	}
}

// The usage gates every create, deployment, rollback and resume passes: the project can be billed and the location's
// release gates hold. A read, a suspend and a delete pass without them.
func TestFunctionUsageGates(t *testing.T) {
	e := newFnEnv(t)
	base := "/project/p/functions"
	id := e.ready(`{"name":"api","image":"nginx:1"}`)
	if code, _ := e.raw("owner-p", "PATCH", base+"/"+id, `{"env":{"A":"1"}}`); code != 200 || !e.settle(id, false) {
		t.Fatal("a second deployment")
	}
	first := e.deployments(id)[1].ID
	if code, _ := e.raw("owner-p", "POST", base+"/"+id+"/action", `{"action":"suspend"}`); code != 200 {
		t.Fatal("suspend")
	}
	adding := [][3]string{
		{"POST", base, `{"name":"more","image":"nginx:1"}`},
		{"PATCH", base + "/" + id, `{"env":{"B":"2"}}`},
		{"POST", base + "/" + id + "/rollback", `{"deploymentId":"` + first + `"}`},
		{"POST", base + "/" + id + "/action", `{"action":"resume"}`},
		{"PATCH", base + "/" + id, `{"maxInstances":2}`},
		{"PATCH", base + "/" + id, `{"minInstances":1}`},
	}
	refused := func(what string, status int, words string) {
		t.Helper()
		for _, c := range adding {
			if code, body := e.raw("owner-p", c[0], c[1], c[2]); code != status || !strings.Contains(body, words) {
				t.Errorf("%s: %s %s: %d %s, want %d %q", what, c[0], c[1], code, body, status, words)
			}
		}
		if fn := e.fn(id); len(e.records()) != 1 || len(e.deployments(id)) != 2 || !fn.Suspended || fn.DeployingID != "" || fn.Instances != 0 || fn.MaxInstances != 1 || fn.MinInstances != 0 {
			t.Fatalf("%s: a refused request added usage: %+v", what, fn)
		}
		// What adds nothing is answered: a read, and a setting that starts nothing or lowers what may run.
		for _, c := range [][3]string{{"GET", base, ""}, {"GET", base + "/" + id, ""}, {"PATCH", base + "/" + id, `{"idleSeconds":120}`}, {"PATCH", base + "/" + id, `{"maxInstances":1,"minInstances":0}`}} {
			if code, body := e.raw("owner-p", c[0], c[1], c[2]); code != 200 {
				t.Errorf("%s: %s %s: %d %s, want 200", what, c[0], c[1], code, body)
			}
		}
	}
	e.gates = []sandbox.Gate{{Name: "fence", OK: false, Detail: "worker pc-7 on node pve3 has no firewall"}}
	e.provider(externalservice.StatusPublic, fnBlock())
	refused("a release gate does not hold", 503, "Functions are not available on this location right now")
	if _, body := e.raw("owner-p", "POST", base, `{"name":"more","image":"nginx:1"}`); strings.Contains(body, "pve3") || strings.Contains(body, "fence") {
		t.Errorf("the tenant is told the operator's reason: %s", body)
	}
	e.gates = []sandbox.Gate{{Name: "test", OK: true}}
	e.provider(externalservice.StatusPublic, fnBlock())
	e.project(pgdoc.M{"status": StatusDisabled})
	refused("the project is disabled", 409, "nothing can be created or started")
	e.project(pgdoc.M{"status": StatusEnabled})
	for _, c := range adding[1:] {
		if code, body := e.raw("owner-p", c[0], c[1], c[2]); code != 200 {
			t.Fatalf("with the gates open: %s %s: %d %s", c[0], c[1], code, body)
		}
		if e.fn(id).DeployingID != "" && !e.settle(id, false) {
			t.Fatal("the rollout must settle")
		}
	}
	// A delete passes with every gate shut.
	e.gates = nil
	e.provider(externalservice.StatusPublic, fnBlock())
	e.project(pgdoc.M{"status": StatusDisabled})
	if code, body := e.raw("owner-p", "DELETE", base+"/"+id, ""); code != 202 || len(e.records()) != 0 {
		t.Fatalf("a delete with the gates shut: %d %s", code, body)
	}
}

// Where functions are off (the provider's block is off, the block is not valid, the provider is disabled) a read and
// a DELETE are answered, so that what is left can be seen and removed, and everything else is 403. A project with no
// location for functions reads an empty list. Without a function service in the portal every route answers 503.
func TestFunctionOffRule(t *testing.T) {
	e := newFnEnv(t)
	base := "/project/p/functions"
	off := map[string]any{"enabled": false, "maxFunctions": 3}
	for name, turnOff := range map[string]func(){
		"the block is off": func() { e.provider(externalservice.StatusPublic, off) },
		"the block is not valid": func() {
			e.provider(externalservice.StatusPublic, map[string]any{"enabled": true, "maxInstances": 1000})
		},
		"the provider is disabled": func() { e.provider(externalservice.StatusDisabled, fnBlock()) },
	} {
		e.provider(externalservice.StatusPublic, fnBlock())
		id := e.ready(`{"name":"api","image":"nginx:1","secrets":{"TOKEN":"x"}}`)
		if code, _ := e.raw("owner-p", "PATCH", base+"/"+id, `{"env":{"A":"1"}}`); code != 200 || !e.settle(id, false) {
			t.Fatalf("%s: a second deployment", name)
		}
		first := e.deployments(id)[1].ID
		turnOff()
		for _, c := range [][3]string{
			{"POST", base, `{"name":"more","image":"nginx:1"}`},
			{"PATCH", base + "/" + id, `{"idleSeconds":120}`},
			{"PATCH", base + "/" + id, `{"env":{"B":"2"}}`},
			{"POST", base + "/" + id + "/rollback", `{"deploymentId":"` + first + `"}`},
			{"POST", base + "/" + id + "/action", `{"action":"suspend"}`},
			{"POST", base + "/" + id + "/action", `{"action":"resume"}`},
		} {
			if code, body := e.raw("owner-p", c[0], c[1], c[2]); code != 403 || !strings.Contains(body, "functions are not offered here") {
				t.Errorf("%s: %s %s: %d %s, want 403", name, c[0], c[1], code, body)
			}
		}
		if fn := e.fn(id); len(e.records()) != 1 || len(e.deployments(id)) != 2 || fn.IdleSeconds != 300 || fn.Suspended || fn.DeployingID != "" {
			t.Fatalf("%s: a refused request changed the function: %+v", name, fn)
		}
		code, list := e.raw("owner-p", "GET", base, "")
		if code != 200 || !strings.Contains(list, id) {
			t.Errorf("%s: the list: %d %s", name, code, list)
		}
		for _, path := range []string{base + "/" + id, base + "/" + id + "/deployments", base + "/" + id + "/logs"} {
			if code, body := e.raw("owner-p", "GET", path, ""); code != 200 {
				t.Errorf("%s: GET %s: %d %s, want 200", name, path, code, body)
			}
		}
		// The service the console reads has no functions block: the menu entry hides with the routes.
		es, err := e.h.esSvc.Get(e.ctx, "svc")
		if err != nil {
			t.Fatal(err)
		}
		if _, has := externalServiceDto(es)["functions"]; has {
			t.Errorf("%s: the project's service still carries the functions block", name)
		}
		// The delete: Deployment and Secret gone, records gone, the row archived with its end.
		if code, body := e.raw("owner-p", "DELETE", base+"/"+id, ""); code != 202 {
			t.Fatalf("%s: delete: %d %s", name, code, body)
		}
		if rows := e.archived(id); len(e.records()) != 0 || len(e.deployments(id)) != 0 || e.cluster.replicas("fn-p", id) != -1 || e.cluster.secretCount() != 0 ||
			e.row(id) != nil || len(rows) != 1 || rows[0].Data["status"] != functionRowDeleted || rows[0].Data["stoppedAt"] == nil {
			t.Fatalf("%s: after the delete: %d records, row %v, archived %+v", name, len(e.records()), e.row(id), rows)
		}
	}

	// A project whose services run no functions: an empty list, and a create is 403.
	e.provider(externalservice.StatusPublic, fnBlock())
	if _, err := e.db.C("project").SetByID(e.ctx, "foreign", pgdoc.M{"services": []any{}}, nil); err != nil {
		t.Fatal(err)
	}
	if code, body := e.raw("owner-foreign", "GET", "/project/foreign/functions", ""); code != 200 || !strings.Contains(body, `"data":[]`) {
		t.Errorf("the list of a project with no location: %d %s", code, body)
	}
	if code, _ := e.raw("owner-foreign", "POST", "/project/foreign/functions", `{"name":"api","image":"nginx:1"}`); code != 403 {
		t.Errorf("a create in a project with no location: %d, want 403", code)
	}
	if ns, _ := e.cluster.GetNamespace(e.ctx, "fn-foreign"); ns != nil {
		t.Error("a refused create made a namespace")
	}
	// Another project's function is not found, whoever asks.
	id := e.ready(`{"name":"api","image":"nginx:1"}`)
	for _, c := range [][3]string{{"owner-foreign", "GET", "/project/foreign/functions/" + id}, {"owner-foreign", "DELETE", "/project/foreign/functions/" + id}, {"outsider", "GET", base + "/" + id}} {
		if code, _ := e.raw(c[0], c[1], c[2], ""); code != 404 {
			t.Errorf("%s %s as %s: %d, want 404", c[1], c[2], c[0], code)
		}
	}
	// No function service in the portal: nothing is offered and nothing is read.
	e.h.functionFor = nil
	for _, c := range [][2]string{{"GET", base}, {"POST", base}, {"GET", base + "/" + id}, {"DELETE", base + "/" + id}} {
		if code, body := e.raw("owner-p", c[0], c[1], `{"name":"x","image":"nginx:1"}`); code != 503 || !strings.Contains(body, "functions are not configured") {
			t.Errorf("%s %s without a function service: %d %s, want 503", c[0], c[1], code, body)
		}
	}
	if len(e.records()) != 1 {
		t.Fatal("a request without a function service changed the records")
	}
}

// A function's record and its cloud row say the same of its running intervals, at every boundary; a delete archives
// the row with the end of the last interval and leaves no live row behind; and when the row cannot be written the
// record is not either.
func TestFunctionRecordAndRow(t *testing.T) {
	e := newFnEnv(t)
	base := "/project/p/functions"
	parse := func(v any) time.Time {
		t.Helper()
		at, err := time.Parse(time.RFC3339, fmt.Sprint(v))
		if err != nil {
			t.Fatalf("not a time: %v", v)
		}
		return at
	}
	agree := func(what string, id string) (functionDoc, map[string]any) {
		t.Helper()
		fn, row := e.fn(id), e.row(id)
		if row == nil || row.Type != cloud.TypeFunction || row.ProjectID != "p" {
			t.Fatalf("%s: no cloud row of the function: %+v", what, row)
		}
		d := row.Data
		if intAny(d["instances"]) != fn.Instances || (fn.RunningSince == nil) != (d["runningSince"] == nil) || (fn.StoppedAt == nil) != (d["stoppedAt"] == nil) ||
			(d["endedIntervals"] == nil) != (len(fn.Ended) == 0) || functionRowStale(d, e.h.functionRowData(e.ctx, &fn)) {
			t.Fatalf("%s: record and row disagree: record %+v, row %v", what, fn, d)
		}
		if fn.RunningSince != nil && !parse(d["runningSince"]).Equal(fn.RunningSince.Truncate(time.Second)) {
			t.Fatalf("%s: runningSince %v in the row, %v in the record", what, d["runningSince"], fn.RunningSince)
		}
		return fn, d
	}

	// A create: the row is there before anything runs, at the size the function may run at.
	id := e.create(`{"name":"api","image":"nginx:1","cpuMillis":500,"memoryMi":512,"minInstances":1}`)
	fn, row := agree("deploying", id)
	if fn.Instances != 1 || fn.RunningSince == nil || row["status"] != functionRowRunning || intAny(row["cpuMillis"]) != 500 || intAny(row["billedInstances"]) != 1 || row["name"] != "api" || row["image"] != "nginx:1" {
		t.Fatalf("while its first rollout warms an instance: record %+v, row %v", fn, row)
	}
	if !e.settle(id, false) {
		t.Fatal("the rollout must settle")
	}
	agree("serving", id)
	// A rollout to a larger size: from the claim on, the larger size is in the row; a failed one gives it back.
	if code, _ := e.raw("owner-p", "PATCH", base+"/"+id, `{"image":"bad:1","cpuMillis":1000,"memoryMi":1024}`); code != 200 {
		t.Fatal("patch to a larger size")
	}
	if _, row = agree("rolling out a larger size", id); intAny(row["cpuMillis"]) != 1000 || intAny(row["memoryMi"]) != 1024 {
		t.Fatalf("the row during a rollout to a larger size: %v", row)
	}
	if !e.settle(id, true) {
		t.Fatal("the final poll must settle")
	}
	if _, row = agree("after the failed rollout", id); intAny(row["cpuMillis"]) != 500 || intAny(row["memoryMi"]) != 512 {
		t.Fatalf("the row after a failed rollout to a larger size: %v", row)
	}

	// The row cannot be written: nothing of the change is in the record either.
	trigger := `CREATE FUNCTION refuse_function_rows() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'no row write'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER refuse_function_rows BEFORE INSERT OR UPDATE OR DELETE ON "cloudResource" FOR EACH ROW EXECUTE FUNCTION refuse_function_rows()`
	if _, err := e.db.Pool.Exec(e.ctx, trigger); err != nil {
		t.Fatal(err)
	}
	before := e.fn(id)
	if code, _ := e.raw("owner-p", "POST", base+"/"+id+"/action", `{"action":"suspend"}`); code != 500 {
		t.Errorf("a suspend whose row write fails: %d, want 500", code)
	}
	stale := e.fn(id)
	if err := e.h.scaleFunction(e.ctx, e.fs(), &stale, 0); err == nil {
		t.Error("a stop whose row write fails must fail")
	}
	if code, _ := e.raw("owner-p", "PATCH", base+"/"+id, `{"env":{"A":"1"}}`); code != 500 {
		t.Errorf("a deployment whose row write fails: %d, want 500", code)
	}
	if code, _ := e.raw("owner-p", "POST", base, `{"name":"more","image":"nginx:1"}`); code != 500 {
		t.Errorf("a create whose row write fails: %d, want 500", code)
	}
	if code, _ := e.raw("owner-p", "DELETE", base+"/"+id, ""); code != 500 {
		t.Errorf("a delete whose row write fails: %d, want 500", code)
	}
	if got := e.fn(id); got.Suspended || got.Instances != before.Instances || got.StoppedAt != nil || len(got.Ended) != 0 || got.DeployingID != "" || got.DeploySeq != before.DeploySeq ||
		len(e.records()) != 1 || len(e.deployments(id)) != 2 {
		t.Fatalf("a change whose row write failed is in the record: %+v (%d records, %d deployments)", got, len(e.records()), len(e.deployments(id)))
	}
	if _, err := e.db.Pool.Exec(e.ctx, `DROP TRIGGER refuse_function_rows ON "cloudResource"`); err != nil {
		t.Fatal(err)
	}
	// The delete above stopped the function on the cluster before its records could not be dropped: the record
	// still says it runs, so it stays billed and can be deleted again. Its next deployment starts it again.
	if code, _ := e.raw("owner-p", "PATCH", base+"/"+id, `{"env":{"A":"1"}}`); code != 200 || !e.settle(id, false) || e.cluster.replicas("fn-p", id) != 1 {
		t.Fatalf("a deployment after the row writes work again: %d", code)
	}
	agree("after the row writes work again", id)

	// A stop: the interval ends in both, with the count and size it ran at.
	since := *e.fn(id).RunningSince
	if code, _ := e.raw("owner-p", "POST", base+"/"+id+"/action", `{"action":"suspend"}`); code != 200 {
		t.Fatal("suspend")
	}
	fn, row = agree("suspended", id)
	ended, _ := row["endedIntervals"].([]any)
	if fn.Instances != 0 || fn.StoppedAt == nil || row["status"] != functionRowSuspended || intAny(row["billedInstances"]) != 1 || len(fn.Ended) != 1 || len(ended) != 1 ||
		!fn.Ended[0].From.Equal(since) || !fn.Ended[0].To.Equal(*fn.StoppedAt) || fn.Ended[0].CPUMillis != 500 || fn.Ended[0].Instances != 1 ||
		!parse(ended[0].(map[string]any)["to"]).Equal(fn.StoppedAt.Truncate(time.Second)) || e.cluster.replicas("fn-p", id) != 0 {
		t.Fatalf("after a stop: record %+v, row %v", fn, row)
	}
	// A second stop is none: no second boundary.
	if err := e.h.scaleFunction(e.ctx, e.fs(), &fn, 0); err != nil || len(e.fn(id).Ended) != 1 || !e.fn(id).StoppedAt.Equal(*fn.StoppedAt) {
		t.Fatalf("a stop of a stopped function: %v, %+v", err, e.fn(id))
	}
	// A resume starts the instance its floor asks for: a new interval, the ended one kept beside it.
	if code, _ := e.raw("owner-p", "POST", base+"/"+id+"/action", `{"action":"resume"}`); code != 200 {
		t.Fatal("resume")
	}
	fn, row = agree("resumed", id)
	if fn.Instances != 1 || fn.StoppedAt != nil || fn.RunningSince == nil || fn.RunningSince.Before(fn.Ended[0].To) ||
		row["status"] != functionRowRunning || len(fn.Ended) != 1 || e.cluster.replicas("fn-p", id) != 1 {
		t.Fatalf("after a resume: record %+v, row %v", fn, row)
	}
	// Two writers of the same start: exactly one interval boundary.
	if code, _ := e.raw("owner-p", "POST", base+"/"+id+"/action", `{"action":"suspend"}`); code != 200 {
		t.Fatal("suspend")
	}
	if _, err := e.h.fns.SetByID(e.ctx, id, pgdoc.M{"suspended": false}, nil); err != nil {
		t.Fatal(err)
	}
	stopped := e.fn(id)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn := stopped
			if err := e.h.scaleFunction(e.ctx, e.fs(), &fn, 1); err != nil {
				t.Errorf("a start: %v", err)
			}
		}()
	}
	wg.Wait()
	if fn, _ = agree("six starts at once", id); fn.Instances != 1 || len(fn.Ended) != 2 || fn.StoppedAt != nil {
		t.Fatalf("after six starts at once: %+v", fn)
	}

	// The delete: the row is archived with the end of the interval it was in, at the count it ran with, and there
	// is no live row of the function from that moment (nothing lingers to be rated as a live resource).
	running := *fn.RunningSince
	if code, _ := e.raw("owner-p", "DELETE", base+"/"+id, ""); code != 202 {
		t.Fatal("delete")
	}
	rows := e.archived(id)
	if e.row(id) != nil || len(rows) != 1 || len(e.records()) != 0 || len(e.deployments(id)) != 0 {
		t.Fatalf("after the delete: live row %v, %d archived, %d records", e.row(id), len(rows), len(e.records()))
	}
	d := rows[0].Data
	if d["status"] != functionRowDeleted || intAny(d["instances"]) != 0 || intAny(d["billedInstances"]) != 1 || intAny(d["cpuMillis"]) != 500 ||
		!parse(d["runningSince"]).Equal(running.Truncate(time.Second)) || parse(d["stoppedAt"]).Before(parse(d["runningSince"])) || rows[0].DeletedAt == nil ||
		parse(d["stoppedAt"]).After(rows[0].DeletedAt.Add(time.Second)) || len(d["endedIntervals"].([]any)) != 2 {
		t.Fatalf("the archived row of a function deleted while it ran: %v (deleted at %v)", d, rows[0].DeletedAt)
	}
	// One deleted while stopped keeps the end it had.
	idle := e.ready(`{"name":"idle","image":"nginx:1"}`)
	if code, _ := e.raw("owner-p", "POST", base+"/"+idle+"/action", `{"action":"suspend"}`); code != 200 {
		t.Fatal("suspend")
	}
	end := *e.fn(idle).StoppedAt
	time.Sleep(1100 * time.Millisecond)
	if code, _ := e.raw("owner-p", "DELETE", base+"/"+idle, ""); code != 202 {
		t.Fatal("delete")
	}
	if rows = e.archived(idle); len(rows) != 1 || !parse(rows[0].Data["stoppedAt"]).Equal(end.Truncate(time.Second)) || rows[0].Data["status"] != functionRowDeleted {
		t.Fatalf("the archived row of a function deleted while stopped: %+v (stopped at %v)", rows, end)
	}
}

// Project deletion: the project's function namespace goes with everything in it, the records go, and every row is
// archived with its end. A location whose namespace cannot be deleted is an error and nothing is dropped; the
// project's teardown then reports the functions as left, keeps the project in deletion and does not touch their rows.
func TestFunctionProjectDeletion(t *testing.T) {
	e := newFnEnv(t)
	// The sandbox leg of the teardown, over a cluster that has no sandbox namespace of the project.
	e.h.sandboxFor = func(*externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(noSandboxes{}, sandbox.Config{}, "svc"), nil
	}
	running := e.ready(`{"name":"running","image":"nginx:1","secrets":{"TOKEN":"x"}}`)
	stopped := e.ready(`{"name":"stopped","image":"nginx:1"}`)
	if code, _ := e.raw("owner-p", "POST", "/project/p/functions/"+stopped+"/action", `{"action":"suspend"}`); code != 200 {
		t.Fatal("suspend")
	}
	deploying := e.create(`{"name":"deploying","image":"nginx:1"}`)
	other := "fn-" + strings.Repeat("0", 16)
	if _, err := e.h.fns.InsertOne(e.ctx, &functionDoc{ID: other, ProjectID: "foreign", ServiceID: "svc"}); err != nil {
		t.Fatal(err)
	}

	// The namespace cannot be deleted: an error, and nothing is dropped.
	e.cluster.fail = map[string]error{"DeleteNamespace": &kamajik8s.APIError{Status: http.StatusServiceUnavailable}}
	if err := e.h.DeleteProjectFunctions(e.ctx, "p"); err == nil || len(e.records()) != 4 || e.row(running) == nil || len(e.archived("")) != 0 {
		t.Fatalf("a namespace that cannot be deleted: %v, %d records", err, len(e.records()))
	}
	e.project(pgdoc.M{"status": StatusDeleteInProgress})
	started := time.Now()
	err := e.h.TeardownProject(e.ctx, "p")
	proj, _ := e.h.svc.repo.FindByID(e.ctx, "p")
	if err == nil || !strings.Contains(err.Error(), "functions") || proj.Status != StatusDeleteInProgress {
		t.Fatalf("a teardown that cannot remove the functions: %v, project %s", err, proj.Status)
	}
	// The rows that stay are given to no other leg: the sweep of what is left would wait 20 s on them, twice.
	if took := time.Since(started); took > 15*time.Second {
		t.Errorf("the teardown held on to the functions' rows for %s", took.Round(time.Second))
	}
	if len(e.records()) != 4 || e.row(running) == nil || e.row(stopped) == nil || len(e.archived("")) != 0 || e.cluster.replicas("fn-p", running) != 1 {
		t.Fatalf("a teardown that could not remove the functions touched them: %d records, %d archived rows", len(e.records()), len(e.archived("")))
	}

	// The retry works: namespace, records and rows are gone, the project is deleted, the other project's function
	// is as it was.
	e.cluster.fail = nil
	if err := e.h.TeardownProject(e.ctx, "p"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if proj, _ = e.h.svc.repo.FindByID(e.ctx, "p"); proj.Status != StatusDeleted {
		t.Fatalf("the project after its teardown: %s", proj.Status)
	}
	if ns, _ := e.cluster.GetNamespace(e.ctx, "fn-p"); ns != nil || len(e.cluster.deps) != 0 || e.cluster.secretCount() != 0 {
		t.Fatalf("after the teardown the cluster holds: namespace %v, %d Deployments, %d Secrets", ns, len(e.cluster.deps), e.cluster.secretCount())
	}
	left := e.records()
	var deps, rows int64
	deps, _ = e.h.fnDeps.Count(e.ctx, pgdoc.M{"projectId": "p"})
	rows, _ = e.db.C("cloudResource").Count(e.ctx, pgdoc.M{"projectId": "p"})
	if len(left) != 1 || left[0].ID != other || deps != 0 || rows != 0 {
		t.Fatalf("after the teardown: records %+v, %d deployment records, %d live rows", left, deps, rows)
	}
	archived := e.archived("")
	if len(archived) != 3 {
		t.Fatalf("%d archived rows, want the project's 3", len(archived))
	}
	for _, row := range archived {
		d := row.Data
		wasRunning := row.ExternalID == running || row.ExternalID == deploying
		if d["status"] != functionRowDeleted || intAny(d["instances"]) != 0 || d["stoppedAt"] == nil || d["runningSince"] == nil || row.ProjectID != "p" || row.DeletedAt == nil ||
			(wasRunning && d["endedIntervals"] != nil) || (!wasRunning && len(d["endedIntervals"].([]any)) != 1) {
			t.Errorf("the archived row of %s: %v", row.ExternalID, d)
		}
	}
	// Every host ever issued stays: a URL is not handed out twice.
	if hosts, _ := e.h.fnHosts.Count(e.ctx, pgdoc.M{"projectId": "p"}); hosts != 3 {
		t.Errorf("%d hosts on record after the teardown, want 3", hosts)
	}
	// A second run finds nothing and fails on nothing.
	if err := e.h.DeleteProjectFunctions(e.ctx, "p"); err != nil || len(e.archived("")) != 3 {
		t.Fatalf("a second run: %v", err)
	}
}

// noSandboxes is a sandbox cluster with nothing of the project on it.
type noSandboxes struct{ sandbox.K8sAPI }

func (noSandboxes) GetNamespace(context.Context, string) (map[string]any, error) { return nil, nil }
