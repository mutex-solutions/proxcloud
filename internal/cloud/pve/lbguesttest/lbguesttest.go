// Package lbguesttest runs the shell scripts the product sends to a load balancer appliance, as the text they are,
// in a shell of this machine. A fake that answers a script from what the script is meant to say cannot see a mistake
// in the script: the state read of v0.11.1-rc.1 lost its answer to "holds the address" to a helper's loop variable,
// and every fake said 1 (drill Q1-2). Here the text runs: the guest's files live under a directory of the test's, the
// script's paths are pointed there, and the guest's commands the scripts ask (systemctl, ss, ip, nft, HAProxy's
// master socket through socat) are a few lines of shell that answer from files. awk, grep, sha256sum, stat and the
// shell itself are this machine's.
//
// What it does not show: the guest's own systemd, keepalived, HAProxy and kernel. Those are the real-guest container
// tests' (test/integration lb_appliance_test.go, lb_udp_test.go).
package lbguesttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Shells lists the shells of this machine a script is run with: dash is /bin/sh on both images an appliance is built
// from, bash is what a contributor's machine may call sh.
func Shells() []string {
	var out []string
	for _, sh := range []string{"dash", "bash"} {
		if _, err := exec.LookPath(sh); err == nil {
			out = append(out, sh)
		}
	}
	return out
}

// Available says whether this machine can run the scripts at all: a POSIX shell and the GNU tools they call.
func Available() bool {
	for _, tool := range []string{"sh", "awk", "grep", "sha256sum", "stat", "cut"} {
		if _, err := exec.LookPath(tool); err != nil {
			return false
		}
	}
	return true
}

// Guest is an appliance's guest as a script sees it.
type Guest struct {
	tb   testing.TB
	root string
}

// stubs are the guest's commands. Each answers from a file of the guest's "stub" directory, which a test writes
// with Stub: no file is the command failing (a stopped service, a socket that refuses, a table that is not loaded).
var stubs = map[string]string{
	// "is-active": the units listed in stub/active are. Every other verb succeeds and does nothing.
	"systemctl": `for a; do u=$a; done
case " $* " in *" is-active "*) grep -qxF -e "$u" "$R/stub/active" 2>/dev/null ;; esac`,
	"ss":  `cat "$R/stub/ss" 2>/dev/null`,
	"ip":  `cat "$R/stub/ip" 2>/dev/null`,
	"nft": `cat "$R/stub/nft" 2>/dev/null`,
	// HAProxy's master socket. stub/socat-refuses holds how many more connections it refuses first: a reload.
	"socat": `cat >/dev/null
n=$(cat "$R/stub/socat-refuses" 2>/dev/null || echo 0)
if [ "$n" -gt 0 ]; then echo $((n-1)) > "$R/stub/socat-refuses"; exit 1; fi
echo x >> "$R/stub/socat-calls"
cat "$R/stub/socat" 2>/dev/null`,
}

// Agent is the guest agent API of the guest, as far as the product's pushes and reads use it: exec runs the command
// here (sh is Shell), file-write puts the file, exec-status answers at once. Writes lists the paths written, in order.
type Agent struct {
	Guest  *Guest
	Shell  string
	mu     sync.Mutex
	runs   []map[string]any
	Writes []string
}

func (a *Agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = r.ParseForm()
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent/exec"):
		args := r.Form["command"]
		var exit int
		var out, stderr string
		if len(args) >= 3 && args[0] == "sh" && args[1] == "-c" {
			exit, out, stderr = a.Guest.Run(a.Shell, args[2], args[3:]...)
		} else { // a plain command: mkdir -p of the guest's directories
			for i := range args[1:] {
				args[i+1] = a.Guest.Script(args[i+1])
			}
			exit, out, stderr = a.Guest.Run(a.Shell, `"$@"`, args...)
		}
		a.runs = append(a.runs, map[string]any{"exited": 1, "exitcode": exit, "out-data": out, "err-data": stderr})
		write(map[string]any{"pid": len(a.runs)})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agent/exec-status"):
		pid, _ := strconv.Atoi(r.Form.Get("pid"))
		write(a.runs[pid-1])
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent/file-write"):
		a.Writes = append(a.Writes, r.Form.Get("file"))
		a.Guest.Write(r.Form.Get("file"), r.Form.Get("content"))
		write(nil)
	default:
		a.Guest.tb.Errorf("reached PVE: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// New is an empty guest: no file, no unit running, nothing listening.
func New(tb testing.TB) *Guest {
	tb.Helper()
	if !Available() {
		tb.Skip("this machine lacks a POSIX shell or the GNU tools the appliance's scripts call")
	}
	g := &Guest{tb: tb, root: tb.TempDir()}
	for _, dir := range []string{"bin", "stub"} {
		if err := os.MkdirAll(filepath.Join(g.root, dir), 0o755); err != nil {
			tb.Fatal(err)
		}
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(g.root, "bin", name), []byte("#!/bin/sh\nR='"+g.root+"'\n"+body+"\n"), 0o755); err != nil {
			tb.Fatal(err)
		}
	}
	return g
}

// Path is where a file of the guest's is on this machine.
func (g *Guest) Path(path string) string { return filepath.Join(g.root, path) }

// Write puts a file on the guest, by the path the guest has it under.
func (g *Guest) Write(path, content string) {
	g.tb.Helper()
	p := g.Path(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		g.tb.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		g.tb.Fatal(err)
	}
}

// Remove takes a file off the guest.
func (g *Guest) Remove(path string) { _ = os.Remove(g.Path(path)) }

// Stub sets what a command of the guest's answers (see stubs).
func (g *Guest) Stub(name, content string) { g.Write("stub/"+name, content) }

// Unstub makes a command of the guest's fail.
func (g *Guest) Unstub(name string) { g.Remove("stub/" + name) }

// Listen is what ss says: HAProxy listening on these addresses ("203.0.113.10:80").
func (g *Guest) Listen(addrs ...string) {
	var b strings.Builder
	for _, a := range addrs {
		b.WriteString("LISTEN 0 4096 " + a + " 0.0.0.0:* users:((\"haproxy\",pid=7,fd=5))\n")
	}
	g.Stub("ss", b.String())
}

// guestPath matches an absolute path of the guest's in a script: one that starts a word.
var guestPath = regexp.MustCompile(`(^|[\s=<>("':])/(etc|run|proc|var|usr/local)/`)

// Script is a script with the guest's paths pointed at this guest.
func (g *Guest) Script(script string) string {
	return guestPath.ReplaceAllString(script, "${1}"+g.root+"/${2}/")
}

// Run runs a script of the product's in shell, as the guest agent would with "sh -c", and answers its exit code and
// what it wrote.
func (g *Guest) Run(shell, script string, args ...string) (exit int, stdout, stderr string) {
	g.tb.Helper()
	cmd := exec.Command(shell, append([]string{"-c", g.Script(script)}, args...)...)
	cmd.Dir = g.root
	cmd.Env = []string{"PATH=" + filepath.Join(g.root, "bin") + ":/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		exit = ee.ExitCode()
	case err != nil:
		g.tb.Fatalf("%s: %v", shell, err)
	}
	return exit, out.String(), errOut.String()
}
