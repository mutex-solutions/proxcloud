//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// The keepalived config of a load balancer pair must pass `keepalived -t` as Debian 13 (2.3.3) and Ubuntu 26.04
// (2.3.4) ship it, with no VIP, one and fifty (spike S1: an instance with every VIP excluded, or a rule without a
// preference, fails there). The container has one interface: the tenant side is eth0 and the public side lo; -t
// checks the syntax and the values, not the wiring.
func TestKeepalivedConfigPassesKeepalivedT(t *testing.T) {
	for _, image := range []string{"debian:trixie", "ubuntu:26.04"} {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{Image: image, Cmd: []string{"sleep", "600"}},
				Started:          true,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Terminate(context.Background()) })
			run := func(cmd string) string {
				t.Helper()
				code, r, err := c.Exec(ctx, []string{"sh", "-c", cmd})
				var out bytes.Buffer
				if r != nil {
					_, _ = io.Copy(&out, r)
				}
				if err != nil || code != 0 {
					t.Fatalf("%s: exit %d %v\n%s", cmd, code, err, out.String())
				}
				return out.String()
			}
			run("apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq keepalived >/dev/null")
			t.Log(run("keepalived --version 2>&1 | head -1"))
			// The scripts keepalived runs must exist, or -t disables the track script and fails.
			run("printf '#!/bin/sh\\n' > /usr/local/sbin/proxcloud-lb-check && printf '#!/bin/sh\\n' > /usr/local/sbin/proxcloud-lb-notify && chmod 0755 /usr/local/sbin/proxcloud-lb-*")
			// The control: keepalived -t here does refuse what the spike saw refused, a rule with no preference.
			bad, _ := pve.RenderKeepalived(pve.LBKeepalived{RouterID: 42, Self: "10.0.0.5", Peer: "10.0.0.6",
				TenantIface: "eth0", PublicIface: "lo", VIPs: []string{"203.0.113.10"}, Prefix: 24, Gateway: "203.0.113.1"})
			if err := c.CopyToContainer(ctx, []byte(strings.ReplaceAll(bad, " preference 100", "")), "/tmp/bad.conf", 0o644); err != nil {
				t.Fatal(err)
			}
			if code, _, err := c.Exec(ctx, []string{"keepalived", "-t", "-f", "/tmp/bad.conf"}); err != nil || code == 0 {
				t.Fatalf("keepalived -t took a rule with no preference: exit %d %v", code, err)
			}
			for _, n := range []int{0, 1, 50} {
				var vips []string
				for i := range n {
					vips = append(vips, fmt.Sprintf("203.0.113.%d", 10+i))
				}
				conf, err := pve.RenderKeepalived(pve.LBKeepalived{RouterID: 42, Self: "10.0.0.5", Peer: "10.0.0.6",
					TenantIface: "eth0", PublicIface: "lo", VIPs: vips, Prefix: 24, Gateway: "203.0.113.1"})
				if err != nil {
					t.Fatal(err)
				}
				if err := c.CopyToContainer(ctx, []byte(conf), "/tmp/k.conf", 0o644); err != nil {
					t.Fatal(err)
				}
				code, r, err := c.Exec(ctx, []string{"keepalived", "-t", "-f", "/tmp/k.conf"})
				var out bytes.Buffer
				if r != nil {
					_, _ = io.Copy(&out, r)
				}
				if err != nil || code != 0 {
					t.Errorf("%d VIPs: keepalived -t exit %d %v\n%s\n%s", n, code, err, out.String(), conf)
				} else if s := out.String(); strings.Contains(s, "WARNING") {
					t.Logf("%d VIPs: %s", n, s)
				}
			}
		})
	}
}
