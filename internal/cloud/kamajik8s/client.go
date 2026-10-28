package kamajik8s

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// API paths for the kinds this client touches. Group/version pins:
//   - argoproj.io/v1alpha1 Application (ArgoCD — stable since 2018, no v1 planned)
//   - kamaji.clastix.io/v1alpha1 TenantControlPlane
//   - cluster.x-k8s.io/v1beta1 Cluster API (MachineDeployment)
const (
	pathApplications        = "/apis/argoproj.io/v1alpha1/namespaces/%s/applications"
	pathTenantControlPlanes = "/apis/kamaji.clastix.io/v1alpha1/namespaces/%s/tenantcontrolplanes"
	pathMachineDeployments  = "/apis/cluster.x-k8s.io/v1beta1/namespaces/%s/machinedeployments"
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
	if c.rc.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.rc.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
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
