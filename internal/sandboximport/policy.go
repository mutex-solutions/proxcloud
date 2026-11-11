package sandboximport

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Policy is operator-owned input, not caller request JSON. It is passed to the
// trusted worker as one bounded, non-secret argument; every worker revalidates it.
// SourceRepositories maps canonical HTTPS origins to EXACT repository names.
// The literal {project} may stand for the current, validated platform project ID.
type Policy struct {
	TemplateRegistry   string              `json:"templateRegistry"`
	SourceRepositories map[string][]string `json:"sourceRepositories,omitempty"`
	AllowAnonymous     bool                `json:"allowAnonymous,omitempty"`
}

const MaxPolicyBytes = 16 << 10

var templateRE = regexp.MustCompile(`^[a-f0-9]{20}$`)
var tagRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func validHost(host string) bool {
	u, err := url.Parse("https://" + host)
	if err != nil || u.Host != host || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.ToLower(host) != host {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return false
		}
	}
	// Do not permit alternate URL spellings or localhost implicit-HTTP behavior.
	return !strings.ContainsAny(host, "\\%@?# \t\r\n") && u.Hostname() != "localhost"
}

func repository(ref string) (host, path string, err error) {
	host, path, ok := strings.Cut(ref, "/")
	if !ok || !validHost(host) || !repoRE.MatchString(path) {
		return "", "", errors.New("invalid canonical registry repository")
	}
	return host, path, nil
}

func (p Policy) Validate() error {
	host, path, err := repository(p.TemplateRegistry)
	if err != nil || path != "t" || host == "" {
		return errors.New("imports require a configured canonical registry host/t prefix")
	}
	count := 0
	for origin, repos := range p.SourceRepositories {
		h, ok := strings.CutPrefix(origin, "https://")
		if !ok || !validHost(h) || len(repos) == 0 {
			return errors.New("import source policy requires canonical HTTPS origins and exact repositories")
		}
		for _, repo := range repos {
			count++
			if !repoRE.MatchString(strings.ReplaceAll(repo, "{project}", strings.Repeat("a", 24))) || strings.ContainsAny(repo, "@:") {
				return errors.New("invalid exact source repository policy")
			}
		}
	}
	b, _ := json.Marshal(p)
	if count > 64 || len(b) > MaxPolicyBytes {
		return errors.New("import source policy exceeds its bounded catalog")
	}
	return nil
}

func (p Policy) JSON() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	return string(b), err
}

func ParsePolicy(raw string) (Policy, error) {
	var p Policy
	if len(raw) > MaxPolicyBytes {
		return p, errors.New("import policy exceeds its budget")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, errors.New("invalid import policy")
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return p, errors.New("invalid trailing import policy")
	}
	return p, nil
}

func (p Policy) ManagedRepository(repo, project string) bool {
	if !idRE.MatchString(project) || p.Validate() != nil {
		return false
	}
	prefix := p.TemplateRegistry + "/" + project + "/"
	return strings.HasPrefix(repo, prefix) && templateRE.MatchString(strings.TrimPrefix(repo, prefix))
}

// Source validates ownership and exact operator allowlists BEFORE credential selection.
func (p Policy) Source(ref, project string) (bool, error) {
	if err := p.Validate(); err != nil {
		return false, err
	}
	if !idRE.MatchString(project) {
		return false, errors.New("invalid project")
	}
	if len(ref) > 512 {
		return false, errors.New("source reference exceeds 512 bytes")
	}
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok || !digestRE.MatchString(digest) {
		return false, errors.New("source must be an immutable repository@sha256:<64 lowercase hex> reference")
	}
	host, path, err := repository(repo)
	if err != nil {
		return false, err
	}
	if p.ManagedRepository(repo, project) {
		return true, nil
	}
	managedHost, _, _ := strings.Cut(p.TemplateRegistry, "/")
	// Another project's managed path cannot be turned into an external source policy.
	if host == managedHost && (path == "t" || strings.HasPrefix(path, "t/")) {
		return false, errors.New("managed source must belong to the current project")
	}
	for _, allowed := range p.SourceRepositories["https://"+host] {
		if strings.ReplaceAll(allowed, "{project}", project) == path {
			return false, nil
		}
	}
	return false, errors.New("source repository is not enabled by the operator import policy")
}

// Destination admits verify-in-place without a tag, or one explicit copy tag in
// the current project's managed repository. No userinfo/path-normalization aliases.
func (p Policy) Destination(source, project, destination string) error {
	managed, err := p.Source(source, project)
	if err != nil {
		return err
	}
	srcRepo, _, _ := strings.Cut(source, "@")
	if managed {
		if destination != srcRepo {
			return errors.New("managed verification must not change repository")
		}
		return nil
	}
	repo, tag, ok := strings.Cut(destination, ":")
	// Registry ports are not tag separators.
	i := strings.LastIndex(destination, ":")
	if i > strings.LastIndex(destination, "/") {
		repo, tag, ok = destination[:i], destination[i+1:], true
	} else {
		ok = false
	}
	if !ok || !tagRE.MatchString(tag) || !p.ManagedRepository(repo, project) {
		return errors.New("invalid project-owned tagged import destination")
	}
	return nil
}

func DestinationRepository(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}
