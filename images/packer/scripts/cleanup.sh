#!/bin/sh
# Clean state: a clone must boot as a new machine and run cloud-init from scratch.
apt-get -y -qq autoremove --purge
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*
cloud-init clean --logs --seed
rm -f /etc/ssh/ssh_host_*
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id
find /var/log -type f -exec truncate -s 0 {} +
rm -f /home/*/.bash_history /root/.bash_history
rm -rf /home/*/.ssh /root/.ssh
fstrim -av || true
sync
