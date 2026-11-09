package sandboximport

import (
	"strings"
	"testing"
)

func TestPolicyDefaultsAndExactProjectScope(t *testing.T) {
	p := Policy{TemplateRegistry: Registry}
	own := Registry + "/" + project + "/" + template + "@" + hash
	if managed, err := p.Source(own, project); err != nil || !managed {
		t.Fatal(managed, err)
	}
	for _, ref := range []string{
		Registry + "/ffffffffffffffffffffffff/" + template + "@" + hash,
		Registry + "/" + project + "/bad@" + hash,
		"registry.example.test/migration/app@" + hash,
		"https://" + own, "u:p@" + own, strings.Replace(own, "/t/", "/t//", 1),
		strings.Replace(own, "/t/", "/t/../", 1), strings.Replace(own, "registry.example.test/", "registry.example.test.evil/", 1),
	} {
		if _, err := p.Source(ref, project); err == nil {
			t.Errorf("accepted untrusted source %q", ref)
		}
	}
	p.SourceRepositories = map[string][]string{"https://registry.example.test": {"t/ffffffffffffffffffffffff/" + template}}
	if _, err := p.Source(Registry+"/ffffffffffffffffffffffff/"+template+"@"+hash, project); err == nil {
		t.Fatal("external policy bypassed project ownership")
	}
}

func TestPolicyCanonicalOriginsAndBoundedExactRepositories(t *testing.T) {
	for _, origin := range []string{"http://example.test", "https://u:p@example.test", "https://example.test/", "https://example.test/path", "https://example.test?x=1", "https://example.test#x", "https://EXAMPLE.test", "https://example.test:000443"} {
		p := Policy{TemplateRegistry: Registry, SourceRepositories: map[string][]string{origin: {"migration/app"}}}
		if p.Validate() == nil {
			t.Errorf("accepted origin %q", origin)
		}
	}
	p := Policy{TemplateRegistry: Registry, SourceRepositories: map[string][]string{"https://source.example.test:5443": {"migration/{project}/app"}}}
	raw, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParsePolicy(raw); err != nil {
		t.Fatal(err)
	}
	if _, err = ParsePolicy(raw + " {}"); err == nil {
		t.Fatal("trailing policy accepted")
	}
	if _, err = p.Source("source.example.test:5443/migration/"+project+"/app@"+hash, project); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Source("source.example.test:5443/migration/"+project+"/app/sub@"+hash, project); err == nil {
		t.Fatal("repo prefix accepted")
	}
	p.SourceRepositories = map[string][]string{"https://source.example.test": {"migration/*"}}
	if p.Validate() == nil {
		t.Fatal("wildcard accepted")
	}
}

func TestImportDestinationIsAuthoritativeTaggedCopyOrOwnedVerify(t *testing.T) {
	own := Registry + "/" + project + "/" + template
	src := "registry.example.test/migration/app@" + hash
	for _, dest := range []string{own, own + "@" + hash, strings.Replace(own, "/"+project+"/", "/ffffffffffffffffffffffff/", 1) + ":build", own + ":build/evil"} {
		if policy.Destination(src, project, dest) == nil {
			t.Errorf("accepted copy destination %q", dest)
		}
	}
	if err := policy.Destination(src, project, own+":build-1"); err != nil {
		t.Fatal(err)
	}
	if err := policy.Destination(own+"@"+hash, project, own); err != nil {
		t.Fatal(err)
	}
	if err := policy.Destination(own+"@"+hash, project, own+":build-1"); err == nil {
		t.Fatal("verify changed repo")
	}
}
