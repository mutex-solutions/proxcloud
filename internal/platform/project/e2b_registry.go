package project

// e2b_registry.go — background upkeep for custom templates (docs/managed-sandbox.md §7):
//   - SyncSandboxBuilds records finished template builds nobody is polling (the SDK polls while it waits; a client that
//     leaves would otherwise find its build "disappeared" once the build pod is cleaned up, although the image was
//     pushed).
//   - PruneSandboxRegistry deletes template images nothing references any more (deleted templates, old builds) through
//     the OCI distribution API of the sandbox registry (Zot), as the registry's gc account (read and delete on t/**
//     only). Zot's own garbage collection then frees the blobs. Ported from stratos-ee, which pruned through Harbor's API.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// SyncSandboxBuilds folds finished build pods into their templates (scheduled every minute).
func (h *Handler) SyncSandboxBuilds(ctx context.Context) {
	if h.sbTemplates == nil || h.sandboxFor == nil {
		return
	}
	var all []sandboxTemplateDoc
	if err := h.sbTemplates.Find(ctx, pgdoc.M{}, &all); err != nil {
		slog.Warn("build sync: list templates", "err", err)
		return
	}
	callers := map[string]*e2bCaller{}
	callerOf := func(projectID string) *e2bCaller {
		if c, ok := callers[projectID]; ok {
			return c
		}
		var c *e2bCaller
		if proj, err := h.svc.repo.FindByID(ctx, projectID); err == nil && proj != nil {
			c, _, _ = h.e2bCallerFor(ctx, nil, proj)
		}
		callers[projectID] = c
		return c
	}
	for i := range all {
		t := &all[i]
		for j := range t.Builds {
			bl := &t.Builds[j]
			if bl.Status != sandbox.BuildBuilding {
				continue
			}
			c := callerOf(t.ProjectID)
			if c == nil {
				continue
			}
			// A missing pod means failure only once the build is past its deadline: until then it may be about to be
			// recorded by a poll.
			gone := bl.CreatedAt != nil && time.Since(*bl.CreatedAt) > sandbox.BuildTimeout+5*time.Minute
			h.syncBuildPod(ctx, c, t, bl, gone)
		}
	}
}

// ---- registry pruning --------------------------------------------------------------------------

const (
	// RegistryPruneGrace: the scheduled prune never touches an image created less than this long ago.
	RegistryPruneGrace   = 24 * time.Hour
	registryPruneRecheck = 2 * time.Minute // a second look before deleting
	registryPruneMax     = 200             // deletions per run
)

var (
	templateRepoRe = regexp.MustCompile(`^t/[a-z0-9]{1,40}/[0-9a-f]{20}$`)
	digestRe       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// registryKeys normalises an image ref into the keys it keeps alive: "<repo>@<digest>" and/or "<repo>:<tag>", with the
// repo relative to the registry host ("t/<p>/<t>"). Refs on other hosts or outside t/ yield nothing.
func registryKeys(host, ref string) []string {
	rest, ok := strings.CutPrefix(ref, host+"/")
	if !ok || !strings.HasPrefix(rest, "t/") {
		return nil
	}
	repo, digest, hasDigest := strings.Cut(rest, "@")
	var keys []string
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		keys = append(keys, repo) // "<repo>:<tag>"
		repo = repo[:i]
	}
	if hasDigest && digestRe.MatchString(digest) {
		keys = append(keys, repo+"@"+digest)
	}
	return keys
}

type registryArtifact struct {
	Repo    string // "t/<p>/<t>"
	Digest  string
	Tags    []string
	Created time.Time // the image config's created (zero: unknown, never pruned)
}

// ownedOnly keeps artifacts of projects that exist in this deployment. A repo of an unknown project is treated as
// foreign (another portal on the same registry), not as deleted, so a deleted project's images are left alone.
func (h *Handler) ownedOnly(ctx context.Context, arts []registryArtifact) ([]registryArtifact, error) {
	known := map[string]bool{}
	var out []registryArtifact
	for _, a := range arts {
		pid := strings.Split(a.Repo, "/")[1]
		k, seen := known[pid]
		if !seen {
			p, err := h.svc.repo.FindByID(ctx, pid)
			if err != nil {
				return nil, err
			}
			k = p != nil
			known[pid] = k
		}
		if k {
			out = append(out, a)
		}
	}
	return out, nil
}

// pruneCandidates are the artifacts that are old enough, well-formed and referenced by nothing.
func pruneCandidates(arts []registryArtifact, live map[string]bool, now time.Time, grace time.Duration) []registryArtifact {
	var out []registryArtifact
	for _, a := range arts {
		if !templateRepoRe.MatchString(a.Repo) || !digestRe.MatchString(a.Digest) || a.Created.IsZero() || now.Sub(a.Created) < grace {
			continue
		}
		if live[a.Repo+"@"+a.Digest] {
			continue
		}
		used := false
		for _, tg := range a.Tags {
			if live[a.Repo+":"+tg] {
				used = true
				break
			}
		}
		if !used {
			out = append(out, a)
		}
	}
	return out
}

// ociRegistry is a minimal OCI distribution API client (catalog, tags, manifests, delete) with basic auth.
type ociRegistry struct {
	base string // https://<host>
	cred sandbox.RegistryCredential
	http *http.Client
}

func newOCIRegistry(host, caPEM string, cred sandbox.RegistryCredential) *ociRegistry {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caPEM != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(caPEM))
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &ociRegistry{base: "https://" + host, cred: cred, http: &http.Client{Timeout: 30 * time.Second, Transport: tr}}
}

const manifestAccept = "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"

func (o *ociRegistry) do(ctx context.Context, method, path, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, o.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(o.cred.User, o.cred.Password)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return o.http.Do(req)
}

func (o *ociRegistry) getJSON(ctx context.Context, path, accept string, out any) (http.Header, error) {
	resp, err := o.do(ctx, http.MethodGet, path, accept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("registry GET %s: %s", path, resp.Status)
	}
	return resp.Header, json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// repos lists the catalog under t/ (the registry shows only what this account may read).
func (o *ociRegistry) repos(ctx context.Context) ([]string, error) {
	var out []string
	last := ""
	for {
		var page struct {
			Repositories []string `json:"repositories"`
		}
		q := url.Values{"n": {"1000"}}
		if last != "" {
			q.Set("last", last)
		}
		if _, err := o.getJSON(ctx, "/v2/_catalog?"+q.Encode(), "", &page); err != nil {
			return nil, err
		}
		for _, r := range page.Repositories {
			if strings.HasPrefix(r, "t/") {
				out = append(out, r)
			}
		}
		if len(page.Repositories) < 1000 {
			return out, nil
		}
		last = page.Repositories[len(page.Repositories)-1]
	}
}

// artifacts lists every tagged image of the template repositories, tags grouped by digest, with the created time of
// each image's config.
func (o *ociRegistry) artifacts(ctx context.Context) ([]registryArtifact, error) {
	repos, err := o.repos(ctx)
	if err != nil {
		return nil, err
	}
	var out []registryArtifact
	for _, repo := range repos {
		var tl struct {
			Tags []string `json:"tags"`
		}
		if _, err := o.getJSON(ctx, "/v2/"+repo+"/tags/list", "", &tl); err != nil {
			return nil, err
		}
		byDigest := map[string]*registryArtifact{}
		for _, tag := range tl.Tags {
			var m struct {
				MediaType string `json:"mediaType"`
				Config    struct {
					Digest string `json:"digest"`
				} `json:"config"`
			}
			hdr, err := o.getJSON(ctx, "/v2/"+repo+"/manifests/"+url.PathEscape(tag), manifestAccept, &m)
			if err != nil {
				return nil, err
			}
			d := hdr.Get("Docker-Content-Digest")
			a := byDigest[d]
			if a == nil {
				a = &registryArtifact{Repo: repo, Digest: d}
				if digestRe.MatchString(m.Config.Digest) { // an index has no config: Created stays zero, never pruned
					var c struct {
						Created time.Time `json:"created"`
					}
					if _, err := o.getJSON(ctx, "/v2/"+repo+"/blobs/"+m.Config.Digest, "", &c); err == nil {
						a.Created = c.Created
					}
				}
				byDigest[d] = a
			}
			a.Tags = append(a.Tags, tag)
		}
		ds := make([]string, 0, len(byDigest))
		for d := range byDigest {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		for _, d := range ds {
			out = append(out, *byDigest[d])
		}
	}
	return out, nil
}

// delete removes a manifest by digest, which drops every tag on it (absent = success).
func (o *ociRegistry) delete(ctx context.Context, a registryArtifact) error {
	resp, err := o.do(ctx, http.MethodDelete, "/v2/"+a.Repo+"/manifests/"+a.Digest, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("registry DELETE %s@%s: %s", a.Repo, a.Digest, resp.Status)
}

// registryLive is every key some record or workload still references. It reads the clusters, then the database, then
// the clusters again, so a reference moving between them (a sandbox started from a template meanwhile) is seen. Any
// error aborts: nothing may be deleted on a partial view.
func (h *Handler) registryLive(ctx context.Context, host string, svcs []*sandbox.Service) (map[string]bool, error) {
	live := map[string]bool{}
	add := func(ref string) {
		for _, k := range registryKeys(host, ref) {
			live[k] = true
		}
	}
	readClusters := func() error {
		for _, ss := range svcs {
			refs, err := ss.RegistryRefsInUse(ctx)
			if err != nil {
				return err
			}
			for _, r := range refs {
				add(r)
			}
			for _, t := range ss.Config().Templates {
				add(t.Image)
			}
		}
		return nil
	}
	if err := readClusters(); err != nil {
		return nil, err
	}
	var templates []sandboxTemplateDoc
	if err := h.sbTemplates.Find(ctx, pgdoc.M{}, &templates); err != nil {
		return nil, err
	}
	for _, t := range templates {
		for _, b := range t.Builds {
			add(b.Image)
			add(b.From)
			if b.Status == sandbox.BuildWaiting || b.Status == sandbox.BuildBuilding {
				add(fmt.Sprintf("%s/t/%s/%s:%s", host, t.ProjectID, t.ID, b.BuildID))
			}
		}
	}
	if err := readClusters(); err != nil {
		return nil, err
	}
	return live, nil
}

// pruneTarget is the one registry every sandbox provider of this portal names, its mode and the gc account. Any
// provider in dry-run makes the run a dry run; providers naming different registries stop it (each would see the
// other's images as unreferenced).
func pruneTarget(cfgs []sandbox.Config) (host, ca, mode string, cred sandbox.RegistryCredential, err error) {
	for _, cfg := range cfgs {
		if cfg.TemplateRegistry == "" {
			continue
		}
		h, _, _ := strings.Cut(strings.TrimRight(cfg.TemplateRegistry, "/"), "/")
		if host != "" && h != host {
			return "", "", "", cred, fmt.Errorf("sandbox providers name two template registries (%s, %s)", host, h)
		}
		host = h
		switch cfg.RegistryPrune {
		case "dry-run":
			mode = "dry-run"
		case "on":
			if mode == "" {
				mode = "on"
			}
		}
		if cred.User == "" && cfg.RegistryGC.User != "" {
			cred, ca = cfg.RegistryGC, cfg.RegistryCA
		}
	}
	return host, ca, mode, cred, nil
}

// PruneSandboxRegistry deletes unreferenced template images (scheduled hourly). It runs only when a sandbox provider
// sets registryPrune ("dry-run" logs, "on" deletes) and holds the gc account; every sandbox provider's cluster
// (disabled ones too) counts as a user.
func (h *Handler) PruneSandboxRegistry(ctx context.Context) {
	h.PruneSandboxRegistryOlderThan(ctx, RegistryPruneGrace)
}

// PruneSandboxRegistryOlderThan is the prune with another age floor (the mgmt port's run-registry-prune?grace=).
func (h *Handler) PruneSandboxRegistryOlderThan(ctx context.Context, grace time.Duration) {
	if h.sbTemplates == nil || h.sandboxFor == nil {
		return
	}
	es, err := h.esSvc.List(ctx)
	if err != nil {
		return
	}
	var svcs []*sandbox.Service
	var cfgs []sandbox.Config
	for i := range es {
		e := &es[i]
		if !e.IsSandbox() {
			continue
		}
		ss, err := h.sandboxFor(e)
		if err != nil {
			slog.Error("registry prune: sandbox service", "service", e.ID, "err", err)
			return // a cluster we cannot read might use any image
		}
		svcs = append(svcs, ss)
		cfgs = append(cfgs, ss.Config())
	}
	host, ca, mode, cred, err := pruneTarget(cfgs)
	if err != nil {
		slog.Error("registry prune", "err", err)
		return
	}
	if host == "" || mode == "" || cred.User == "" {
		return
	}
	h.pruneRegistry(ctx, host, svcs, newOCIRegistry(host, ca, cred), mode == "dry-run", grace, time.Sleep)
}

// pruneRegistry returns how many images it deleted (or would have, in a dry run).
func (h *Handler) pruneRegistry(ctx context.Context, host string, svcs []*sandbox.Service, reg *ociRegistry, dryRun bool, grace time.Duration, sleep func(time.Duration)) int {
	live, err := h.registryLive(ctx, host, svcs)
	if err != nil {
		slog.Error("registry prune: live set", "err", err)
		return 0
	}
	arts, err := reg.artifacts(ctx)
	if err != nil {
		slog.Error("registry prune: list images", "err", err)
		return 0
	}
	cands, err := h.ownedOnly(ctx, pruneCandidates(arts, live, time.Now(), grace))
	if err != nil {
		slog.Error("registry prune: project lookup", "err", err)
		return 0
	}
	if len(cands) == 0 {
		slog.Info("registry prune: nothing to delete", "images", len(arts))
		return 0
	}
	sleep(registryPruneRecheck)
	if ctx.Err() != nil {
		return 0
	}
	if live, err = h.registryLive(ctx, host, svcs); err != nil {
		slog.Error("registry prune: live set (recheck)", "err", err)
		return 0
	}
	cands = pruneCandidates(cands, live, time.Now(), grace)
	if len(cands) > registryPruneMax {
		slog.Info("registry prune: capped", "candidates", len(cands), "max", registryPruneMax)
		cands = cands[:registryPruneMax]
	}
	deleted := 0
	var errs []error
	for _, a := range cands {
		if dryRun {
			slog.Info("registry prune: would delete", "repo", a.Repo, "digest", a.Digest, "tags", a.Tags, "created", a.Created)
			deleted++
			continue
		}
		if err := reg.delete(ctx, a); err != nil {
			errs = append(errs, err)
			continue
		}
		slog.Info("registry prune: deleted", "repo", a.Repo, "digest", a.Digest, "tags", a.Tags)
		deleted++
	}
	if err := errors.Join(errs...); err != nil {
		slog.Error("registry prune: delete", "err", err)
	}
	slog.Info("registry prune: done", "dryRun", dryRun, "images", len(arts), "candidates", len(cands), "deleted", deleted)
	return deleted
}
