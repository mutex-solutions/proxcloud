#!/bin/sh
# The template contract (images/README.md): guest agent and current packages.
export DEBIAN_FRONTEND=noninteractive
cloud-init status --wait || true
apt-get update -qq
apt-get -y -qq -o Dpkg::Options::=--force-confold dist-upgrade
apt-get -y -qq install qemu-guest-agent cloud-guest-utils
systemctl enable qemu-guest-agent
# `cipassword` must be enough to log in: vendor images turn password SSH off in a drop-in.
rm -f /etc/ssh/sshd_config.d/60-cloudimg-settings.conf
