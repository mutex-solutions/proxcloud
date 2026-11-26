//go:build integration

package integration

import (
	"context"
	"io"
	"os"
	"strings"
	"time"
)

// lbBakedImage names, for each distribution an appliance is built from, the variable that holds the image of
// test/integration/images: the distribution's own image with the packages the real-guest tests need already in it.
// CI builds the two once per run and sets both (test.yml), so that no test waits for the distribution's mirror or
// fails with it. Unset, a test runs on the plain image and installs what it needs while it runs, as every test did
// before.
var lbBakedImage = map[string]string{"debian:trixie": "LB_TEST_IMAGE_DEBIAN", "ubuntu:26.04": "LB_TEST_IMAGE_UBUNTU"}

// lbImage answers the image a test runs its guest of distro in: the baked one where its variable names one.
//
// Two tests never ask, because the product's own install on an image that has nothing is what they show:
// TestLBApplianceServesWhatIsRecorded (the pair's install line and, at its end, the UDP service's) and
// TestLBSingleApplianceInstalls (the single appliance's line). They run on the plain image whatever is set, read no
// variable and have no skip, so a run cannot leave an install line out.
func lbImage(distro string) string {
	if image := os.Getenv(lbBakedImage[distro]); image != "" {
		return image
	}
	return distro
}

// lbInstall is the shell that makes sure the guest has packages. Nothing runs when dpkg has them all, which is the
// baked image. On the plain image the missing ones come from the distribution's mirror, in three tries, and a
// failure says "mirror" under apt's own lines. A baked image that lacks one fails and names the files to add it to:
// it would otherwise install from the mirror again, in every run, unseen.
func lbInstall(packages ...string) string {
	return `m=; for p in ` + strings.Join(packages, " ") + `; do [ "$(dpkg-query -W -f='${Status}' $p 2>/dev/null)" = 'install ok installed' ] || m="$m $p"; done
if [ -n "$m" ]; then
  [ ! -e /etc/proxcloud-lb-test-image ] || { echo "the baked image lacks$m: add it to both files of test/integration/images and build them again" >&2; exit 1; }
  n=1; until apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq $m >/dev/null; do
    [ $n -lt 3 ] || { echo "mirror: apt-get did not install$m in 3 tries: where the lines above are fetch errors, the distribution's mirror did not answer" >&2; exit 100; }
    n=$((n+1)); sleep 5
  done
fi`
}

// lbInit is the command of a guest that runs systemd as PID 1, with packages in it. No policy-rc.d: on a VM a
// package's postinst starts its service.
func lbInit(packages ...string) []string {
	return []string{"sh", "-c", lbInstall(packages...) + "\nrm -f /usr/sbin/policy-rc.d && exec /sbin/init"}
}

// mirror is the end of the failure message of a step that waited for the product's install, the one thing here
// that needs the distribution's mirror: it says so when the mirror does not answer the guest. (Lists of its own: the
// product's apt-get may hold the lock of the guest's.)
func (g *guestAgent) mirror(ctx context.Context) string {
	code, out, _ := g.sh(ctx, "mirror", "sh", "-c", "apt-get update -qq -o APT::Update::Error-Mode=any -o Dir::State::lists=/tmp/lb-mirror-probe 2>&1")
	if code == 0 {
		return ""
	}
	return "\nmirror: apt-get update fails in the guest: the distribution's mirror did not answer the product's install, which is not the test's doing\n" + out
}

// booted waits until systemd is up in the guest. A guest whose command ended never comes up: its output says why,
// and holds the word "mirror" when apt could not install (lbInstall).
func (g *guestAgent) booted(ctx context.Context) {
	g.t.Helper()
	for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(2 * time.Second) {
		if code, out, _ := g.sh(ctx, "boot", "systemctl", "is-system-running"); code == 0 || strings.TrimSpace(out) == "degraded" {
			return
		}
		st, err := g.c.State(ctx)
		if gone := err == nil && !st.Running; gone || time.Now().After(deadline) {
			logs := ""
			if r, err := g.c.Logs(ctx); err == nil {
				b, _ := io.ReadAll(io.LimitReader(r, 1<<16))
				logs = string(b)
			}
			why := "in 5 minutes"
			if gone {
				why = "its command ended"
			}
			g.t.Fatalf("systemd did not come up in the container: %s\n%s", why, logs)
		}
	}
}
