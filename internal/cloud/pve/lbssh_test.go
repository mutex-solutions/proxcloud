package pve

import (
	"os"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbguesttest"
)

// Every script that reaches an appliance opens by masking sshd: the install of a new one, and each apply, which is
// how an appliance of an older release gets it (R4-o1 of v0.11.1-rc.2).
func TestEveryPushMasksSSH(t *testing.T) {
	// The line as the shell runs it: both units are disabled, then masked, and it answers 0 whatever systemctl says.
	for _, shell := range lbguesttest.Shells() {
		g := lbguesttest.New(t)
		if exit, _, stderr := g.Run(shell, lbNoSSH+"exit 0"); exit != 0 {
			t.Fatalf("%s: the line that masks sshd: exit %d %s", shell, exit, stderr)
		}
		calls, _ := os.ReadFile(g.Path("stub/systemctl-calls"))
		if string(calls) != "disable --now ssh.service ssh.socket\nmask --now ssh.service ssh.socket\n" {
			t.Errorf("%s: the line that masks sshd asked systemctl:\n%s", shell, calls)
		}
	}
	for name, script := range map[string]string{"haproxy apply": haproxyReload, "keepalived apply": lbKeepalivedApply, "udp apply": lvsApply,
		"install": haproxyInstall, "pair install": lbPairInstall, "udp install": lvsInstall} {
		if !strings.HasPrefix(script, lbNoSSH) {
			t.Errorf("the %s does not open with the mask of sshd", name)
		}
	}
}
