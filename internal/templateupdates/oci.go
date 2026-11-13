package templateupdates

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OCI media types (OCI image-spec v1.0/v1.1 and the Docker v2 equivalents GHCR serves).
const (
	MediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	maxManifestBytes    = 4 << 20
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

// Manifest is an OCI image manifest or index (only the fields we check).
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	ArtifactType  string       `json:"artifactType,omitempty"`
	Config        *Descriptor  `json:"config,omitempty"`
	Layers        []Descriptor `json:"layers,omitempty"`
	Manifests     []Descriptor `json:"manifests,omitempty"`
}

// repoForPackage is the registry repository of a package owned by Owner.
func repoForPackage(pkg string) (string, error) {
	if !packageNameRE.MatchString(pkg) {
		return "", errors.New("ghcr: invalid package name")
	}
	return Owner + "/" + pkg, nil
}

// registryToken exchanges the PAT for a pull-scoped registry bearer token (distribution token
// auth). The realm must be on the registry's own host: the PAT is never sent elsewhere.
func (c *Client) registryToken(ctx context.Context, challenge string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	params := parseChallenge(challenge)
	realm := params["realm"]
	ru, err := url.Parse(realm)
	if err != nil || ru.Host != c.registry || (ru.Scheme != c.scheme) {
		return "", errors.New("ghcr: refusing a token realm outside the registry host")
	}
	q := ru.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if s := params["scope"]; s != "" {
		if !strings.HasPrefix(s, "repository:"+Owner+"/") || !strings.HasSuffix(s, ":pull") {
			return "", errors.New("ghcr: unexpected token scope")
		}
		q.Set("scope", s)
	}
	ru.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("proxcloud:"+c.token)))
	res, err := c.http.Do(req)
	if err != nil {
		return "", scrub(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", classify(res, body)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &tok) != nil {
		return "", errors.New("ghcr: malformed token response")
	}
	if tok.Token == "" {
		tok.Token = tok.AccessToken
	}
	if tok.Token == "" {
		return "", errors.New("ghcr: empty registry token")
	}
	return tok.Token, nil
}

// Fixed refusal codes of PullToken (Permanent errors; their messages never carry a token or the PAT).
const (
	// CodeTokenUnscoped: GHCR answered with something that is not a short-lived scoped registry token.
	CodeTokenUnscoped = "ghcr_token_unscoped"
	// CodePackageNotPublic: the package cannot be read anonymously (the sandbox mirror supports public packages only).
	CodePackageNotPublic = "ghcr_package_not_public"
	// maxPullTokenBytes bounds the token written into the worker's Secret; maxPullTokenTTL bounds expires_in.
	maxPullTokenBytes = 8 << 10
	maxPullTokenTTL   = 3600
)

// PullToken returns an ANONYMOUS short-lived GHCR registry token with pull scope on exactly ONE public sandbox package
// (scope repository:<Owner>/sandbox-<name>:pull, service ghcr.io), for the sandbox mirror worker. The request carries
// NO Authorization header: the PAT (used for discovery only) never takes part in minting it, and there is no fallback
// to the PAT for a package that is not public (Permanent CodePackageNotPublic). Before it is returned the token is
// checked (CheckPullToken against the PAT) and its pull access is confirmed with one anonymous-token tags read; a
// token that is, contains or decodes to the PAT (or any GitHub token), or that claims a lifetime over an hour, is
// refused (Permanent CodeTokenUnscoped). Redirects are not followed.
func (c *Client) PullToken(ctx context.Context, pkg string) (string, error) {
	repo, err := repoForPackage(pkg)
	if err != nil || !strings.HasPrefix(pkg, "sandbox-") {
		return "", errors.New("ghcr: invalid sandbox package name")
	}
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	hc := &http.Client{Transport: c.http.Transport, Timeout: c.requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	q := url.Values{"scope": {"repository:" + repo + ":pull"}, "service": {DefaultRegistry}}
	get := func(u, bearer string) (*http.Response, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("User-Agent", "proxcloud-template-updates")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res, err := hc.Do(req)
		if err != nil {
			return nil, nil, scrub(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		return res, body, nil
	}
	notPublic := func(status int) bool {
		return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound
	}
	res, body, err := get(c.scheme+"://"+c.registry+"/token?"+q.Encode(), "")
	if err != nil {
		return "", err
	}
	switch {
	case notPublic(res.StatusCode):
		return "", permanent(CodePackageNotPublic, "GHCR does not serve "+repo+" anonymously; the sandbox mirror supports public sandbox packages only")
	case res.StatusCode != http.StatusOK:
		return "", classify(res, nil)
	}
	var tr struct {
		Token       string      `json:"token"`
		AccessToken string      `json:"access_token"`
		ExpiresIn   json.Number `json:"expires_in"`
	}
	if json.Unmarshal(body, &tr) != nil {
		return "", permanent(CodeTokenUnscoped, "GHCR answered the anonymous token request with a malformed response")
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if err := CheckPullToken(tok, c.token); err != nil {
		return "", err
	}
	if tr.AccessToken != "" && tr.AccessToken != tok {
		if err := CheckPullToken(tr.AccessToken, c.token); err != nil {
			return "", err // GHCR handed out something PAT-derived next to it: the whole answer is distrusted
		}
	}
	if tr.ExpiresIn != "" {
		if ttl, err := strconv.ParseFloat(tr.ExpiresIn.String(), 64); err != nil || !(ttl > 0 && ttl <= maxPullTokenTTL) {
			return "", permanent(CodeTokenUnscoped, "GHCR's anonymous registry token does not expire within an hour")
		}
	}
	// The anonymous token must read the package (a public package answers 200; a private one 401/403/404).
	res, _, err = get(c.scheme+"://"+c.registry+"/v2/"+repo+"/tags/list?n=1", tok)
	if err != nil {
		return "", err
	}
	switch {
	case notPublic(res.StatusCode):
		return "", permanent(CodePackageNotPublic, "GHCR refuses an anonymous read of "+repo+"; the sandbox mirror supports public sandbox packages only")
	case res.StatusCode != http.StatusOK:
		return "", classify(res, nil)
	}
	return tok, nil
}

// githubTokenPrefixes are GitHub credential prefixes (classic/fine-grained PAT, OAuth, user/server-to-server, refresh).
var githubTokenPrefixes = []string{"ghp_", "github_pat_", "gho_", "ghu_", "ghs_", "ghr_"}

// CheckPullToken refuses (Permanent CodeTokenUnscoped) anything that must not be written into a worker Secret as a
// GHCR registry token: empty, longer than 8 KiB, not printable ASCII without whitespace, a GitHub token (by prefix),
// or — when pat is non-empty — the PAT itself, a token containing it, containing its base64 form (any alphabet,
// padding or alignment), or one whose base64 decoding (std/raw/url/raw-url, also after a leading "<word>:" prefix such
// as "v1:", nested up to three levels) contains the PAT or starts with a GitHub token prefix. The error never includes
// the token or the PAT.
func CheckPullToken(tok, pat string) error {
	refuse := permanent(CodeTokenUnscoped, "GHCR did not answer with a short-lived scoped registry token; it was not used")
	if tok == "" || len(tok) > maxPullTokenBytes {
		return refuse
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] <= ' ' || tok[i] > '~' {
			return refuse
		}
	}
	var needles []string
	if pat != "" {
		needles = append(needles, pat)
		needles = append(needles, base64Needles(pat)...)
	}
	bad := func(s string) bool {
		for _, p := range githubTokenPrefixes {
			if strings.HasPrefix(s, p) {
				return true
			}
		}
		for _, n := range needles {
			if strings.Contains(s, n) {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	level := []string{tok}
	for depth := 0; depth < 4 && len(level) > 0; depth++ {
		var next []string
		for _, s := range level {
			if seen[s] {
				continue
			}
			seen[s] = true
			if bad(s) {
				return refuse
			}
			if depth == 3 {
				continue
			}
			cands := []string{s}
			if w, rest, ok := strings.Cut(s, ":"); ok && w != "" && isWord(w) {
				cands = append(cands, rest)
				if bad(rest) {
					return refuse
				}
			}
			for _, cs := range cands {
				for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
					if b, err := enc.DecodeString(cs); err == nil && len(b) > 0 {
						next = append(next, string(b))
					}
				}
			}
		}
		level = next
	}
	return nil
}

func isWord(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// base64Needles are the substrings every base64 encoding (standard or URL alphabet, any padding) of a text containing
// pat must contain, for each of the three byte alignments pat can start at.
func base64Needles(pat string) []string {
	var out []string
	for shift := 0; shift < 3; shift++ {
		enc := base64.StdEncoding.EncodeToString(append(make([]byte, shift), pat...))
		start := (shift*8 + 5) / 6 // the first character made only of pat bits
		end := len(enc) - 4        // drop the final quantum (padding / bits shared with what follows)
		if end-start < 8 {
			continue
		}
		n := enc[start:end]
		out = append(out, n, strings.NewReplacer("+", "-", "/", "_").Replace(n))
	}
	return out
}

// parseChallenge parses `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) map[string]string {
	out := map[string]string{}
	_, rest, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(strings.Fields(h)[0], "Bearer") {
		return out
	}
	for _, part := range splitParams(rest) {
		k, v, ok := strings.Cut(part, "=")
		if ok {
			out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return out
}

func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// registryDo performs a registry GET with token auth: an unauthenticated try, then (on a 401
// Bearer challenge) the PAT-backed token exchange. Registry tokens are cached per scope. hc is
// c.http (bounded calls) or c.blob (streams).
func (c *Client) registryDo(ctx context.Context, hc *http.Client, path, accept string) (*http.Response, error) {
	u := c.scheme + "://" + c.registry + path
	send := func(tok string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		req.Header.Set("User-Agent", "proxcloud-template-updates")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := hc.Do(req)
		if err != nil {
			return nil, scrub(err)
		}
		return res, nil
	}
	scope := registryScope(path)
	if tok := c.regTokens[scope]; tok != "" {
		res, err := send(tok)
		if err != nil || res.StatusCode != http.StatusUnauthorized {
			return res, err
		}
		res.Body.Close()
	}
	res, err := send("")
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		return res, err
	}
	challenge := res.Header.Get("WWW-Authenticate")
	res.Body.Close()
	tok, err := c.registryToken(ctx, challenge)
	if err != nil {
		return nil, err
	}
	if c.regTokens == nil {
		c.regTokens = map[string]string{}
	}
	c.regTokens[scope] = tok
	return send(tok)
}

func registryScope(path string) string {
	p := strings.TrimPrefix(path, "/v2/")
	if i := strings.Index(p, "/manifests/"); i >= 0 {
		return p[:i]
	}
	if i := strings.Index(p, "/blobs/"); i >= 0 {
		return p[:i]
	}
	return p
}

// GetManifest fetches a manifest BY DIGEST and verifies the bytes hash to that digest (a
// registry cannot substitute content for an immutable reference).
func (c *Client) GetManifest(ctx context.Context, pkg, digest string) (*Manifest, []byte, error) {
	repo, err := repoForPackage(pkg)
	if err != nil {
		return nil, nil, err
	}
	if !digestRE.MatchString(digest) {
		return nil, nil, errors.New("ghcr: digest must be sha256:<64 lowercase hex>")
	}
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	accept := strings.Join([]string{MediaOCIManifest, MediaOCIIndex, MediaDockerManifest, MediaDockerList}, ", ")
	res, err := c.registryDo(ctx, c.http, "/v2/"+repo+"/manifests/"+digest, accept)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxManifestBytes+1))
	if err != nil {
		return nil, nil, scrub(err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, nil, classify(res, body)
	}
	if len(body) > maxManifestBytes {
		return nil, nil, errors.New("ghcr: manifest too large")
	}
	sum := sha256.Sum256(body)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, nil, errors.New("ghcr: manifest bytes do not match the requested digest")
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, errors.New("ghcr: malformed manifest")
	}
	if m.MediaType == "" {
		m.MediaType = res.Header.Get("Content-Type")
	}
	return &m, body, nil
}

// FetchBlob reads a small blob (≤ limit bytes) and verifies its digest and descriptor size.
func (c *Client) FetchBlob(ctx context.Context, pkg string, d Descriptor, limit int64) ([]byte, error) {
	if d.Size <= 0 || d.Size > limit {
		return nil, fmt.Errorf("ghcr: blob size %d outside 1..%d", d.Size, limit)
	}
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	rc, err := c.OpenBlob(ctx, pkg, d.Digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	vr := NewVerifyingReader(rc, d.Digest, d.Size)
	body, err := io.ReadAll(vr)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// ErrBlobStalled is returned when a blob stream delivers no byte for the idle timeout.
var ErrBlobStalled = errors.New("ghcr: blob transfer stalled")

// OpenBlob streams a blob by digest. GHCR redirects to a short-lived signed storage URL; the
// Authorization header is never forwarded there (see NewClient's CheckRedirect). The stream has no
// total deadline (an 8 GiB disk can take long); it is bounded by ctx, the transport's response
// header timeout, and the stall guard: no byte for blobIdle aborts it with ErrBlobStalled.
func (c *Client) OpenBlob(ctx context.Context, pkg, digest string) (io.ReadCloser, error) {
	repo, err := repoForPackage(pkg)
	if err != nil {
		return nil, err
	}
	if !digestRE.MatchString(digest) {
		return nil, errors.New("ghcr: invalid blob digest")
	}
	sctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(c.blobIdle, func() { cancel(ErrBlobStalled) })
	res, err := c.registryDo(sctx, c.blob, "/v2/"+repo+"/blobs/"+digest, "")
	if err != nil {
		timer.Stop()
		cancel(nil)
		if errors.Is(context.Cause(sctx), ErrBlobStalled) {
			return nil, ErrBlobStalled
		}
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		timer.Stop()
		cancel(nil)
		return nil, classify(res, body)
	}
	return &stallReader{rc: res.Body, ctx: sctx, cancel: cancel, timer: timer, idle: c.blobIdle}, nil
}

// stallReader re-arms the idle timer on every byte received; the timer cancels the request.
type stallReader struct {
	rc     io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	idle   time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 {
		s.timer.Reset(s.idle)
	}
	if err != nil && err != io.EOF && errors.Is(context.Cause(s.ctx), ErrBlobStalled) {
		return n, ErrBlobStalled
	}
	return n, err
}

func (s *stallReader) Close() error {
	s.timer.Stop()
	err := s.rc.Close()
	s.cancel(nil)
	return err
}

// VerifyingReader hashes everything read and fails the final Read (io.EOF position) unless the
// byte count equals size and the SHA-256 equals digest. Reading past size fails immediately.
type VerifyingReader struct {
	r      io.Reader
	h      hash.Hash
	digest string
	size   int64
	n      int64
	err    error
}

// ErrDigestMismatch is returned when streamed bytes do not hash to the expected digest.
var ErrDigestMismatch = errors.New("content does not match its expected sha256 digest")

// ErrSizeMismatch is returned when streamed bytes are longer or shorter than expected.
var ErrSizeMismatch = errors.New("content length does not match its expected size")

func NewVerifyingReader(r io.Reader, digest string, size int64) *VerifyingReader {
	return &VerifyingReader{r: r, h: sha256.New(), digest: strings.TrimPrefix(digest, "sha256:"), size: size}
}

func (v *VerifyingReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	n, err := v.r.Read(p)
	if n > 0 {
		v.n += int64(n)
		if v.n > v.size {
			v.err = ErrSizeMismatch
			return 0, v.err
		}
		v.h.Write(p[:n])
	}
	if err == io.EOF {
		if v.n != v.size {
			v.err = ErrSizeMismatch
			return n, v.err
		}
		if hex.EncodeToString(v.h.Sum(nil)) != v.digest {
			v.err = ErrDigestMismatch
			return n, v.err
		}
	}
	if err != nil && err != io.EOF {
		v.err = err
	}
	return n, err
}

// Sum is the hex SHA-256 of the bytes read so far.
func (v *VerifyingReader) Sum() string { return hex.EncodeToString(v.h.Sum(nil)) }

// N is the byte count read so far.
func (v *VerifyingReader) N() int64 { return v.n }
