package pve

import (
	"encoding/base64"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files under testdata")

// The root command of an installation, as generated: one golden per variant (go test ./internal/cloud/pve -run
// EnrollScriptGolden -update rewrites them). No line has a `!` outside single quotes: the command is pasted into an
// interactive shell.
func TestEnrollScriptGolden(t *testing.T) {
	for name, gen := range map[string]func() (string, error){
		"enroll-new": func() (string, error) {
			return EnrollScript(DefaultPlatformTokenID, false, "", "local-lvm", "local", Enrollment{InstallationID: "pi-0a1b2c3d"})
		},
		"enroll-held": func() (string, error) {
			return EnrollScript("proxcloud@pve!m0", true, "nas-templates", "pcplat", "local", Enrollment{InstallationID: "pi-0a1b2c3d", HaveCAPI: true, HaveCSI: true})
		},
		"enroll-capi-held": func() (string, error) {
			return EnrollScript(DefaultPlatformTokenID, false, "local", "pcplat", "local", Enrollment{InstallationID: "pi-0a1b2c3d", HaveCAPI: true})
		},
	} {
		got, err := gen()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, l := range strings.Split(got, "\n") {
			if bangOutsideSingleQuotes(l) {
				t.Errorf("%s line %d has a ! outside single quotes: %s", name, i+1, l)
			}
		}
		path := filepath.Join("testdata", "setupscript", name+".sh")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs from %s (run with -update after checking the change). Generated:\n%s", name, path, got)
		}
	}
	// Without an installation the managed-services script is what it was: no marker, no stop, the secrets printed.
	old, err := ManagedSetupScript(DefaultPlatformTokenID, false, "", "local-lvm", "local")
	if err != nil || strings.Contains(old, "PC_MARK") || strings.Contains(old, "PC-ENROLL") ||
		!strings.Contains(old, "pveum user token add 'capmox@pve' 'capi' --privsep 1 || echo ") {
		t.Fatalf("the script without an installation changed: %v\n%s", err, old)
	}
	for _, id := range []string{"", "pi-1; id", "pi-1'", "pi:1", "$(id)", "pi 1", "-pi", "pi\n1"} {
		if s, err := EnrollScript(DefaultPlatformTokenID, false, "", "local-lvm", "local", Enrollment{InstallationID: id}); err == nil {
			t.Errorf("installation id %q accepted:\n%s", id, s)
		}
	}
}

const (
	fakeCAPISecret = "11111111-1111-4111-8111-111111111111"
	fakeCSISecret  = "22222222-2222-4222-8222-222222222222"
)

// runEnroll runs the root command as on a node, with a pveum that records its arguments, answers `user token list`
// from lists (user → JSON) and `user token add` with a secret per token name (lists["fail-csi"] set: the CSI token's
// add fails).
func runEnroll(t *testing.T, e Enrollment, lists map[string]string) (out, calls string, err error) {
	t.Helper()
	for _, bin := range []string{"sh", "perl", "base64"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s", bin)
		}
	}
	script, err := EnrollScript(DefaultPlatformTokenID, false, "", "local-lvm", "local", e)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	fake := `#!/bin/sh
echo "$*" >> ` + log + `
case "$1 $2 $3" in
"acl list "*) printf '[]' ;;
"user token list")
  case "$4" in
  capmox@pve) [ -n "$FAKE_LIST_CAPMOX" ] || exit 2; printf '%s' "$FAKE_LIST_CAPMOX" ;;
  csi@pve) [ -n "$FAKE_LIST_CSI" ] || exit 2; printf '%s' "$FAKE_LIST_CSI" ;;
  esac ;;
"user token add")
  case "$5" in
  capi) printf '{"full-tokenid":"capmox@pve!capi","info":{"privsep":1},"value":"` + fakeCAPISecret + `"}' ;;
  csi) [ -z "$FAKE_FAIL_CSI" ] || exit 1; printf '{"full-tokenid":"csi@pve!csi","info":{"privsep":1},"value":"` + fakeCSISecret + `"}' ;;
  *) exit 1 ;;
  esac ;;
esac
`
	for _, bin := range []string{"pveum", "pvenode"} {
		if err := os.WriteFile(filepath.Join(dir, bin), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "FAKE_LIST_CAPMOX=" + lists["capmox@pve"], "FAKE_LIST_CSI=" + lists["csi@pve"], "FAKE_FAIL_CSI=" + lists["fail-csi"]}
	b, err := cmd.CombinedOutput()
	c, _ := os.ReadFile(log)
	return string(b), string(c), err
}

var enrollLine = regexp.MustCompile(`(?m)^PC-ENROLL:([A-Za-z0-9+/=]+)$`)

// payload is the decoded enrollment line of out ("" when it prints none); more than one line fails.
func payload(t *testing.T, out string) string {
	t.Helper()
	m := enrollLine.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		return ""
	}
	if len(m) > 1 {
		t.Fatalf("more than one enrollment line:\n%s", out)
	}
	b, err := base64.StdEncoding.DecodeString(m[0][1])
	if err != nil {
		t.Fatalf("the enrollment line is not base64: %v\n%s", err, out)
	}
	return string(b)
}

func TestEnrollScriptRuns(t *testing.T) {
	const id = "pi-0a1b2c3d"
	mark := "proxcloud:" + id
	payloadRE := func(capi, csi string) *regexp.Regexp {
		return regexp.MustCompile(`^1:` + id + `:[0-9]{9,}:` + capi + `:` + csi + `$`)
	}

	t.Run("a new site: both tokens made and marked, one line, no secret in clear", func(t *testing.T) {
		out, calls, err := runEnroll(t, Enrollment{InstallationID: id}, nil) // no user yet: the list fails
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if p := payload(t, out); !payloadRE(fakeCAPISecret, fakeCSISecret).MatchString(p) {
			t.Fatalf("payload %q\n%s", p, out)
		}
		if strings.Contains(out, fakeCAPISecret) || strings.Contains(out, fakeCSISecret) {
			t.Errorf("a secret is printed in clear:\n%s", out)
		}
		if !strings.HasPrefix(strings.TrimSpace(out[strings.LastIndex(out, "PC-ENROLL:"):]), "PC-ENROLL:") || strings.Contains(strings.TrimSpace(out[strings.LastIndex(out, "PC-ENROLL:"):]), "\n") {
			t.Errorf("the enrollment line is not the last line:\n%s", out)
		}
		for _, want := range []string{"user token add capmox@pve capi --privsep 1 --comment " + mark + " --output-format json",
			"user token add csi@pve csi --privsep 1 --comment " + mark + " --output-format json"} {
			if !strings.Contains(calls, want+"\n") {
				t.Errorf("missing call %q in\n%s", want, calls)
			}
		}
	})

	t.Run("a step that fails after a token was made still hands that token's secret back", func(t *testing.T) {
		out, _, err := runEnroll(t, Enrollment{InstallationID: id}, map[string]string{"fail-csi": "1"})
		if err == nil {
			t.Fatalf("the command did not fail:\n%s", out)
		}
		if p := payload(t, out); !payloadRE(fakeCAPISecret, "").MatchString(p) {
			t.Fatalf("payload %q\n%s", p, out)
		}
	})

	t.Run("a foreign token stops the command before any change", func(t *testing.T) {
		out, calls, err := runEnroll(t, Enrollment{InstallationID: id}, map[string]string{
			"capmox@pve": `[{"tokenid":"capi","privsep":0,"comment":"cluster-api of the homelab"}]`, "csi@pve": `[]`})
		if err == nil {
			t.Fatalf("the command went on:\n%s", out)
		}
		for _, want := range []string{"STOP: the token capmox@pve!capi exists on this cluster and was not made for this ProxCloud installation (" + id + ").",
			"\n  pveum user token remove capmox@pve capi\n", "Nothing was changed."} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in\n%s", want, out)
			}
		}
		if strings.Contains(out, "csi@pve!csi") || payload(t, out) != "" {
			t.Errorf("it names the token that is not there, or prints a line:\n%s", out)
		}
		for _, l := range strings.Split(strings.TrimSpace(calls), "\n") {
			if !strings.HasPrefix(l, "user token list ") {
				t.Errorf("a call other than a read before the stop: %s", l)
			}
		}
	})

	t.Run("a token without the marker whose secret the portal holds is marked, not stopped on and not remade", func(t *testing.T) {
		out, calls, err := runEnroll(t, Enrollment{InstallationID: id, HaveCAPI: true}, map[string]string{
			"capmox@pve": `[{"tokenid":"capi","privsep":1}]`, "csi@pve": `[]`})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if p := payload(t, out); !payloadRE("", fakeCSISecret).MatchString(p) {
			t.Fatalf("payload %q\n%s", p, out)
		}
		if !strings.Contains(calls, "user token modify capmox@pve capi --comment "+mark+"\n") || strings.Contains(calls, "user token add capmox@pve") ||
			strings.Contains(calls, "token remove") {
			t.Errorf("calls:\n%s", calls)
		}
	})

	t.Run("a second run changes no token; a kept token the portal does not hold is named with its command", func(t *testing.T) {
		ours := map[string]string{"capmox@pve": `[{"tokenid":"capi","privsep":1,"comment":"` + mark + `"}]`,
			"csi@pve": `[{"tokenid":"other","comment":"x"},{"tokenid":"csi","privsep":1,"comment":"` + mark + `"}]`}
		out, calls, err := runEnroll(t, Enrollment{InstallationID: id, HaveCAPI: true}, ours)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if payload(t, out) != "" || !strings.Contains(out, "No token was made, so there is no line to paste.") {
			t.Errorf("a line without a new token:\n%s", out)
		}
		if !strings.Contains(out, "The token csi@pve!csi was made for this installation by an earlier run, and ProxCloud does not hold its secret.") ||
			!strings.Contains(out, "\n  pveum user token remove csi@pve csi\n") || strings.Contains(out, "pveum user token remove capmox@pve capi") {
			t.Errorf("the kept tokens:\n%s", out)
		}
		if strings.Contains(calls, "token add capmox@pve") || strings.Contains(calls, "token add csi@pve") || strings.Contains(calls, "token modify") || strings.Contains(calls, "token remove") {
			t.Errorf("a token was changed:\n%s", calls)
		}
	})
}
