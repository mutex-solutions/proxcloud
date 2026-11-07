package kamajik8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// API paths for the kinds this client touches. Group/version pins:
//   - argoproj.io/v1alpha1 Application (ArgoCD — stable since 2018, no v1 planned)
//   - kamaji.clastix.io/v1alpha1 TenantControlPlane
//   - cluster.x-k8s.io/v1beta2 Cluster API (MachineDeployment)
const (
	pathApplications        = "/apis/argoproj.io/v1alpha1/namespaces/%s/applications"
	pathTenantControlPlanes = "/apis/kamaji.clastix.io/v1alpha1/namespaces/%s/tenantcontrolplanes"
	pathMachineDeployments  = "/apis/cluster.x-k8s.io/v1beta2/namespaces/%s/machinedeployments"
	pathSecrets             = "/api/v1/namespaces/%s/secrets"
	pathServices            = "/api/v1/namespaces/%s/services"
	pathNetworkPolicies     = "/apis/networking.k8s.io/v1/namespaces/%s/networkpolicies"
	pathVPAs                = "/apis/autoscaling.k8s.io/v1/namespaces/%s/verticalpodautoscalers"
	pathNamespaces          = "/api/v1/namespaces"
	fieldManager            = "proxcloud"
)

// Client is the minimal management-cluster API client. Safe for concurrent use.
type Client struct {
	rc   *restConfig
	http *http.Client
}

// New builds a Client from a raw kubeconfig (the provider secret's `kubeconfig` field).
func New(kubeconfigYAML string) (*Client, error) {
	rc, err := parseKubeconfig([]byte(kubeconfigYAML))
	if err != nil {
		return nil, err
	}
	return &Client{rc: rc, http: rc.httpClient()}, nil
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// NewInCluster builds a Client from the pod's service account (token + CA) and
// KUBERNETES_SERVICE_HOST/PORT. The token file is re-read periodically.
func NewInCluster() (*Client, error) {
	return newInCluster(saDir, os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT"))
}

func newInCluster(dir, host, port string) (*Client, error) {
	if host == "" || port == "" {
		return nil, errors.New("in-cluster: KUBERNETES_SERVICE_HOST/PORT not set")
	}
	ca, err := os.ReadFile(dir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("in-cluster: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("in-cluster: ca.crt has no PEM certs")
	}
	rc := &restConfig{
		server:    "https://" + net.JoinHostPort(host, port),
		tlsCfg:    &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		timeout:   30 * time.Second,
		tokenFile: dir + "/token",
	}
	if rc.bearer() == "" {
		return nil, errors.New("in-cluster: empty service account token")
	}
	return &Client{rc: rc, http: rc.httpClient()}, nil
}

// ErrUnreachable wraps transport failures (dial, TLS, timeout): the API server never answered.
// Callers map it to "service unavailable" instead of an internal error.
var ErrUnreachable = errors.New("kubernetes api unreachable")

// NotFound reports whether err is a Kubernetes 404 (returned as *APIError).
func NotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// APIError is a non-2xx Kubernetes API response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("kubernetes api: %d: %s", e.Status, e.Message) }

// do runs one API call. body != nil is JSON-encoded. contentType overrides the default
// application/json (server-side apply needs application/apply-patch+yaml — JSON is valid YAML,
// so the payload stays JSON). 2xx with a body decodes into map[string]any; 404 on GET/DELETE
// returns (nil, *APIError{404}).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, contentType string) (map[string]any, error) {
	u := strings.TrimRight(c.rc.server, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("kubernetes api: encode body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	if t := c.rc.bearer(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg := string(raw)
		var status struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &status) == nil && status.Message != "" {
			msg = status.Message
		}
		return nil, &APIError{Status: res.StatusCode, Message: msg}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("kubernetes api: decode response: %w", err)
	}
	return out, nil
}

// apply server-side-applies obj (which must carry apiVersion/kind/metadata.name) at path/{name}.
// force=true takes field ownership from any previous manager — proxcloud is the single writer for
// everything it applies, so conflicts always resolve our way.
func (c *Client) apply(ctx context.Context, path, name string, obj map[string]any) (map[string]any, error) {
	q := url.Values{"fieldManager": {fieldManager}, "force": {"true"}}
	return c.do(ctx, http.MethodPatch, path+"/"+name, q, obj, "application/apply-patch+yaml")
}

// get returns the object, or (nil, nil) when it does not exist.
func (c *Client) get(ctx context.Context, path, name string) (map[string]any, error) {
	out, err := c.do(ctx, http.MethodGet, path+"/"+name, nil, nil, "")
	if NotFound(err) {
		return nil, nil
	}
	return out, err
}

// list returns .items of a collection GET (optionally label-filtered).
func (c *Client) list(ctx context.Context, path, labelSelector string) ([]map[string]any, error) {
	var q url.Values
	if labelSelector != "" {
		q = url.Values{"labelSelector": {labelSelector}}
	}
	out, err := c.do(ctx, http.MethodGet, path, q, nil, "")
	if err != nil {
		return nil, err
	}
	items, _ := out["items"].([]any)
	res := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			res = append(res, m)
		}
	}
	return res, nil
}

// delete removes the object; an already-absent object is success (idempotent delete).
func (c *Client) delete(ctx context.Context, path, name string) error {
	_, err := c.do(ctx, http.MethodDelete, path+"/"+name, nil, nil, "")
	if NotFound(err) {
		return nil
	}
	return err
}

// EnsureNamespace applies the namespace with the given labels (create-or-update).
func (c *Client) EnsureNamespace(ctx context.Context, name string, labels map[string]string) error {
	_, err := c.apply(ctx, pathNamespaces, name, map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": name, "labels": toAny(labels)},
	})
	return err
}

// GetNamespace returns the namespace object, or nil when absent.
func (c *Client) GetNamespace(ctx context.Context, name string) (map[string]any, error) {
	return c.get(ctx, pathNamespaces, name)
}

// DeleteNamespace removes the namespace (absent = success). Kubernetes finalizes the delete
// asynchronously, cascading to everything still inside.
func (c *Client) DeleteNamespace(ctx context.Context, name string) error {
	return c.delete(ctx, pathNamespaces, name)
}

// ApplySecret applies an Opaque secret with stringData (create-or-update). annotations may be nil.
func (c *Client) ApplySecret(ctx context.Context, ns, name string, stringData map[string]string, labels, annotations map[string]string) error {
	meta := map[string]any{"name": name, "namespace": ns, "labels": toAny(labels)}
	if len(annotations) > 0 {
		meta["annotations"] = toAny(annotations)
	}
	_, err := c.apply(ctx, fmt.Sprintf(pathSecrets, ns), name, map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   meta,
		"type":       "Opaque",
		"stringData": toAny(stringData),
	})
	return err
}

// ApplyResourceSetSecret writes a Secret a ClusterResourceSet may deliver into a workload cluster: the
// type is what CAPI insists on, and a Secret's type is immutable, so it cannot be a later patch.
func (c *Client) ApplyResourceSetSecret(ctx context.Context, ns, name string, stringData, labels map[string]string) error {
	_, err := c.apply(ctx, fmt.Sprintf(pathSecrets, ns), name, map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": ns, "labels": toAny(labels)},
		"type":       "addons.cluster.x-k8s.io/resource-set",
		"stringData": toAny(stringData),
	})
	return err
}

// ListSecrets lists secrets in ns filtered by labelSelector, as full objects (metadata incl.
// labels/annotations — the orphan-finalize scan reads names and timestamps off them).
func (c *Client) ListSecrets(ctx context.Context, ns, labelSelector string) ([]map[string]any, error) {
	return c.list(ctx, fmt.Sprintf(pathSecrets, ns), labelSelector)
}

// ListSecretsAllNamespaces lists label-matched secrets across EVERY namespace (GET
// /api/v1/secrets) — the service-level orphan sweep, which must find leftovers of projects
// whose proxcloud doc is already gone (scheduled deletion) and so cannot be enumerated per
// project. metadata.namespace on each item routes the follow-up calls.
func (c *Client) ListSecretsAllNamespaces(ctx context.Context, labelSelector string) ([]map[string]any, error) {
	return c.list(ctx, "/api/v1/secrets", labelSelector)
}

// GetSecretData returns the base64-decoded .data of a secret, or nil when absent.
func (c *Client) GetSecretData(ctx context.Context, ns, name string) (map[string][]byte, error) {
	obj, err := c.get(ctx, fmt.Sprintf(pathSecrets, ns), name)
	if err != nil || obj == nil {
		return nil, err
	}
	data, _ := obj["data"].(map[string]any)
	out := make(map[string][]byte, len(data))
	for k, v := range data {
		s, _ := v.(string)
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("secret %s/%s: decode %q: %w", ns, name, k, err)
		}
		out[k] = b
	}
	return out, nil
}

// AnnotateSecret adds/overwrites annotations on a secret, leaving everything else alone. A JSON
// merge-patch rather than an apply: the secret's data belongs to whoever created it (Kamaji, for
// the kubeconfig secrets proxcloud annotates to trigger a rotation) and must not change owner.
func (c *Client) AnnotateSecret(ctx context.Context, ns, name string, annotations map[string]string) error {
	patch := map[string]any{"metadata": map[string]any{"annotations": toAny(annotations)}}
	_, err := c.do(ctx, http.MethodPatch, fmt.Sprintf(pathSecrets, ns)+"/"+name, nil, patch, "application/merge-patch+json")
	return err
}

// PatchSecretData overwrites individual data keys on an EXISTING secret, leaving type, metadata
// and every other key alone — the AnnotateSecret discipline for payload: an SSA apply onto an
// operator-created secret would both fight its immutable `type` (CNPG mints basic-auth, ApplySecret
// hardcodes Opaque → 422) and retract labels this field manager applied earlier. A merge-patch
// touches only the named keys and never changes owner. 404 (secret not minted yet) surfaces as
// the *APIError so callers can say "not ready".
func (c *Client) PatchSecretData(ctx context.Context, ns, name string, stringData map[string]string) error {
	patch := map[string]any{"stringData": toAny(stringData)}
	_, err := c.do(ctx, http.MethodPatch, fmt.Sprintf(pathSecrets, ns)+"/"+name, nil, patch, "application/merge-patch+json")
	return err
}

// ApplyNetworkPolicy applies a networking.k8s.io/v1 NetworkPolicy (create-or-update; name and
// namespace from its metadata) — the dbaas namespace default-deny leg.
func (c *Client) ApplyNetworkPolicy(ctx context.Context, np map[string]any) error {
	meta, _ := np["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	ns, _ := meta["namespace"].(string)
	if name == "" || ns == "" {
		return fmt.Errorf("networkpolicy: metadata.name/namespace required")
	}
	_, err := c.apply(ctx, fmt.Sprintf(pathNetworkPolicies, ns), name, np)
	return err
}

// DeleteSecret removes a secret (absent = success).
func (c *Client) DeleteSecret(ctx context.Context, ns, name string) error {
	return c.delete(ctx, fmt.Sprintf(pathSecrets, ns), name)
}

// ApplyApplication applies an ArgoCD Application (create-or-update; name/namespace from its metadata).
func (c *Client) ApplyApplication(ctx context.Context, app map[string]any) error {
	meta, _ := app["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	ns, _ := meta["namespace"].(string)
	if name == "" || ns == "" {
		return fmt.Errorf("application: metadata.name/namespace required")
	}
	_, err := c.apply(ctx, fmt.Sprintf(pathApplications, ns), name, app)
	return err
}

// GetApplication returns the Application, or nil when absent.
func (c *Client) GetApplication(ctx context.Context, ns, name string) (map[string]any, error) {
	return c.get(ctx, fmt.Sprintf(pathApplications, ns), name)
}

// ListApplications lists Applications in ns filtered by labelSelector.
func (c *Client) ListApplications(ctx context.Context, ns, labelSelector string) ([]map[string]any, error) {
	return c.list(ctx, fmt.Sprintf(pathApplications, ns), labelSelector)
}

// DeleteApplication removes the Application. The Application carries ArgoCD's resources-finalizer
// (set at create), so ArgoCD cascades the delete to everything the chart rendered.
func (c *Client) DeleteApplication(ctx context.Context, ns, name string) error {
	return c.delete(ctx, fmt.Sprintf(pathApplications, ns), name)
}

// GetService returns a core/v1 Service, or nil when absent. Read-only: the dbaas provider reads
// the LB ingress IP back off .status.loadBalancer.ingress of chart-rendered LoadBalancer Services.
func (c *Client) GetService(ctx context.Context, ns, name string) (map[string]any, error) {
	return c.get(ctx, fmt.Sprintf(pathServices, ns), name)
}

// GetVPA returns an autoscaling.k8s.io/v1 VerticalPodAutoscaler, or nil when absent (also nil
// when the VPA CRD is not installed — the 404 shape is identical). Read-only: the dbaas
// autoscale tick consumes .status.recommendation; the VPA runs in updateMode Off and never
// mutates pods itself.
func (c *Client) GetVPA(ctx context.Context, ns, name string) (map[string]any, error) {
	return c.get(ctx, fmt.Sprintf(pathVPAs, ns), name)
}

// ListPods lists pods in ns filtered by labelSelector. Read-only: the dbaas GET_LOGS leg finds a
// database's pods with it.
func (c *Client) ListPods(ctx context.Context, ns, labelSelector string) ([]map[string]any, error) {
	return c.list(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods", ns), labelSelector)
}

// PodLogs returns the tail of one container's stdout. Bounded by design: tailLines caps what
// the apiserver sends (default 500, max 5000) and the response read is capped at 8 MiB, so a
// chatty database cannot stream unbounded output through the API into a browser. Returns raw
// text, not JSON — this subresource does not speak it.
func (c *Client) PodLogs(ctx context.Context, ns, pod, container string, tailLines int) (string, error) {
	if tailLines <= 0 || tailLines > 5000 {
		tailLines = 500
	}
	q := url.Values{}
	q.Set("container", container)
	q.Set("tailLines", strconv.Itoa(tailLines))
	q.Set("timestamps", "true")
	body, err := c.raw(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?%s", ns, pod, q.Encode()))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// raw GETs a path whose response is NOT JSON (pods/log is plain text). Same auth and transport
// as do(), and the same 8 MiB read cap. NO Accept header: the apiserver content-negotiates even
// on pods/log and answers `Accept: text/plain` with 406 — the body is plain text regardless.
func (c *Client) raw(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.rc.server, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	if t := c.rc.bearer(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return nil, &APIError{Status: res.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	return body, nil
}

// ListCRs lists any namespaced custom resource by group/version/plural, filtered by
// labelSelector. Read-only escape hatch for CRs that need no typed helper — the dbaas backup
// list, where the kind differs per engine. A missing CRD 404s the same way an empty list does,
// which is the right behaviour: an operator that is not installed simply has no backups.
func (c *Client) ListCRs(ctx context.Context, group, version, plural, ns, labelSelector string) ([]map[string]any, error) {
	items, err := c.list(ctx, crPath(group, version, plural, ns), labelSelector)
	if NotFound(err) {
		return nil, nil
	}
	return items, err
}

// crPath is the namespaced collection path of a custom resource.
func crPath(group, version, plural, ns string) string {
	return fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s", group, version, ns, plural)
}

// ApplyCR server-side-applies a namespaced custom resource (obj carries apiVersion, kind,
// metadata.name and metadata.namespace). Create-or-update, proxcloud as the single field owner.
func (c *Client) ApplyCR(ctx context.Context, group, version, plural string, obj map[string]any) error {
	name, ns := metaNameNS(obj)
	if name == "" || ns == "" {
		return fmt.Errorf("%s: metadata.name/namespace required", plural)
	}
	_, err := c.apply(ctx, crPath(group, version, plural, ns), name, obj)
	return err
}

// GetCR returns a namespaced custom resource, or nil when absent.
func (c *Client) GetCR(ctx context.Context, group, version, plural, ns, name string) (map[string]any, error) {
	return c.get(ctx, crPath(group, version, plural, ns), name)
}

// DeleteCR removes a namespaced custom resource (absent = success).
func (c *Client) DeleteCR(ctx context.Context, group, version, plural, ns, name string) error {
	return c.delete(ctx, crPath(group, version, plural, ns), name)
}

// MergePatchCR applies a JSON merge patch to a namespaced custom resource and returns it.
func (c *Client) MergePatchCR(ctx context.Context, group, version, plural, ns, name string, patch map[string]any) (map[string]any, error) {
	return c.do(ctx, http.MethodPatch, crPath(group, version, plural, ns)+"/"+name, nil, patch, "application/merge-patch+json")
}

// ListCRsAllNamespaces lists a custom resource across every namespace, filtered by labelSelector.
func (c *Client) ListCRsAllNamespaces(ctx context.Context, group, version, plural, labelSelector string) ([]map[string]any, error) {
	return c.list(ctx, fmt.Sprintf("/apis/%s/%s/%s", group, version, plural), labelSelector)
}

// ApplyResourceQuota server-side-applies a core/v1 ResourceQuota (create-or-update).
func (c *Client) ApplyResourceQuota(ctx context.Context, rq map[string]any) error {
	name, ns := metaNameNS(rq)
	if name == "" || ns == "" {
		return fmt.Errorf("resourcequota: metadata.name/namespace required")
	}
	_, err := c.apply(ctx, corePath("resourcequotas", ns), name, rq)
	return err
}

// corePath is the collection path of a namespaced core (v1) resource, e.g. pods, configmaps.
func corePath(plural, ns string) string { return fmt.Sprintf("/api/v1/namespaces/%s/%s", ns, plural) }

// ApplyCore server-side-applies a namespaced core/v1 object (Pod, ConfigMap, Secret …).
func (c *Client) ApplyCore(ctx context.Context, plural string, obj map[string]any) error {
	name, ns := metaNameNS(obj)
	if name == "" || ns == "" {
		return fmt.Errorf("%s: metadata.name/namespace required", plural)
	}
	_, err := c.apply(ctx, corePath(plural, ns), name, obj)
	return err
}

// GetCore returns a namespaced core/v1 object, or nil when absent.
func (c *Client) GetCore(ctx context.Context, plural, ns, name string) (map[string]any, error) {
	return c.get(ctx, corePath(plural, ns), name)
}

// DeleteCore removes a namespaced core/v1 object (absent = success).
func (c *Client) DeleteCore(ctx context.Context, plural, ns, name string) error {
	return c.delete(ctx, corePath(plural, ns), name)
}

// ListPodsAllNamespaces lists pods in every namespace filtered by labelSelector.
func (c *Client) ListPodsAllNamespaces(ctx context.Context, labelSelector string) ([]map[string]any, error) {
	return c.list(ctx, "/api/v1/pods", labelSelector)
}

// ListNodes lists the cluster's nodes.
func (c *Client) ListNodes(ctx context.Context) ([]map[string]any, error) {
	return c.list(ctx, "/api/v1/nodes", "")
}

func metaNameNS(obj map[string]any) (name, ns string) {
	meta, _ := obj["metadata"].(map[string]any)
	name, _ = meta["name"].(string)
	ns, _ = meta["namespace"].(string)
	return name, ns
}

// GetTenantControlPlane returns the Kamaji TCP, or nil when absent.
func (c *Client) GetTenantControlPlane(ctx context.Context, ns, name string) (map[string]any, error) {
	return c.get(ctx, fmt.Sprintf(pathTenantControlPlanes, ns), name)
}

// ListTenantControlPlanes lists TCPs in ns (labelSelector optional).
func (c *Client) ListTenantControlPlanes(ctx context.Context, ns, labelSelector string) ([]map[string]any, error) {
	return c.list(ctx, fmt.Sprintf(pathTenantControlPlanes, ns), labelSelector)
}

// ListMachineDeployments lists CAPI MachineDeployments in ns filtered by labelSelector
// (cluster.x-k8s.io/cluster-name=<cluster> scopes to one cluster's node groups).
func (c *Client) ListMachineDeployments(ctx context.Context, ns, labelSelector string) ([]map[string]any, error) {
	return c.list(ctx, fmt.Sprintf(pathMachineDeployments, ns), labelSelector)
}

func toAny[V any](m map[string]V) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
