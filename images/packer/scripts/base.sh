#!/bin/sh
# The template contract (images/README.md): guest agent and current packages.
export DEBIAN_FRONTEND=noninteractive
cloud-init status --wait || true
# The build disk is small (images/build.py DEFAULT_BUILD_DISK_GIB): record the room before and after the
# upgrade in the build log. This runs under `sh -eu`: an APT that runs out of space stops the build here.
df -Pk /
apt-get update -qq
apt-get -y -qq -o Dpkg::Options::=--force-confold dist-upgrade
apt-get -y -qq install qemu-guest-agent cloud-guest-utils
df -Pk /
systemctl enable qemu-guest-agent
# `cipassword` must be enough to log in: vendor images turn password SSH off in a drop-in.
rm -f /etc/ssh/sshd_config.d/60-cloudimg-settings.conf
