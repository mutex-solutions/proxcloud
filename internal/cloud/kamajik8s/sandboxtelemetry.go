package kamajik8s

import (
	"context"
	"fmt"
	"net/http"
)

// ListConfigMaps reads the dedicated telemetry namespace; no kubelet proxy right.
func (c *Client) ListConfigMaps(ctx context.Context, ns, selector string) ([]map[string]any, error) {
	return c.list(ctx, "/api/v1/namespaces/"+ns+"/configmaps", selector)
}
func (c *Client) SetNodeUnschedulable(ctx context.Context, name string, value bool) error {
	_, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%s", name), nil, map[string]any{"spec": map[string]any{"unschedulable": value}}, "application/merge-patch+json")
	return err
}
