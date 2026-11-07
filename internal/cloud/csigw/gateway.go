// Package csigw is the CSI gateway: the PVE API as proxmox-csi-plugin's controller uses it, for one tenant cluster at a
// time, with no PVE credential in the cluster.
//
// The controller presents a per-cluster credential (csi@proxcloud!<clusterId>); the gateway calls PVE with its own token.
// It is not a reverse proxy. Each allowed call is matched by method and path, its parameters are checked one by one, and
// only those are sent on. Everything else answers 403. Ownership is never read from the request: a disk belongs to a
// cluster when the gateway wrote its csiVolume row, a VM when it is in the cluster's pool with its pc-k8s-<id> tag, a
// task when the gateway handed out its UPID. See docs/managed-k8s.md, "Block volumes through the CSI gateway".
package csigw

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Handler serves the gateway listener.
type Handler struct {
	Reg Registry
	// PVE builds a compute provider's client with the gateway token. Every call forwarded to PVE goes through it.
	PVE func(ctx context.Context, serviceID string) (*pve.Client, error)
	// Portal builds the provider's own client, for the one read the gateway token cannot make: /cluster/resources leaves
	// out a VM's pool without Pool.Audit, which ProxCloudCSI does not hold. It also hides proxcloud-system (NoAccess).
	Portal func(ctx context.Context, serviceID string) (*pve.Client, error)
	// Quota gates addGiB more disk for the project; csiUsed is the registry's share of the usage. nil = no quota.
	Quota func(ctx context.Context, projectID, serviceID string, addGiB int, csiUsed func() (int, error)) error
	// Audit records refusals. nil = log only.
	Audit func(audit.AuditEvent)
	Log   *slog.Logger

	mu    sync.Mutex
	cache map[string]cachedList
}

// NewServer is the gateway's own listener. It serves TLS only: the credential travels in a header. A call makes a few
// PVE requests of at most 30 s each, which bounds WriteTimeout; writes go on without the client (WithoutCancel).
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 3 * time.Minute, IdleTimeout: 2 * time.Minute}
}

const (
	cacheTTL = 10 * time.Second // the store list only; workers are read on every call
	taskTTL  = time.Hour
	maxBody  = 64 << 10
	gib      = int64(1) << 30
)

// refusal is an answer the gateway gives itself, not PVE's.
type refusal struct {
	status int
	msg    string
}

func (r *refusal) Error() string { return r.msg }

func deny(format string, a ...any) error {
	return &refusal{http.StatusForbidden, fmt.Sprintf(format, a...)}
}

func conflict(format string, a ...any) error {
	return &refusal{http.StatusConflict, fmt.Sprintf(format, a...)}
}

// call is one parsed request: the path below /api2/json split into segments, and its parameters from the query and the
// form body together (the plugin sends a POST's parameters in the query), one value each.
type call struct {
	method string
	seg    []string
	params url.Values
	cred   *Credential
	gw     *pve.Client
}

func (c *call) path() string { return "/" + strings.Join(c.seg, "/") }

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cred, err := h.authenticate(ctx, r.Header.Get("Authorization"))
	if err != nil {
		var rf *refusal
		if errors.As(err, &rf) {
			h.log().Warn("csi gateway: refused credential", "remote", r.RemoteAddr, "reason", rf.msg)
			writeError(w, rf.status, rf.msg)
			return
		}
		h.log().Error("csi gateway: credential lookup", "err", err)
		writeError(w, http.StatusServiceUnavailable, "csi gateway: try again")
		return
	}
	c, err := parseCall(w, r)
	if err == nil {
		c.cred = cred
		var data json.RawMessage
		if data, err = h.serve(ctx, c); err == nil {
			writeData(w, data)
			return
		}
	}
	method, path := r.Method, r.URL.EscapedPath()
	var rf *refusal
	var ae *pve.APIError
	switch {
	case errors.As(err, &rf):
		h.refused(cred, method, path, rf)
		writeError(w, rf.status, "csi gateway: "+rf.msg)
	case errors.As(err, &ae):
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(ae.Status)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "message": ae.Message, "errors": ae.Errors})
	default:
		h.log().Error("csi gateway: call failed", "cluster", cred.ID, "method", method, "path", path, "err", err)
		writeError(w, http.StatusBadGateway, "csi gateway: the call failed, try again")
	}
}

func writeData(w http.ResponseWriter, data json.RawMessage) {
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	_, _ = fmt.Fprintf(w, `{"data":%s}`, data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "message": msg})
}

func (h *Handler) refused(cred *Credential, method, path string, rf *refusal) {
	h.log().Warn("csi gateway: refused", "cluster", cred.ID, "method", method, "path", path, "reason", rf.msg)
	if h.Audit == nil {
		return
	}
	// ponytail: one audit row per refusal and no rate limit per credential; add one when a tenant floods it.
	ev := audit.SystemEvent()
	ev.RequestInterface = audit.InterfaceCSIGateway
	ev.EventContext = audit.ContextProject
	ev.Actor = &audit.AuditActor{Type: audit.ActorSystem, ID: TokenID(cred.ID), DisplayName: "proxmox-csi " + cred.ID}
	ev.Action, ev.Outcome = audit.ActionDeny, audit.OutcomeFailure
	ev.ResourceType, ev.ResourceID = audit.ResourceCSIVolume, cred.ID
	ev.ProjectID, ev.ErrorMessage = cred.ProjectID, rf.msg
	ev.ResourceMetadata = map[string]any{"clusterId": cred.ID, "method": method, "path": path, "status": rf.status}
	h.Audit(ev)
}

var (
	clusterIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	noSecretHash = hashSecret("")
)

// authenticate reads `PVEAPIToken=csi@proxcloud!<clusterId>=<secret>`, or with a space after the scheme as the plugin's
// HTTP client sends it (PVE takes both). The cluster comes from the credential alone.
func (h *Handler) authenticate(ctx context.Context, header string) (*Credential, error) {
	unauth := &refusal{http.StatusUnauthorized, "authentication failure"}
	tok, ok := strings.CutPrefix(header, "PVEAPIToken=")
	if !ok {
		tok, ok = strings.CutPrefix(header, "PVEAPIToken ")
	}
	if !ok {
		return nil, unauth
	}
	id, secret, ok := strings.Cut(tok, "=")
	if !ok || secret == "" {
		return nil, unauth
	}
	clusterID, ok := strings.CutPrefix(id, TokenUser+"!")
	if !ok || !clusterIDRe.MatchString(clusterID) {
		return nil, unauth
	}
	cred, err := h.Reg.Credential(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	// The hash is compared for an unknown cluster too, so the answer takes as long whether the id exists or not.
	want := noSecretHash
	if cred != nil {
		want = cred.SecretSHA256
	}
	match := subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(want)) == 1
	if cred == nil || cred.RevokedAt != nil || !match {
		return nil, unauth
	}
	return cred, nil
}

var segmentRe = regexp.MustCompile(`^[A-Za-z0-9._:@!-]+$`)

// parseCall refuses anything a path could be dressed up with: escapes, empty or dot segments, a trailing slash. What
// PVE then receives is rebuilt from the parsed pieces.
func parseCall(w http.ResponseWriter, r *http.Request) (*call, error) {
	// RawPath is also set for a plain "!" (a UPID's token id), which Go would escape: only a % is an escape.
	if strings.Contains(r.URL.RawPath, "%") || strings.Contains(r.URL.Path, "%") {
		return nil, deny("encoded path")
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api2/json/")
	if !ok || rest == "" {
		return nil, deny("not an allowed call")
	}
	seg := strings.Split(rest, "/")
	for _, s := range seg {
		if s == "." || s == ".." || !segmentRe.MatchString(s) {
			return nil, deny("malformed path")
		}
	}
	params := url.Values{}
	add := func(vals url.Values) error {
		for k, vs := range vals {
			if len(vs) != 1 || params.Has(k) {
				return deny("parameter %s given more than once", k)
			}
			params.Set(k, vs[0])
		}
		return nil
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, deny("malformed query")
	}
	if err := add(q); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return nil, deny("body too large")
	}
	if len(body) > 0 {
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/x-www-form-urlencoded" {
			return nil, deny("only form-encoded bodies")
		}
		f, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, deny("malformed body")
		}
		if err := add(f); err != nil {
			return nil, err
		}
	}
	return &call{method: r.Method, seg: seg, params: params}, nil
}

// only refuses any parameter outside keys; need refuses a missing one.
func (c *call) only(keys ...string) error {
	for k := range c.params {
		if !slices.Contains(keys, k) {
			return deny("parameter %s is not allowed here", k)
		}
	}
	return nil
}

func (c *call) need(keys ...string) error {
	for _, k := range keys {
		if c.params.Get(k) == "" {
			return deny("parameter %s is required", k)
		}
	}
	return nil
}

func (c *call) is(method string, shape ...string) bool {
	if c.method != method || len(c.seg) != len(shape) {
		return false
	}
	for i, s := range shape {
		if s != "*" && c.seg[i] != s {
			return false
		}
	}
	return true
}

// serve is the allowlist. Each case is one call proxmox-csi-plugin v0.22.0 makes, with the check that keeps it inside
// the cluster. Anything not listed, /cluster/nextid, VM create, replication, migrate and /access/ticket among them, is 403.
func (h *Handler) serve(ctx context.Context, c *call) (json.RawMessage, error) {
	route := h.route(c)
	if route == nil {
		return nil, deny("not an allowed call")
	}
	gw, err := h.PVE(ctx, c.cred.ComputeServiceID)
	if err != nil {
		return nil, err
	}
	c.gw = gw
	return route(ctx, c)
}

func (h *Handler) route(c *call) func(context.Context, *call) (json.RawMessage, error) {
	switch {
	case c.is("GET", "cluster", "resources"):
		return h.resources
	case c.is("GET", "storage", "*"):
		return h.storageConfig
	case c.is("GET", "nodes", "*", "storage", "*", "status"):
		return h.storageStatus
	case c.is("GET", "nodes", "*", "storage", "*", "content"):
		return h.contentList
	case c.is("POST", "nodes", "*", "storage", "*", "content"):
		return h.contentCreate
	case c.is("POST", "nodes", "*", "storage", "*", "content", "*"),
		c.is("POST", "nodes", "*", "storage", "*", "content", "*", "*"):
		return h.contentCopy
	case c.is("DELETE", "nodes", "*", "storage", "*", "content", "*"),
		c.is("DELETE", "nodes", "*", "storage", "*", "content", "*", "*"):
		return h.contentDelete
	case c.is("GET", "nodes", "*", "qemu", "*", "config"):
		return h.vmConfig
	case c.is("POST", "nodes", "*", "qemu", "*", "config"):
		return h.vmAttach
	case c.is("PUT", "nodes", "*", "qemu", "*", "config"):
		return h.vmRevert
	case c.is("PUT", "nodes", "*", "qemu", "*", "unlink"):
		return h.vmUnlink
	case c.is("PUT", "nodes", "*", "qemu", "*", "resize"):
		return h.vmResize
	case c.is("GET", "nodes", "*", "tasks", "*", "status"):
		return h.taskStatus
	}
	return nil
}

// --- what the cluster may see ---

// resource is a /cluster/resources entry, as far as the checks read it.
type resource struct {
	Type     string `json:"type"`
	Node     string `json:"node"`
	VMID     int    `json:"vmid"`
	Pool     string `json:"pool"`
	Tags     string `json:"tags"`
	Template int    `json:"template"`
	Storage  string `json:"storage"`
	Status   string `json:"status"`
	Shared   int    `json:"shared"`
}

type cachedList struct {
	at   time.Time
	list []resource
}

func listResources(ctx context.Context, cl *pve.Client, typ string) ([]resource, error) {
	raw, err := cl.Raw(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {typ}})
	if err != nil {
		return nil, err
	}
	var list []resource
	return list, json.Unmarshal(raw, &list)
}

// storages reads /cluster/resources?type=storage with the gateway token, cached per provider for cacheTTL. Stores are
// the operator's and never change hands, so a stale list cannot cross tenants.
func (h *Handler) storages(ctx context.Context, serviceID string) ([]resource, error) {
	h.mu.Lock()
	if e, ok := h.cache[serviceID]; ok && time.Since(e.at) < cacheTTL {
		h.mu.Unlock()
		return e.list, nil
	}
	h.mu.Unlock()
	cl, err := h.PVE(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	list, err := listResources(ctx, cl, "storage")
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.cache == nil {
		h.cache = map[string]cachedList{}
	}
	h.cache[serviceID] = cachedList{time.Now(), list}
	h.mu.Unlock()
	return list, nil
}

// workers are the cluster's VMs: QEMU guests in its pool with its pc-k8s-<id> tag, by vmid → node. Read on every call,
// never cached: PVE hands a freed vmid to the next VM at once, so a list even seconds old would let a cluster reach the
// VM another project now has under its old worker's id.
func (h *Handler) workers(ctx context.Context, cred *Credential) (map[int]string, error) {
	cl, err := h.Portal(ctx, cred.ComputeServiceID)
	if err != nil {
		return nil, err
	}
	list, err := listResources(ctx, cl, "vm")
	if err != nil {
		return nil, err
	}
	tag := "pc-k8s-" + cred.ID
	out := map[int]string{}
	for _, r := range list {
		if r.Type == "qemu" && r.Template == 0 && r.Pool == cred.Pool && hasTag(r.Tags, tag) {
			out[r.VMID] = r.Node
		}
	}
	return out, nil
}

func hasTag(tags, tag string) bool {
	return slices.Contains(strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }), tag)
}

// worker checks that vmid is a worker of the cluster and node is where it runs.
func (h *Handler) worker(ctx context.Context, c *call, node, vmid string) error {
	id, err := strconv.Atoi(vmid)
	if err != nil || id <= 0 || strconv.Itoa(id) != vmid {
		return deny("vm %s is not a worker of this cluster", vmid)
	}
	ws, err := h.workers(ctx, c.cred)
	if err != nil {
		return err
	}
	if at, ok := ws[id]; !ok || at != node {
		return deny("vm %s on %s is not a worker of this cluster", vmid, node)
	}
	return nil
}

// store checks that s is one of the cluster's stores and, when node is set, that it is active there. It reports whether
// the store is shared.
func (h *Handler) store(ctx context.Context, c *call, node, s string) (shared bool, err error) {
	if !slices.Contains(c.cred.Storages, s) {
		return false, deny("storage %s is not offered to this cluster", s)
	}
	list, err := h.storages(ctx, c.cred.ComputeServiceID)
	if err != nil {
		return false, err
	}
	active := node == ""
	for _, r := range list {
		if r.Storage != s {
			continue
		}
		shared = shared || r.Shared == 1
		active = active || (r.Node == node && r.Status == "available")
	}
	if !active {
		return false, deny("storage %s is not active on %s", s, node)
	}
	return shared, nil
}

func filterList(raw json.RawMessage, keep func(map[string]any) bool) (json.RawMessage, error) {
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if keep(e) {
			out = append(out, e)
		}
	}
	return json.Marshal(out)
}

// #1 GET /cluster/resources?type=node|storage|vm. Nodes as they are, stores of the cluster, workers of the cluster.
func (h *Handler) resources(ctx context.Context, c *call) (json.RawMessage, error) {
	if err := c.only("type"); err != nil {
		return nil, err
	}
	typ := c.params.Get("type")
	var keep func(map[string]any) bool
	switch typ {
	case "node":
	case "storage":
		keep = func(e map[string]any) bool { s, _ := e["storage"].(string); return slices.Contains(c.cred.Storages, s) }
	case "vm":
		ws, err := h.workers(ctx, c.cred)
		if err != nil {
			return nil, err
		}
		keep = func(e map[string]any) bool {
			id, _ := e["vmid"].(float64)
			_, ok := ws[int(id)]
			return ok && e["type"] == "qemu"
		}
	default:
		return nil, deny("type must be node, storage or vm")
	}
	raw, err := c.gw.Raw(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {typ}})
	if err != nil || keep == nil {
		return raw, err
	}
	return filterList(raw, keep)
}

// #2 GET /storage/{s}
func (h *Handler) storageConfig(ctx context.Context, c *call) (json.RawMessage, error) {
	if err := c.only(); err != nil {
		return nil, err
	}
	if _, err := h.store(ctx, c, "", c.seg[1]); err != nil {
		return nil, err
	}
	return c.gw.Raw(ctx, http.MethodGet, c.path(), nil)
}

// #3 GET /nodes/{n}/storage/{s}/status
func (h *Handler) storageStatus(ctx context.Context, c *call) (json.RawMessage, error) {
	if err := c.only(); err != nil {
		return nil, err
	}
	if _, err := h.store(ctx, c, c.seg[1], c.seg[3]); err != nil {
		return nil, err
	}
	return c.gw.Raw(ctx, http.MethodGet, c.path(), nil)
}

// #4 GET /nodes/{n}/storage/{s}/content: only the cluster's own disks, so no other tenant's name shows.
func (h *Handler) contentList(ctx context.Context, c *call) (json.RawMessage, error) {
	if err := c.only(); err != nil {
		return nil, err
	}
	s := c.seg[3]
	if _, err := h.store(ctx, c, c.seg[1], s); err != nil {
		return nil, err
	}
	rows, err := h.Reg.ClusterVolumes(ctx, c.cred.ID, c.cred.ComputeServiceID, s)
	if err != nil {
		return nil, err
	}
	own := map[string]bool{}
	for _, v := range rows {
		own[v.VolID] = true
	}
	raw, err := c.gw.Raw(ctx, http.MethodGet, c.path(), nil)
	if err != nil {
		return nil, err
	}
	return filterList(raw, func(e map[string]any) bool { v, _ := e["volid"].(string); return own[v] })
}

// --- disks ---

var (
	uuidPart = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
	sizeRe   = regexp.MustCompile(`^([0-9]{1,12})([KMGT]?)$`)
)

// diskName is what the plugin may call a disk of this cluster: vm-<controllerVmId>-pvc-<uuid> for a volume,
// vm-<controllerVmId>-snapshot-<uuid> for a snapshot, .raw/.qcow2 on file-based stores.
func diskName(cvmid int, kinds string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`^vm-%d-(%s)-%s(\.raw|\.qcow2)?$`, cvmid, kinds, uuidPart))
}

// sizeBytes reads PVE's size syntax: a number of KiB, or with a K, M, G or T suffix.
func sizeBytes(s string) (int64, bool) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	shift := map[string]uint{"": 10, "K": 10, "M": 20, "G": 30, "T": 40}[m[2]]
	if n > (int64(1)<<62)>>shift {
		return 0, false
	}
	return n << shift, true
}

func gibUp(b int64) int { return int((b + gib - 1) / gib) }

// baseName drops the "<vmid>/" directory a file-based store puts a disk under. Only the controller's vmid may be there.
func baseName(cvmid int, disk string) (string, bool) {
	dir, name, ok := strings.Cut(disk, "/")
	if !ok {
		return disk, true
	}
	return name, dir == strconv.Itoa(cvmid) && name != "" && !strings.Contains(name, "/")
}

// owned returns the cluster's row for volid ("<s>:<disk>") in one of states, or a refusal.
func (h *Handler) owned(ctx context.Context, c *call, volid string, states ...string) (*Volume, error) {
	s, disk, ok := strings.Cut(volid, ":")
	if !ok || !slices.Contains(c.cred.Storages, s) {
		return nil, deny("volume %s is not owned by this cluster", volid)
	}
	name, ok := baseName(c.cred.ControllerVMID, disk)
	if !ok {
		return nil, deny("volume %s is not owned by this cluster", volid)
	}
	v, err := h.Reg.Volume(ctx, VolumeID(c.cred.ComputeServiceID, s, name))
	if err != nil {
		return nil, err
	}
	if v == nil || v.ClusterID != c.cred.ID || v.VolID != volid || !slices.Contains(states, v.State) {
		return nil, deny("volume %s is not owned by this cluster", volid)
	}
	return v, nil
}

// onNode checks the node a call on a disk names: the disk's own on a local store, one where the store is active on a
// shared one.
func (h *Handler) onNode(ctx context.Context, c *call, v *Volume, node string) error {
	shared, err := h.store(ctx, c, node, v.Storage)
	if err != nil {
		return err
	}
	if !shared && node != v.Node {
		return deny("volume %s is on %s, not %s", v.VolID, v.Node, node)
	}
	return nil
}

func (h *Handler) quota(ctx context.Context, c *call, addGiB int) error {
	if h.Quota == nil || addGiB <= 0 {
		return nil
	}
	err := h.Quota(ctx, c.cred.ProjectID, c.cred.ComputeServiceID, addGiB, func() (int, error) {
		n, err := h.Reg.ProjectBytes(ctx, c.cred.ProjectID, c.cred.ComputeServiceID)
		return gibUp(n), err
	})
	var he *httpx.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusConflict {
		return conflict("%s", he.Msg)
	}
	return err
}

// reserve runs fn, a quota check and the registry write it allows, under the project's lock, so concurrent creates,
// copies and resizes cannot each pass on the same usage.
func (h *Handler) reserve(ctx context.Context, c *call, fn func(ctx context.Context) error) error {
	return h.Reg.Locked(ctx, "csigw-quota:"+c.cred.ComputeServiceID+"/"+c.cred.ProjectID, fn)
}

// onPVE reports whether store s on node already holds a disk called name, under any vmid directory.
func (h *Handler) onPVE(ctx context.Context, c *call, node, s, name string) (string, bool, error) {
	raw, err := c.gw.Raw(ctx, http.MethodGet, "/nodes/"+node+"/storage/"+s+"/content", nil)
	if err != nil {
		return "", false, err
	}
	var list []struct {
		VolID string `json:"volid"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", false, err
	}
	for _, e := range list {
		_, disk, _ := strings.Cut(e.VolID, ":")
		if disk[strings.LastIndex(disk, "/")+1:] == name {
			return e.VolID, true, nil
		}
	}
	return "", false, nil
}

// #5 POST /nodes/{n}/storage/{s}/content: a new disk. Its row goes in first, so a name taken by anyone is refused and two
// creates of one name cannot both reach PVE.
func (h *Handler) contentCreate(ctx context.Context, c *call) (json.RawMessage, error) {
	ctx = context.WithoutCancel(ctx)
	n, s := c.seg[1], c.seg[3]
	if err := c.only("filename", "vmid", "size"); err != nil {
		return nil, err
	}
	if err := c.need("filename", "vmid", "size"); err != nil {
		return nil, err
	}
	if c.params.Get("vmid") != strconv.Itoa(c.cred.ControllerVMID) {
		return nil, deny("vmid must be %d", c.cred.ControllerVMID)
	}
	name := c.params.Get("filename")
	if !diskName(c.cred.ControllerVMID, "pvc").MatchString(name) {
		return nil, deny("filename %s is not a volume name of this cluster", name)
	}
	size, ok := sizeBytes(c.params.Get("size"))
	if !ok {
		return nil, deny("size %s is not a size", c.params.Get("size"))
	}
	if _, err := h.store(ctx, c, n, s); err != nil {
		return nil, err
	}
	id := VolumeID(c.cred.ComputeServiceID, s, name)
	if v, err := h.Reg.Volume(ctx, id); err != nil {
		return nil, err
	} else if v != nil {
		return h.existing(v, c)
	}
	if err := h.unused(ctx, c, n, s, name); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	v := &Volume{ID: id, ClusterID: c.cred.ID, ProjectID: c.cred.ProjectID, ComputeServiceID: c.cred.ComputeServiceID,
		Storage: s, Node: n, VolID: s + ":" + name, SizeBytes: size, State: StateCreating, CreatedAt: &now}
	if err := h.reserve(ctx, c, func(ctx context.Context) error {
		if err := h.quota(ctx, c, gibUp(size)); err != nil {
			return err
		}
		return h.Reg.InsertVolume(ctx, v)
	}); errors.Is(err, ErrExists) {
		return nil, conflict("volume %s is being created", name)
	} else if err != nil {
		return nil, err
	}
	raw, err := c.gw.Raw(ctx, http.MethodPost, c.path(), c.params)
	if err != nil {
		h.createFailed(ctx, c, v, name)
		return nil, err
	}
	var volid string
	_ = json.Unmarshal(raw, &volid)
	if !strings.HasPrefix(volid, s+":") {
		volid = s + ":" + name
	}
	return raw, h.Reg.UpdateVolume(ctx, id, pgdoc.M{"state": StateReady, "volid": volid})
}

// unused refuses a name the store on node already has a disk under. Such a disk has no row (the row was looked up
// first): an older disk, another cluster's from before the gateway, which a create or copy must not take over.
func (h *Handler) unused(ctx context.Context, c *call, node, s, name string) error {
	if _, found, err := h.onPVE(ctx, c, node, s, name); err != nil {
		return err
	} else if found {
		return conflict("volume name is taken")
	}
	return nil
}

// createFailed settles the row of a create PVE did not confirm. The name was unused before, so a disk under it now is
// this create's: the row turns ready and the plugin's retry gets it. No disk, the row goes. No answer from PVE either,
// the row stays creating for the registry reconciler, so a disk is never left without a row.
func (h *Handler) createFailed(ctx context.Context, c *call, v *Volume, name string) {
	volid, found, err := h.onPVE(ctx, c, v.Node, v.Storage, name)
	switch {
	case err != nil:
		h.log().Error("csi gateway: create not confirmed, row kept creating", "volume", v.ID, "err", err)
		return
	case found:
		err = h.Reg.UpdateVolume(ctx, v.ID, pgdoc.M{"state": StateReady, "volid": volid})
	default:
		err = h.Reg.DeleteVolume(ctx, v.ID)
	}
	if err != nil {
		h.log().Error("csi gateway: row of a failed create", "volume", v.ID, "err", err)
	}
}

// existing answers a create of a name with a row: the same cluster's ready disk again is the plugin retrying after a lost
// answer, anything else is refused.
func (h *Handler) existing(v *Volume, c *call) (json.RawMessage, error) {
	if v.ClusterID == c.cred.ID && v.State == StateReady {
		return json.Marshal(v.VolID)
	}
	if v.ClusterID != c.cred.ID {
		return nil, conflict("volume name is taken")
	}
	return nil, conflict("volume %s is %s", v.VolID, v.State)
}

func diskOf(c *call) string { return strings.Join(c.seg[5:], "/") }

// #6 POST /nodes/{n}/storage/{s}/content/{disk}: a copy (clone or snapshot) of an owned disk into a new name of the cluster.
func (h *Handler) contentCopy(ctx context.Context, c *call) (json.RawMessage, error) {
	ctx = context.WithoutCancel(ctx)
	n, s := c.seg[1], c.seg[3]
	if err := c.only("target", "target_node"); err != nil {
		return nil, err
	}
	if err := c.need("target"); err != nil {
		return nil, err
	}
	src, err := h.owned(ctx, c, s+":"+diskOf(c), StateReady)
	if err != nil {
		return nil, err
	}
	if err := h.onNode(ctx, c, src, n); err != nil {
		return nil, err
	}
	target := c.params.Get("target")
	name, ok := baseName(c.cred.ControllerVMID, target)
	if !ok || !diskName(c.cred.ControllerVMID, "pvc|snapshot").MatchString(name) {
		return nil, deny("target %s is not a volume name of this cluster", target)
	}
	node := n
	if tn := c.params.Get("target_node"); tn != "" {
		if _, err := h.store(ctx, c, tn, s); err != nil {
			return nil, err
		}
		node = tn
	}
	id := VolumeID(c.cred.ComputeServiceID, s, name)
	if v, err := h.Reg.Volume(ctx, id); err != nil {
		return nil, err
	} else if v != nil {
		return nil, conflict("volume name is taken")
	}
	if err := h.unused(ctx, c, node, s, name); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	v := &Volume{ID: id, ClusterID: c.cred.ID, ProjectID: c.cred.ProjectID, ComputeServiceID: c.cred.ComputeServiceID,
		Storage: s, Node: node, VolID: s + ":" + target, SizeBytes: src.SizeBytes, SourceID: src.ID,
		State: StateCreating, CreatedAt: &now}
	if err := h.reserve(ctx, c, func(ctx context.Context) error {
		if err := h.quota(ctx, c, gibUp(src.SizeBytes)); err != nil {
			return err
		}
		return h.Reg.InsertVolume(ctx, v)
	}); errors.Is(err, ErrExists) {
		return nil, conflict("volume name is taken")
	} else if err != nil {
		return nil, err
	}
	raw, err := c.gw.Raw(ctx, http.MethodPost, c.path(), c.params)
	var ae *pve.APIError
	if errors.As(err, &ae) {
		// PVE refused it, so no task runs: the name is free again.
		if derr := h.Reg.DeleteVolume(ctx, id); derr != nil {
			h.log().Error("csi gateway: row of a refused copy left", "volume", id, "err", derr)
		}
		return nil, err
	} else if err != nil {
		// A lost answer: the task may be writing the disk. The row stays creating for the reconciler.
		return nil, err
	}
	// ponytail: a copy task that fails leaves the row creating; the registry reconciler drops it once PVE has no disk.
	return raw, h.task(ctx, c, raw, "copy", id, 0)
}

// #7 DELETE /nodes/{n}/storage/{s}/content/{disk}
func (h *Handler) contentDelete(ctx context.Context, c *call) (json.RawMessage, error) {
	ctx = context.WithoutCancel(ctx)
	n, s := c.seg[1], c.seg[3]
	if err := c.only("delay"); err != nil {
		return nil, err
	}
	if d := c.params.Get("delay"); d != "" {
		if i, err := strconv.Atoi(d); err != nil || i < 1 || i > 30 {
			return nil, deny("delay must be 1 to 30")
		}
	}
	v, err := h.owned(ctx, c, s+":"+diskOf(c), StateReady, StateDeleting)
	if err != nil {
		return nil, err
	}
	if err := h.onNode(ctx, c, v, n); err != nil {
		return nil, err
	}
	if err := h.Reg.UpdateVolume(ctx, v.ID, pgdoc.M{"state": StateDeleting}); err != nil {
		return nil, err
	}
	raw, err := c.gw.Raw(ctx, http.MethodDelete, c.path(), c.params)
	if err != nil {
		state := StateReady
		if pve.IsNotFound(err) {
			state = StateDeleted
		}
		if uerr := h.setState(ctx, v.ID, state); uerr != nil {
			h.log().Error("csi gateway: volume state after a failed delete", "volume", v.ID, "err", uerr)
		}
		return nil, err
	}
	var upid string
	if _ = json.Unmarshal(raw, &upid); upid == "" {
		return raw, h.setState(ctx, v.ID, StateDeleted)
	}
	return raw, h.task(ctx, c, raw, "delete", v.ID, 0)
}

func (h *Handler) setState(ctx context.Context, id, state string) error {
	set := pgdoc.M{"state": state}
	if state == StateDeleted {
		set["deletedAt"] = time.Now().UTC()
	}
	return h.Reg.UpdateVolume(ctx, id, set)
}

// task records the UPID an answer carries, so the cluster may poll it and the gateway can apply its result.
func (h *Handler) task(ctx context.Context, c *call, raw json.RawMessage, kind, volumeID string, size int64) error {
	var upid string
	if json.Unmarshal(raw, &upid) != nil || upid == "" {
		return nil
	}
	return h.Reg.PutTask(ctx, &Task{ID: upid, ClusterID: c.cred.ID, Kind: kind, VolumeID: volumeID, SizeBytes: size,
		ExpiresAt: time.Now().UTC().Add(taskTTL)})
}

// --- VMs ---

// #8 GET /nodes/{n}/qemu/{vmid}/config[?current=1]
func (h *Handler) vmConfig(ctx context.Context, c *call) (json.RawMessage, error) {
	if err := c.only("current"); err != nil {
		return nil, err
	}
	if cur := c.params.Get("current"); cur != "" && cur != "0" && cur != "1" {
		return nil, deny("current must be 0 or 1")
	}
	if err := h.worker(ctx, c, c.seg[1], c.seg[3]); err != nil {
		return nil, err
	}
	raw, err := c.gw.Raw(ctx, http.MethodGet, c.path(), c.params)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if err := tagged(c, cfg); err != nil {
		return nil, err
	}
	return raw, nil
}

// tagged checks the cluster's tag again on the config just read, so a VM that took a worker's vmid between the worker
// list and this read is still refused.
func tagged(c *call, cfg map[string]any) error {
	tags, _ := cfg["tags"].(string)
	if !hasTag(tags, "pc-k8s-"+c.cred.ID) {
		return deny("vm %s is not a worker of this cluster", c.seg[3])
	}
	return nil
}

var slotRe = regexp.MustCompile(`^scsi([1-9]|1[0-9]|2[0-9])$`)

// driveOptions is every option the plugin writes on a disk it attaches, with the values it may take. replicate is
// always sent and only "0" passes: a disk of a cluster never joins a replication job.
var driveOptions = map[string]*regexp.Regexp{
	"wwn":       regexp.MustCompile(`^0x[0-9a-fA-F]{16}$`),
	"backup":    regexp.MustCompile(`^[01]$`),
	"iothread":  regexp.MustCompile(`^[01]$`),
	"ssd":       regexp.MustCompile(`^[01]$`),
	"ro":        regexp.MustCompile(`^[01]$`),
	"replicate": regexp.MustCompile(`^0$`),
	"cache":     regexp.MustCompile(`^(none|writethrough|writeback|unsafe|directsync)$`),
	"discard":   regexp.MustCompile(`^(on|ignore)$`),
	"aio":       regexp.MustCompile(`^(native|threads|io_uring)$`),
	"iops":      regexp.MustCompile(`^[0-9]{1,9}$`),
	"iops_rd":   regexp.MustCompile(`^[0-9]{1,9}$`),
	"iops_wr":   regexp.MustCompile(`^[0-9]{1,9}$`),
	"mbps":      regexp.MustCompile(`^[0-9]{1,9}$`),
	"mbps_rd":   regexp.MustCompile(`^[0-9]{1,9}$`),
	"mbps_wr":   regexp.MustCompile(`^[0-9]{1,9}$`),
	// What PVE reports back on a drive; the plugin sends it again when it rewrites an attached disk (wwn, options).
	"size": regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]+)?[KMGT]?$`),
}

// driveFile parses a scsiN value and returns its volume, refusing any option outside driveOptions.
func driveFile(value string) (string, error) {
	file, seen := "", map[string]bool{}
	for i, part := range strings.Split(value, ",") {
		k, v, hasEq := strings.Cut(part, "=")
		if !hasEq && i == 0 {
			k, v = "file", part
		}
		if seen[k] {
			return "", deny("drive option %s given twice", k)
		}
		seen[k] = true
		if k == "file" {
			file = v
			continue
		}
		re, ok := driveOptions[k]
		if !ok || !re.MatchString(v) {
			return "", deny("drive option %q is not allowed", part)
		}
	}
	if file == "" {
		return "", deny("drive has no volume")
	}
	return file, nil
}

// slotFile is the volume a config holds on key ("" when the slot is empty).
func slotFile(cfg map[string]any, key string) string {
	v, _ := cfg[key].(string)
	file, _, _ := strings.Cut(v, ",")
	return strings.TrimPrefix(file, "file=")
}

func (h *Handler) vmConfigs(ctx context.Context, c *call) (current, pending map[string]any, err error) {
	base := "/nodes/" + c.seg[1] + "/qemu/" + c.seg[3] + "/config"
	for _, q := range []struct {
		dst  *map[string]any
		form url.Values
	}{{&current, url.Values{"current": {"1"}}}, {&pending, nil}} {
		raw, err := c.gw.Raw(ctx, http.MethodGet, base, q.form)
		if err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(raw, q.dst); err != nil {
			return nil, nil, err
		}
		if err := tagged(c, *q.dst); err != nil {
			return nil, nil, err
		}
	}
	return current, pending, nil
}

// #9 POST /nodes/{n}/qemu/{vmid}/config: attach one owned disk on scsi1..29, or rewrite the options of the disk already
// there. No other key: not scsi0, ide*, virtio*, args, hostpci*.
func (h *Handler) vmAttach(ctx context.Context, c *call) (json.RawMessage, error) {
	ctx = context.WithoutCancel(ctx)
	if len(c.params) != 1 {
		return nil, deny("exactly one scsi1..scsi29 key")
	}
	var slot string
	for k := range c.params {
		slot = k
	}
	if !slotRe.MatchString(slot) {
		return nil, deny("key %s is not allowed", slot)
	}
	if err := h.worker(ctx, c, c.seg[1], c.seg[3]); err != nil {
		return nil, err
	}
	file, err := driveFile(c.params.Get(slot))
	if err != nil {
		return nil, err
	}
	if _, err := h.owned(ctx, c, file, StateReady); err != nil {
		return nil, err
	}
	current, pending, err := h.vmConfigs(ctx, c)
	if err != nil {
		return nil, err
	}
	for _, cfg := range []map[string]any{current, pending} {
		if f := slotFile(cfg, slot); f != "" && f != file {
			return nil, deny("%s holds another disk", slot)
		}
	}
	raw, err := c.gw.Raw(ctx, http.MethodPost, c.path(), c.params)
	if err != nil {
		return nil, err
	}
	return raw, h.task(ctx, c, raw, "attach", "", 0)
}

// #10 PUT /nodes/{n}/qemu/{vmid}/config with revert=scsiN[,…] only: drops the pending half of a failed hotplug.
func (h *Handler) vmRevert(ctx context.Context, c *call) (json.RawMessage, error) {
	if err := c.only("revert"); err != nil {
		return nil, err
	}
	if err := c.need("revert"); err != nil {
		return nil, err
	}
	for _, k := range strings.Split(c.params.Get("revert"), ",") {
		if !slotRe.MatchString(k) {
			return nil, deny("revert %s is not allowed", k)
		}
	}
	if err := h.worker(ctx, c, c.seg[1], c.seg[3]); err != nil {
		return nil, err
	}
	return c.gw.Raw(context.WithoutCancel(ctx), http.MethodPut, c.path(), c.params)
}

// ownedSlot is the cluster's disk on a worker's scsiN, from its current config: what unlink and resize may touch.
func (h *Handler) ownedSlot(ctx context.Context, c *call, slot string, states ...string) (*Volume, error) {
	if !slotRe.MatchString(slot) {
		return nil, deny("disk %s is not allowed", slot)
	}
	if err := h.worker(ctx, c, c.seg[1], c.seg[3]); err != nil {
		return nil, err
	}
	current, _, err := h.vmConfigs(ctx, c)
	if err != nil {
		return nil, err
	}
	file := slotFile(current, slot)
	if file == "" {
		return nil, deny("%s holds no disk", slot)
	}
	return h.owned(ctx, c, file, states...)
}

// #11 PUT /nodes/{n}/qemu/{vmid}/unlink idlist=scsiN: detach. The disk on the slot must be the cluster's, so the boot
// disk or another tenant's cannot be taken off; force (physical removal) is refused.
func (h *Handler) vmUnlink(ctx context.Context, c *call) (json.RawMessage, error) {
	if err := c.only("idlist"); err != nil {
		return nil, err
	}
	if _, err := h.ownedSlot(ctx, c, c.params.Get("idlist"), StateReady, StateDeleting); err != nil {
		return nil, err
	}
	return c.gw.Raw(context.WithoutCancel(ctx), http.MethodPut, c.path(), c.params)
}

// #12 PUT /nodes/{n}/qemu/{vmid}/resize disk=scsiN size=<abs>: grow an owned, attached disk.
func (h *Handler) vmResize(ctx context.Context, c *call) (json.RawMessage, error) {
	ctx = context.WithoutCancel(ctx)
	if err := c.only("disk", "size"); err != nil {
		return nil, err
	}
	if err := c.need("disk", "size"); err != nil {
		return nil, err
	}
	size, ok := sizeBytes(c.params.Get("size"))
	if !ok {
		return nil, deny("size %s is not an absolute size", c.params.Get("size"))
	}
	v, err := h.ownedSlot(ctx, c, c.params.Get("disk"), StateReady)
	if err != nil {
		return nil, err
	}
	// The row takes the new size as the call is accepted, not when a task poll comes (which the cluster may skip), so
	// quota and billing count it at once. Resize only grows; a smaller size is PVE's to refuse.
	var before int64
	if err := h.reserve(ctx, c, func(ctx context.Context) error {
		cur, err := h.Reg.Volume(ctx, v.ID)
		if err != nil {
			return err
		}
		if cur == nil {
			return deny("volume %s is not owned by this cluster", v.VolID)
		}
		if before = cur.SizeBytes; size <= before {
			return nil
		}
		if err := h.quota(ctx, c, gibUp(size)-gibUp(before)); err != nil {
			return err
		}
		return h.Reg.UpdateVolume(ctx, v.ID, pgdoc.M{"sizeBytes": size})
	}); err != nil {
		return nil, err
	}
	raw, err := c.gw.Raw(ctx, http.MethodPut, c.path(), c.params)
	var ae *pve.APIError
	if errors.As(err, &ae) && size > before {
		// Refused: the disk kept its size. Any other failure leaves the larger size for the reconciler to correct.
		if uerr := h.Reg.UpdateVolume(ctx, v.ID, pgdoc.M{"sizeBytes": before}); uerr != nil {
			h.log().Error("csi gateway: size of a refused resize", "volume", v.ID, "err", uerr)
		}
	}
	if err != nil {
		return nil, err
	}
	return raw, h.task(ctx, c, raw, "resize", v.ID, size)
}

// #13 GET /nodes/{n}/tasks/{upid}/status: only a UPID the gateway answered this cluster with. A finished task's result
// lands on its row once.
func (h *Handler) taskStatus(ctx context.Context, c *call) (json.RawMessage, error) {
	if err := c.only(); err != nil {
		return nil, err
	}
	upid := c.seg[3]
	t, err := h.Reg.Task(ctx, upid)
	if err != nil {
		return nil, err
	}
	if t == nil || t.ClusterID != c.cred.ID || time.Now().After(t.ExpiresAt) || !strings.HasPrefix(upid, "UPID:"+c.seg[1]+":") {
		return nil, deny("task is not one of this cluster's")
	}
	raw, err := c.gw.Raw(ctx, http.MethodGet, c.path(), nil)
	if err != nil {
		return nil, err
	}
	var st struct {
		Status     string `json:"status"`
		ExitStatus string `json:"exitstatus"`
	}
	if json.Unmarshal(raw, &st) == nil && st.Status == "stopped" && t.VolumeID != "" {
		if err := h.finish(context.WithoutCancel(ctx), t, st.ExitStatus == "OK"); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func (h *Handler) finish(ctx context.Context, t *Task, ok bool) error {
	first, err := h.Reg.FinishTask(ctx, t.ID)
	if err != nil || !first {
		return err
	}
	switch {
	case t.Kind == "delete" && ok:
		return h.setState(ctx, t.VolumeID, StateDeleted)
	case t.Kind == "delete":
		return h.setState(ctx, t.VolumeID, StateReady)
	case t.Kind == "copy" && ok:
		return h.setState(ctx, t.VolumeID, StateReady)
	case t.Kind == "resize" && ok:
		return h.Reg.UpdateVolume(ctx, t.VolumeID, pgdoc.M{"sizeBytes": t.SizeBytes})
	}
	return nil
}

// ErrNoGatewayToken is a compute provider without config.csiGatewayTokenId and secret.csiGatewaySecret.
var ErrNoGatewayToken = errors.New("csigw: the compute provider has no CSI gateway token")

// ProviderClients builds Handler.PVE and Handler.Portal from the compute provider's document.
func ProviderClients(get func(ctx context.Context, id string) (*externalservice.ExternalService, error)) (gateway, portal func(context.Context, string) (*pve.Client, error)) {
	gateway = func(ctx context.Context, id string) (*pve.Client, error) {
		es, err := get(ctx, id)
		if err != nil {
			return nil, err
		}
		cfg, ok := es.PVECSIGatewayConfig()
		if !ok {
			return nil, ErrNoGatewayToken
		}
		return pve.New(cfg)
	}
	portal = func(ctx context.Context, id string) (*pve.Client, error) {
		es, err := get(ctx, id)
		if err != nil {
			return nil, err
		}
		return pve.New(es.PVEConfig())
	}
	return gateway, portal
}
