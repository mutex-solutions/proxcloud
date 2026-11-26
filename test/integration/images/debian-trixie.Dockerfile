# The guest of the real-guest load balancer tests on Debian 13 (test/integration/lb_guest_test.go): the
# distribution's own image with every package those tests would install from the mirror while they run. The packages
# are the distribution's, at the versions its mirror serves on the day of the build: nothing is pinned or held, and
# recommended packages come along as on an appliance (ipvsadm with keepalived). No service is started here; a test
# removes policy-rc.d and runs systemd as PID 1, in privileged mode, as it does on the plain image.
#
# openssh-server is left out on purpose: only TestLBApplianceServesWhatIsRecorded installs it, and that test runs on
# the plain image because the product's own install is its subject. Baked in, sshd would run in every other guest.
#
# The base is pinned by digest (Dependabot moves it, .github/dependabot.yml). REFRESH is part of the cache key of the
# layer below: CI passes the ISO week (test.yml), so the first run of a week installs from the mirror again and every
# other run takes the layer from the cache.
FROM debian:trixie@sha256:3cda3ca472ce45507a5109c6a3907093e24d9ae67d55954c9a07a45665dafba2
ARG REFRESH=local
RUN set -eu; export DEBIAN_FRONTEND=noninteractive; \
    pkgs="systemd-sysv procps iproute2 socat openssl tcpdump nftables keepalived haproxy"; \
    n=1; until apt-get update -qq && apt-get install -y -qq $pkgs >/dev/null; do \
      [ "$n" -lt 3 ] || { echo "mirror: apt-get did not install [$pkgs] in 3 tries (refresh $REFRESH): if the lines above are fetch errors, the distribution's mirror did not answer" >&2; exit 1; }; \
      n=$((n+1)); sleep 10; \
    done; \
    rm -rf /var/lib/apt/lists/*; \
    { echo "refresh $REFRESH"; dpkg-query -W -f='${Package} ${Version}\n' $pkgs systemd ipvsadm 2>&1 || true; } | tee /etc/proxcloud-lb-test-image
