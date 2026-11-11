#!/usr/bin/env bash
# Runs as root only after upstream provisioning, reboot and goss have succeeded.
# Do not apply/restart networking or power off here: Packer must remove its SSH script.
# The Proxmox builder owns the later VM stop and template conversion phase.
set -euo pipefail
[ "$(id -u)" = 0 ]

# These are standard image-build state locations, not caller-controlled paths.
for directory in /etc/netplan /run/netplan; do
  if [ -d "$directory" ]; then
    while IFS= read -r -d '' file; do
      case "$file" in
        /etc/netplan/*.yaml|/run/netplan/*.yaml) rm -- "$file" ;;
        *) echo 'Refusing unexpected network cleanup path' >&2; exit 1 ;;
      esac
    done < <(find "$directory" -maxdepth 1 -type f -name '*.yaml' -print0)
  fi
done
# Remove installer-generated network/cloud identity and secrets, not cloud-init itself.
for file in /etc/cloud/cloud.cfg.d/99-installer.cfg /etc/cloud/cloud.cfg.d/subiquity-disable-cloudinit-networking.cfg /etc/cloud/cloud.cfg.d/90-installer-network.cfg; do
  [ ! -L "$file" ] || { echo 'Refusing symlink in installer cleanup' >&2; exit 1; }
  rm -f -- "$file"
done
for directory in /var/lib/cloud /run/cloud-init /var/log/installer; do
  [ ! -L "$directory" ] || { echo 'Refusing symlink in build-state cleanup' >&2; exit 1; }
  rm -rf -- "$directory"
done
rm -f -- /var/log/cloud-init.log /var/log/cloud-init-output.log /autoinstall.yaml /etc/cloud/cloud-init.disabled
cloud-init clean --logs --seed --machine-id
# Make next boot accept only its new cloud-init identity. Do not retain the temporary
# builder's password hash/sudo grant or SSH keys from the seed/installer.
passwd -l builder >/dev/null
usermod -p '!' builder
for directory in /home/builder/.ssh /root/.ssh; do
  [ ! -L "$directory" ] || { echo 'Refusing symlink in temporary SSH cleanup' >&2; exit 1; }
  rm -rf -- "$directory"
done
rm -f -- /etc/ssh/ssh_host_*
for file in /etc/sudoers.d/*; do
  [ -f "$file" ] || continue
  if grep -qw builder "$file"; then
    [ ! -L "$file" ] || { echo 'Refusing symlink in sudo cleanup' >&2; exit 1; }
    rm -- "$file"
  fi
done
# Fail closed rather than publishing an image carrying a static build network/seed.
for directory in /etc/netplan /run/netplan; do
  if [ -d "$directory" ]; then
    remaining="$(find "$directory" -maxdepth 1 -type f -name '*.yaml' -print -quit)"
    [ -z "$remaining" ] || { echo 'Static build network cleanup verification failed' >&2; exit 1; }
  fi
done
[ ! -d /var/lib/cloud/seed ]
[ "$(getent shadow builder | cut -d: -f2)" = '!' ]
sync
