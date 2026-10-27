package pve

import (
	"context"
	"net/url"
	"strings"
)

// Access management — used by Phase K to mint narrow per-cluster tokens
// (proxcloud-capi@pve!<clusterID>) and by teardown to revoke them.

// CreateUser creates a PVE-realm user (idempotent: "already exists" is not an error).
func (c *Client) CreateUser(ctx context.Context, userid, comment string) error {
	form := formVals{"userid": userid, "comment": comment}.values()
	err := c.post(ctx, "/access/users", form, nil)
	if err != nil {
		if ae, ok := err.(*APIError); ok && strings.Contains(strings.ToLower(ae.Error()), "already exists") {
			return nil
		}
	}
	return err
}

// CreateAPIToken mints a token for a user. Returns the full token id + secret —
// the secret is shown exactly once by PVE, so the caller must persist it.
// privsep=true keeps the token's rights bounded by its own ACLs, not the user's.
func (c *Client) CreateAPIToken(ctx context.Context, userid, tokenName string) (tokenID, secret string, err error) {
	form := formVals{"privsep": "1"}.values()
	var out struct {
		FullTokenID string `json:"full-tokenid"`
		Value       string `json:"value"`
	}
	path := "/access/users/" + url.PathEscape(userid) + "/token/" + url.PathEscape(tokenName)
	if err := c.post(ctx, path, form, &out); err != nil {
		return "", "", err
	}
	return out.FullTokenID, out.Value, nil
}

// DeleteAPIToken revokes a token. Absent token is not an error (idempotent revoke).
func (c *Client) DeleteAPIToken(ctx context.Context, userid, tokenName string) error {
	path := "/access/users/" + url.PathEscape(userid) + "/token/" + url.PathEscape(tokenName)
	err := c.delete(ctx, path, nil, nil)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// SetACL grants (or with del revokes) a role on a path for users/tokens.
// path e.g. "/pool/st-abc123", roles e.g. "PVEVMAdmin".
func (c *Client) SetACL(ctx context.Context, path, roles string, tokens []string, del bool) error {
	form := formVals{
		"path":   path,
		"roles":  roles,
		"tokens": strings.Join(tokens, ","),
		"delete": pbool(del),
	}.values()
	return c.put(ctx, "/access/acl", form, nil)
}
