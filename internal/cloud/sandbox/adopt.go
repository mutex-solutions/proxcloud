package sandbox

import (
	"context"
	"sort"
	"strings"
)

// PrepullNamespace and PrepullSelector find the DaemonSets that pre-pull the built-in template
// images onto every node (infra-ops: sandbox-az1 apps/node-runtime/template-prepull.yaml).
const (
	PrepullNamespace = "sandbox-system"
	PrepullSelector  = "app in (template-prepull,template-prepull-gpu)"
)

// Adoption is a template whose digest-pinned image the prepull DaemonSets now pull at another
// digest. Ready means every prepull pod listing that repository (on a Ready node) runs and has
// pulled To, so a sandbox started from To will not wait for a download.
type Adoption struct {
	Template, From, To string
	Pulled, Total      int
	Ready              bool
}

// splitRef splits "host/path:tag@sha256:d" into ("host/path", "sha256:d"); digest "" when the
// ref is not digest-pinned.
func splitRef(ref string) (repo, digest string) {
	at := strings.Index(ref, "@")
	if at < 0 {
		return "", ""
	}
	repo, digest = ref[:at], ref[at+1:]
	if c := strings.LastIndex(repo, ":"); c > strings.LastIndex(repo, "/") {
		repo = repo[:c]
	}
	return repo, digest
}

// Adoptions compares the templates' images with what the prepull pods run. Tag-only refs,
// another repository and template names with a "." (a config path segment) are left alone.
func Adoptions(tpls map[string]Template, pods []map[string]any, notReady map[string]bool) []Adoption {
	type seen struct {
		ref    string // the spec image, verbatim
		pulled int
	}
	total := map[string]int{}                 // repo -> prepull pods listing it
	byDigest := map[string]map[string]*seen{} // repo -> digest -> pods
	for _, p := range pods {
		if notReady[digStr(p, "spec", "nodeName")] || !ownedByDaemonSet(p) {
			continue
		}
		running := digStr(p, "status", "phase") == "Running"
		imageID := map[string]string{}
		statuses, _ := dig(p, "status", "initContainerStatuses").([]any)
		for _, s := range statuses {
			if m, ok := s.(map[string]any); ok {
				imageID[str(m["name"])] = str(m["imageID"])
			}
		}
		inits, _ := dig(p, "spec", "initContainers").([]any)
		for _, c := range inits {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			ref := str(m["image"])
			repo, d := splitRef(ref)
			if d == "" {
				continue
			}
			total[repo]++
			if byDigest[repo] == nil {
				byDigest[repo] = map[string]*seen{}
			}
			s := byDigest[repo][d]
			if s == nil {
				s = &seen{ref: ref}
				byDigest[repo][d] = s
			}
			if running && strings.HasSuffix(imageID[str(m["name"])], "@"+d) {
				s.pulled++
			}
		}
	}
	var out []Adoption
	for name, t := range tpls {
		repo, cur := splitRef(t.Image)
		if cur == "" || strings.Contains(name, ".") {
			continue
		}
		for d, s := range byDigest[repo] {
			if d == cur {
				continue
			}
			out = append(out, Adoption{Template: name, From: t.Image, To: s.ref, Pulled: s.pulled, Total: total[repo],
				Ready: s.pulled == total[repo]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Template+out[i].To < out[j].Template+out[j].To })
	return out
}

func ownedByDaemonSet(p map[string]any) bool {
	refs, _ := dig(p, "metadata", "ownerReferences").([]any)
	for _, r := range refs {
		if m, ok := r.(map[string]any); ok && m["kind"] == "DaemonSet" {
			return true
		}
	}
	return false
}

// TemplateAdoptions reads the prepull pods and the nodes, then compares (Adoptions).
func (s *Service) TemplateAdoptions(ctx context.Context) ([]Adoption, error) {
	pods, err := s.api.ListPods(ctx, PrepullNamespace, PrepullSelector)
	if err != nil {
		return nil, err
	}
	nodes, err := s.api.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	notReady := map[string]bool{}
	for _, n := range nodes {
		if !conditionTrue(n, "Ready") {
			notReady[digStr(n, "metadata", "name")] = true
		}
	}
	return Adoptions(s.cfg.Templates, pods, notReady), nil
}
