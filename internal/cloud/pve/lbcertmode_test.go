package pve

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbguesttest"
)

// A certificate file holds a key, and never exists with a mode wider than 0600: it is made empty with that mode
// before the agent writes into it, and the agent leaves the mode of a file that exists (R3-o4 of v0.11.1-rc.2: the
// agent created it with 0666 and the chmod came a command later). The agent here creates and overwrites as the real
// one does; the real guest's file system and a watcher are in test/integration/lb_tls_test.go.
func TestCertificateFileIsNeverWiderThan0600(t *testing.T) {
	shells := lbguesttest.Shells()
	if len(shells) == 0 {
		t.Skip("no dash or bash on this machine")
	}
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			ctx := context.Background()
			ps, g, a := aptGuest(t, shell, "global\n")
			cert := LBCertFile{Name: strings.Repeat("a1", 32), PEM: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"}
			wrote, err := ps.PushHAProxy(ctx, "n1", 100, scriptHAProxy, cert)
			if err != nil || wrote != 1 {
				t.Fatalf("push with a certificate: wrote %d, %v", wrote, err)
			}
			i := slices.Index(a.Writes, lbCertDir+"/"+cert.Name+".pem.new")
			if i < 0 || a.Modes[i] != 0o600 {
				t.Fatalf("the certificate file as the agent's write left it: %v %v", a.Writes, a.Modes)
			}
			// The control: a file the agent creates itself, in the same directory, is 0666. Without the step that
			// makes the file first, that is the certificate's.
			if err := ps.C.GuestWriteFile(ctx, "n1", 100, lbCertDir+"/control.new", "x"); err != nil {
				t.Fatal(err)
			}
			if got := a.Modes[len(a.Modes)-1]; got != 0o666 {
				t.Fatalf("a file the agent creates has mode %o here: the test shows nothing", got)
			}
			if fi, err := os.Stat(g.Path(lbCertDir)); err != nil || fi.Mode().Perm() != 0o700 {
				t.Errorf("the certificate directory: %v", err)
			}
		})
	}
}
