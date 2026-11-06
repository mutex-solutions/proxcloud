package sso

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// KCAdmin talks to the Keycloak Admin API of the customer realm with the proxcloud-idp-admin
// service account. Its roles are confined to identity providers and users; it cannot edit clients,
// flows or realm settings. The operator console's Create user uses it too.
type KCAdmin struct {
	base, realm, clientID, secret string
	http                          *http.Client

	mu    sync.Mutex
	token string
	exp   time.Time
}

// NewKCAdmin derives the realm from the issuer and the endpoint from discoveryURI when this
// process reaches Keycloak under another name than the browser does.
func NewKCAdmin(issuer, discoveryURI, clientID, secret string) (*KCAdmin, error) {
	from := issuer
	if discoveryURI != "" {
		from = discoveryURI
	}
	base, realm, ok := strings.Cut(strings.TrimRight(from, "/"), "/realms/")
	if !ok || realm == "" || strings.Contains(realm, "/") {
		return nil, fmt.Errorf("sso: cannot derive a realm from %q", from)
	}
	return &KCAdmin{base: base, realm: realm, clientID: clientID, secret: secret,
		http: &http.Client{Timeout: 20 * time.Second}}, nil
}

func (k *KCAdmin) accessToken(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Now().Before(k.exp) {
		return k.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {k.clientID}, "client_secret": {k.secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		k.base+"/realms/"+url.PathEscape(k.realm)+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := k.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sso: keycloak token: status %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", err
	}
	k.token, k.exp = tok.AccessToken, time.Now().Add(time.Duration(tok.ExpiresIn-30)*time.Second)
	return k.token, nil
}

// call performs one admin request. 404 comes back as found=false instead of an error.
func (k *KCAdmin) call(ctx context.Context, method, path, contentType string, body io.Reader, out any) (found bool, err error) {
	tok, err := k.accessToken(ctx)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+"/admin/realms/"+url.PathEscape(k.realm)+path, body)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("sso: keycloak %s %s: status %d: %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		return true, json.NewDecoder(resp.Body).Decode(out)
	}
	return true, nil
}

func (k *KCAdmin) callJSON(ctx context.Context, method, path string, in, out any) (bool, error) {
	if in == nil {
		return k.call(ctx, method, path, "", nil, out)
	}
	buf, err := json.Marshal(in)
	if err != nil {
		return false, err
	}
	return k.call(ctx, method, path, "application/json", bytes.NewReader(buf), out)
}

func idpPath(alias string) string { return "/identity-provider/instances/" + url.PathEscape(alias) }

// ImportConfig has Keycloak parse a discovery document or SAML metadata that this process already
// fetched behind the SSRF guard, so Keycloak itself never dereferences a tenant-supplied URL here.
func (k *KCAdmin) ImportConfig(ctx context.Context, providerID string, doc []byte) (map[string]string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("providerId", providerID); err != nil {
		return nil, err
	}
	f, err := w.CreateFormFile("file", "metadata")
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(doc); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	out := map[string]string{}
	if _, err := k.call(ctx, http.MethodPost, "/identity-provider/import-config", w.FormDataContentType(), &buf, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetIdP returns the raw instance or found=false.
func (k *KCAdmin) GetIdP(ctx context.Context, alias string) (map[string]any, bool, error) {
	var out map[string]any
	found, err := k.callJSON(ctx, http.MethodGet, idpPath(alias), nil, &out)
	return out, found, err
}

// CreateIdP creates an instance.
func (k *KCAdmin) CreateIdP(ctx context.Context, rep map[string]any) error {
	_, err := k.callJSON(ctx, http.MethodPost, "/identity-provider/instances", rep, nil)
	return err
}

// UpdateIdP replaces an instance.
func (k *KCAdmin) UpdateIdP(ctx context.Context, alias string, rep map[string]any) error {
	_, err := k.callJSON(ctx, http.MethodPut, idpPath(alias), rep, nil)
	return err
}

// DeleteIdP removes an instance; a missing one is fine.
func (k *KCAdmin) DeleteIdP(ctx context.Context, alias string) error {
	_, err := k.callJSON(ctx, http.MethodDelete, idpPath(alias), nil, nil)
	return err
}

const groupsMapperName = "idp_groups"

// EnsureGroupsMapper copies the IdP's groups claim or attribute into the idp_groups user attribute
// on every login.
func (k *KCAdmin) EnsureGroupsMapper(ctx context.Context, alias, protocol, claim string) error {
	rep := map[string]any{"name": groupsMapperName, "identityProviderAlias": alias}
	cfg := map[string]string{"user.attribute": "idp_groups", "syncMode": "FORCE"}
	if protocol == protoSAML {
		rep["identityProviderMapper"] = "saml-user-attribute-idp-mapper"
		cfg["attribute.name"] = claim
	} else {
		rep["identityProviderMapper"] = "oidc-user-attribute-idp-mapper"
		cfg["claim"] = claim
	}
	rep["config"] = cfg

	var existing []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if _, err := k.callJSON(ctx, http.MethodGet, idpPath(alias)+"/mappers", nil, &existing); err != nil {
		return err
	}
	for _, m := range existing {
		if m.Name == groupsMapperName {
			rep["id"] = m.ID
			_, err := k.callJSON(ctx, http.MethodPut, idpPath(alias)+"/mappers/"+url.PathEscape(m.ID), rep, nil)
			return err
		}
	}
	_, err := k.callJSON(ctx, http.MethodPost, idpPath(alias)+"/mappers", rep, nil)
	return err
}

// KCUser is the slice of a Keycloak user the foreign-user sweep needs.
type KCUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// UsersOfIdP lists the Keycloak users linked to an IdP.
func (k *KCAdmin) UsersOfIdP(ctx context.Context, alias string) ([]KCUser, error) {
	var out []KCUser
	_, err := k.callJSON(ctx, http.MethodGet, "/users?idpAlias="+url.QueryEscape(alias)+"&briefRepresentation=true&max=1000", nil, &out)
	return out, err
}

// IsDisposable reports whether a user has exactly this one IdP link and no credential, meaning the
// account holds nothing but what that IdP asserted.
func (k *KCAdmin) IsDisposable(ctx context.Context, userID, alias string) (bool, error) {
	var links []struct {
		IdentityProvider string `json:"identityProvider"`
	}
	if _, err := k.callJSON(ctx, http.MethodGet, "/users/"+url.PathEscape(userID)+"/federated-identity", nil, &links); err != nil {
		return false, err
	}
	if len(links) != 1 || links[0].IdentityProvider != alias {
		return false, nil
	}
	var creds []json.RawMessage
	if _, err := k.callJSON(ctx, http.MethodGet, "/users/"+url.PathEscape(userID)+"/credentials", nil, &creds); err != nil {
		return false, err
	}
	return len(creds) == 0, nil
}

// DeleteUser removes a Keycloak user.
func (k *KCAdmin) DeleteUser(ctx context.Context, userID string) error {
	_, err := k.callJSON(ctx, http.MethodDelete, "/users/"+url.PathEscape(userID), nil, nil)
	return err
}

// CreateUser makes an enabled account with a verified email (the operator vouches for it) and a
// temporary password, returning its id and that password. An account already holding the email is
// reused: its id comes back with no password.
func (k *KCAdmin) CreateUser(ctx context.Context, email, first, last string) (id, tempPassword string, err error) {
	if id, err = k.userIDByEmail(ctx, email); err != nil || id != "" {
		return id, "", err
	}
	secret := make([]byte, 18)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	tempPassword = base64.RawURLEncoding.EncodeToString(secret)
	if _, err := k.callJSON(ctx, http.MethodPost, "/users", map[string]any{
		"username": email, "email": email, "firstName": first, "lastName": last,
		"enabled": true, "emailVerified": true,
		"credentials": []map[string]any{{"type": "password", "value": tempPassword, "temporary": true}},
	}, nil); err != nil {
		return "", "", err
	}
	if id, err = k.userIDByEmail(ctx, email); err != nil {
		return "", "", err
	}
	if id == "" {
		return "", "", fmt.Errorf("sso: keycloak does not list the user it just created: %s", email)
	}
	return id, tempPassword, nil
}

func (k *KCAdmin) userIDByEmail(ctx context.Context, email string) (string, error) {
	var out []KCUser
	if _, err := k.callJSON(ctx, http.MethodGet, "/users?exact=true&email="+url.QueryEscape(email), nil, &out); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", nil
	}
	return out[0].ID, nil
}
