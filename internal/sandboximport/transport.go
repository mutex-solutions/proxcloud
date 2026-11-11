package sandboximport

import (
	"net/http"
	"regexp"
	"strings"
)

var uploadIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)

// authorizedRegistryPath matches registry endpoint suffix grammar after extracting
// the FULL repository. A nested repository is not a child permission of its prefix.
func (t GuardTransport) authorizedRegistryPath(r *http.Request) bool {
	q := r.URL.Query()
	if r.URL.Path == "/service/token" {
		for k := range q {
			if k != "scope" && k != "service" && k != "account" && k != "client_id" && k != "offline_token" {
				return false
			}
		}
		for _, sc := range q["scope"] {
			parts := strings.Split(sc, ":")
			if len(parts) != 3 || parts[0] != "repository" || !t.Repositories[r.URL.Host+"/"+parts[1]] {
				return false
			}
			for _, action := range strings.Split(parts[2], ",") {
				if action != "pull" && action != "push" {
					return false
				}
			}
		}
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	}
	if r.URL.Path == "/v2/" {
		return (r.Method == http.MethodGet || r.Method == http.MethodHead) && len(q) == 0
	}
	for repo := range t.Repositories {
		host, p, _ := strings.Cut(repo, "/")
		if host != r.URL.Host {
			continue
		}
		suffix, ok := strings.CutPrefix(r.URL.Path, "/v2/"+p+"/")
		if !ok {
			continue
		}
		for k, values := range q {
			if len(values) != 1 || len(values[0]) > 8192 {
				return false
			}
			switch k {
			case "digest":
				if !digestRE.MatchString(values[0]) {
					return false
				}
			case "_state", "state": // opaque upload transaction state only
			default:
				return false
			}
		}
		if ref, ok := strings.CutPrefix(suffix, "manifests/"); ok {
			return (tagRE.MatchString(ref) || digestRE.MatchString(ref)) && !strings.Contains(ref, "/") && len(q) == 0 && (r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodPut)
		}
		if digest, ok := strings.CutPrefix(suffix, "blobs/"); ok && digestRE.MatchString(digest) {
			return len(q) == 0 && (r.Method == http.MethodGet || r.Method == http.MethodHead)
		}
		if suffix == "blobs/uploads/" || suffix == "blobs/uploads" {
			return r.Method == http.MethodPost && (len(q) == 0 || (len(q) == 1 && q.Get("digest") != ""))
		}
		if id, ok := strings.CutPrefix(suffix, "blobs/uploads/"); ok && uploadIDRE.MatchString(id) {
			return r.Method == http.MethodPatch || r.Method == http.MethodPut || r.Method == http.MethodGet || r.Method == http.MethodDelete
		}
	}
	return false
}
