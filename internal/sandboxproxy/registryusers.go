package sandboxproxy

// registryusers.go — the sandbox registry's accounts (docs/managed-sandbox.md §6). The registry (Zot) runs on the
// services cluster; this keeps its users and access rules in step with the projects on the sandbox cluster:
//
//	pull-<project>   reads library/** and t/<project>/**                    Secret sandbox-template-pull in sb-<project>
//	build-<project>  reads library/** and t/<project>/**, writes t/<project>/** (template builds of that project, never
//	                 library/** or another project)                         Secret registry-build-<project> in sbx-build
//	pull-nodes       reads library/**                                       Secret sandbox-registry-nodes in the system namespace
//	gc               reads and deletes t/** (the portal's pruning of unreferenced template images)
//	admin            everything (the installer's mirror of the built-in images)
//
// A project's password is derived from a key (HMAC), so nothing is stored per project: a restart writes the same
// file. Zot reloads its config and htpasswd when they change on disk.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// Registry account names and the Secrets that carry pull credentials.
const (
	RegistryAdminUser  = "admin"
	RegistryGCUser     = "gc"
	RegistryNodesUser  = "pull-nodes"
	RegistryNodeSecret = "sandbox-registry-nodes"
)

// RegistryAPI is the slice of kamajik8s.Client (sandbox cluster) the account sync uses.
type RegistryAPI interface {
	ListNamespaces(ctx context.Context, labelSelector string) ([]map[string]any, error)
	GetSecretData(ctx context.Context, ns, name string) (map[string][]byte, error)
	ApplyCore(ctx context.Context, plural string, obj map[string]any) error
}

// RegistryUsers writes Zot's config.json and htpasswd into Dir and the pull Secrets onto the sandbox cluster.
type RegistryUsers struct {
	Base     map[string]any    // Zot's config without http.auth and http.accessControl (storage, TLS, log)
	Host     string            // the registry as the nodes pull from it: host[:port]
	Key      []byte            // derives pull-<project> passwords
	Static   map[string]string // admin, gc, pull-nodes -> password
	Dir      string            // config.json and htpasswd, read by Zot
	SystemNS string            // where the nodes' Secret goes (sandbox-system)
	// ServiceID, when set, counts only the namespaces of that sandbox provider (proxcloud.io/service): a cluster
	// shared by two portals gets one registry each.
	ServiceID string
	API       RegistryAPI

	hashes   map[string][2]string // user -> {password, bcrypt}
	projects []string             // as last written
	written  bool
	kept     bool // config.json was on disk at start and is kept until the first list succeeds
	// applied: build Secrets written by this process (ns/name -> content). Those are applied blind (this identity
	// cannot read Secrets in sbx-build, where user builds keep their registry credentials), once per process.
	applied map[string]string
}

// Ready reports that Zot has a config to serve: written by this process, or kept from before a restart.
func (r *RegistryUsers) Ready() bool { return r.written || r.kept }

var registryProjectRe = regexp.MustCompile(`^[a-z0-9]{1,40}$`)

// RegistryPassword is a project's pull password: the same for the same key and project, never stored.
func RegistryPassword(key []byte, user string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(user))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// RegistryProjects reads the projects off the sandbox cluster's tenant namespaces. Only a namespace the portal made
// counts (managed-by, tenant label, its name is sb-<project label>), and the id must be plain lowercase alphanumerics:
// it becomes part of a repository pattern, where a '*' or '/' would widen it.
func RegistryProjects(namespaces []map[string]any) []string {
	var out []string
	for _, ns := range namespaces {
		meta, _ := ns["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		name, _ := meta["name"].(string)
		pid, _ := labels[sandbox.LabelProject].(string)
		if labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue || labels[sandbox.LabelTenant] != "true" ||
			!registryProjectRe.MatchString(pid) || name != sandbox.NamespaceFor(pid) {
			continue
		}
		out = append(out, pid)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// ZotAccess is http.accessControl for these projects. Zot applies the most specific matching pattern, so a project's
// pattern repeats the gc user's rights.
func ZotAccess(projects []string) map[string]any {
	rw := []any{"read", "create", "update"}
	gc := map[string]any{"users": []any{RegistryGCUser}, "actions": []any{"read", "delete"}}
	pull := []any{RegistryNodesUser}
	repos := map[string]any{
		"library/**": map[string]any{"policies": []any{map[string]any{"groups": []any{"pull"}, "actions": []any{"read"}}}, "defaultPolicy": []any{}},
		"t/**":       map[string]any{"policies": []any{gc}, "defaultPolicy": []any{}},
	}
	for _, p := range projects {
		// The build account reads the built-in images too: a build from a built-in template starts from one.
		pull = append(pull, "pull-"+p, "build-"+p)
		repos["t/"+p+"/**"] = map[string]any{"policies": []any{
			map[string]any{"users": []any{"pull-" + p}, "actions": []any{"read"}},
			map[string]any{"users": []any{"build-" + p}, "actions": rw},
			gc,
		}, "defaultPolicy": []any{}}
	}
	return map[string]any{
		"groups":       map[string]any{"pull": map[string]any{"users": pull}},
		"repositories": repos,
		"adminPolicy":  map[string]any{"users": []any{RegistryAdminUser}, "actions": []any{"read", "create", "update", "delete"}},
	}
}

// Pass lists the projects, rewrites Zot's files when the set changed, and makes sure every project namespace and the
// system namespace hold their pull Secret. A failed list keeps the users Zot has (on the first pass of a new pod none:
// then only the static users are written, so the registry can start; after a sidecar restart the file on disk).
func (r *RegistryUsers) Pass(ctx context.Context) (changed bool, err error) {
	sel := sandbox.LabelTenant + "=true," + sandbox.LabelManagedBy + "=" + sandbox.ManagedByValue
	if r.ServiceID != "" {
		sel += "," + sandbox.LabelService + "=" + r.ServiceID
	}
	nss, lerr := r.API.ListNamespaces(ctx, sel)
	projects := RegistryProjects(nss)
	if lerr != nil && !r.written {
		// A restarted sidecar (the pod and its config volume stay): Zot still serves the last full config. Writing the
		// static users now would drop every project's account until a list succeeds.
		if _, err := os.Stat(filepath.Join(r.Dir, "config.json")); err == nil {
			r.kept = true
			return false, fmt.Errorf("list tenant namespaces (keeping the config on disk): %w", lerr)
		}
	}
	if lerr != nil && r.written {
		return false, fmt.Errorf("list tenant namespaces: %w", lerr)
	}
	if !r.written || !slices.Equal(projects, r.projects) {
		if err := r.write(projects); err != nil {
			return false, err
		}
		r.projects, r.written, changed = projects, true, true
	}
	if lerr != nil {
		return changed, fmt.Errorf("list tenant namespaces: %w", lerr)
	}
	var errs []error
	if err := r.ensureSecret(ctx, r.SystemNS, RegistryNodeSecret, RegistryNodesUser, nil); err != nil {
		errs = append(errs, err)
	}
	for _, p := range projects {
		labels := map[string]any{sandbox.LabelProject: p, sandbox.LabelManagedBy: ManagedByRegistry}
		if err := r.ensureSecret(ctx, sandbox.NamespaceFor(p), sandbox.TemplatePullSecret, "pull-"+p, labels); err != nil {
			errs = append(errs, err)
		}
		if err := r.applySecret(ctx, sandbox.BuildNamespace, sandbox.BuildSecretFor(p), "build-"+p, labels); err != nil {
			errs = append(errs, err)
		}
	}
	// ponytail: a removed project's build Secret stays in sbx-build; its account is gone from Zot with the namespace.
	return changed, errors.Join(errs...)
}

// applySecret writes a Secret without reading it first (sbx-build: the build pods' users' credentials live there),
// once per process unless its content changes.
func (r *RegistryUsers) applySecret(ctx context.Context, ns, name, user string, labels map[string]any) error {
	want := DockerConfig(r.Host, user, r.password(user))
	key := ns + "/" + name
	if r.applied[key] == string(want) {
		return nil
	}
	if err := r.API.ApplyCore(ctx, "secrets", map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels},
		"data":     map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(want)},
	}); err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	if r.applied == nil {
		r.applied = map[string]string{}
	}
	r.applied[key] = string(want)
	return nil
}

// ManagedByRegistry marks the pull Secrets this sync writes (the portal leaves them alone).
const ManagedByRegistry = "proxcloud-registry-users"

func (r *RegistryUsers) password(user string) string {
	if p, ok := r.Static[user]; ok {
		return p
	}
	return RegistryPassword(r.Key, user)
}

// htpasswd hashes each user once per process (bcrypt is slow on purpose) and again only if its password changes.
func (r *RegistryUsers) htpasswd(users []string) ([]byte, error) {
	if r.hashes == nil {
		r.hashes = map[string][2]string{}
	}
	var b bytes.Buffer
	for _, u := range users {
		pw := r.password(u)
		if pw == "" {
			return nil, fmt.Errorf("registry user %s has no password", u)
		}
		h, ok := r.hashes[u]
		if !ok || h[0] != pw {
			sum, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
			if err != nil {
				return nil, err
			}
			h = [2]string{pw, string(sum)}
			r.hashes[u] = h
		}
		fmt.Fprintf(&b, "%s:%s\n", u, h[1])
	}
	return b.Bytes(), nil
}

func (r *RegistryUsers) write(projects []string) error {
	users := []string{RegistryAdminUser, RegistryGCUser, RegistryNodesUser}
	for _, p := range projects {
		users = append(users, "pull-"+p, "build-"+p)
	}
	ht, err := r.htpasswd(users)
	if err != nil {
		return err
	}
	cfg, err := json.Marshal(r.Base)
	if err != nil {
		return err
	}
	var doc map[string]any // a deep copy of Base, so the next pass starts from it again
	if err := json.Unmarshal(cfg, &doc); err != nil {
		return err
	}
	httpCfg, _ := doc["http"].(map[string]any)
	if httpCfg == nil {
		httpCfg = map[string]any{}
		doc["http"] = httpCfg
	}
	httpCfg["auth"] = map[string]any{"htpasswd": map[string]any{"path": filepath.Join(r.Dir, "htpasswd")}, "failDelay": 1}
	httpCfg["accessControl"] = ZotAccess(projects)
	out, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return err
	}
	// In place, users first: Zot watches the config file and reads htpasswd when it reloads it.
	if err := os.WriteFile(filepath.Join(r.Dir, "htpasswd"), ht, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.Dir, "config.json"), out, 0o600)
}

// DockerConfig is the .dockerconfigjson of one registry account.
func DockerConfig(host, user, password string) []byte {
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]any{
		"username": user, "password": password, "auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + password)),
	}}})
	return b
}

func (r *RegistryUsers) ensureSecret(ctx context.Context, ns, name, user string, labels map[string]any) error {
	want := DockerConfig(r.Host, user, r.password(user))
	have, err := r.API.GetSecretData(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("read %s/%s: %w", ns, name, err)
	}
	if bytes.Equal(have[".dockerconfigjson"], want) {
		return nil
	}
	if labels == nil {
		labels = map[string]any{sandbox.LabelManagedBy: ManagedByRegistry}
	}
	if err := r.API.ApplyCore(ctx, "secrets", map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels},
		"data":     map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(want)},
	}); err != nil {
		return fmt.Errorf("write %s/%s: %w", ns, name, err)
	}
	return nil
}

// ReadRegistryUsersDir reads the static passwords and the key from files named after them (a mounted Secret).
func ReadRegistryUsersDir(dir string) (static map[string]string, key []byte, err error) {
	static = map[string]string{}
	for _, u := range []string{RegistryAdminUser, RegistryGCUser, RegistryNodesUser} {
		b, err := os.ReadFile(filepath.Join(dir, u))
		if err != nil {
			return nil, nil, err
		}
		if static[u] = strings.TrimSpace(string(b)); static[u] == "" {
			return nil, nil, fmt.Errorf("%s/%s is empty", dir, u)
		}
	}
	key, err = os.ReadFile(filepath.Join(dir, "key"))
	if err == nil && len(bytes.TrimSpace(key)) < 16 {
		err = fmt.Errorf("%s/key is shorter than 16 bytes", dir)
	}
	return static, bytes.TrimSpace(key), err
}
