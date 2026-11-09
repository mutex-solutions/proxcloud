#!/bin/sh
# E2B sandbox conventions every ProxCloud template follows: user "user" (uid 1000, bash) with
# passwordless sudo, home /home/user, a writable /code, and /usr/local open for new installs.
# sandbox-init runs the user's commands as "user" (it creates the account if an image lacks it).
set -eu
if ! id -u user >/dev/null 2>&1; then
  if old=$(getent passwd 1000 | cut -d: -f1) && [ -n "$old" ]; then
    # The image already has a uid-1000 account (ubuntu, node, pwuser…): rename it.
    usermod -l user -d /home/user -m "$old"
    oldg=$(getent group 1000 | cut -d: -f1 || true)
    [ -n "$oldg" ] && [ "$oldg" != user ] && groupmod -n user "$oldg"
  else
    useradd -m -u 1000 -s /bin/bash user
  fi
fi
usermod -s /bin/bash user
mkdir -p /home/user /code
chown -R user:user /home/user /code
echo 'user ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/user
chmod 0440 /etc/sudoers.d/user
# Directories only (not -R): new global installs work without sudo, and the image does not get a
# second copy of everything under /usr/local (a recursive chmod rewrites every file in a layer).
for d in /usr/local /usr/local/bin /usr/local/lib /usr/local/share /usr/local/lib/node_modules \
         /usr/local/lib/python3*/site-packages /usr/local/lib/python3*/dist-packages; do
  if [ -d "$d" ]; then chmod a+rwx "$d"; fi
done
