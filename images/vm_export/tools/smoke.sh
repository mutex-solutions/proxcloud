#!/bin/sh
# Smoke test of the VM export tool image, run by vm-export-tools.yml before any push (and locally):
#   images/vm_export/tools/smoke.sh IMAGE
# Same confinement as images/vm_export/capsule.py (no network, read-only root, no capabilities, env -i),
# but as an arbitrary uid that has no passwd entry, with tmpfs standing in for the scratch bind.
set -eu
image="${1:?usage: smoke.sh IMAGE}"
uid=1002
exec docker run --rm --pull=never --network=none --read-only --user="$uid:$uid" --cap-drop=ALL \
    --security-opt=no-new-privileges --pids-limit=256 --memory=4g --shm-size=128m \
    --tmpfs "/tmp:rw,exec,nosuid,nodev,size=1g,mode=0700,uid=$uid,gid=$uid" \
    --tmpfs "/scratch:rw,exec,nosuid,nodev,size=2g,mode=0700,uid=$uid,gid=$uid" \
    --entrypoint /usr/bin/env "$image" -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/scratch/home \
    TMPDIR=/scratch/tmp LC_ALL=C LANG=C LIBGUESTFS_BACKEND=direct LIBGUESTFS_BACKEND_SETTINGS=force_tcg \
    LIBGUESTFS_CACHEDIR=/scratch/cache LIBGUESTFS_TMPDIR=/scratch/tmp \
    /usr/bin/python3 -I -c '
import os, pwd, stat, subprocess, sys
uid = int(sys.argv[1])
assert os.getuid() == uid and os.getgid() == uid
try:
    pwd.getpwuid(uid)
    raise SystemExit("uid unexpectedly has a passwd entry")
except KeyError:
    pass
# No file of the image keeps a privilege of its own: no setuid, no setgid, no file capability.
root = os.stat("/").st_dev
for top, dirs, files in os.walk("/"):
    dirs[:] = [name for name in dirs if os.lstat(os.path.join(top, name)).st_dev == root]
    for path in (os.path.join(top, name) for name in files):
        mode = os.lstat(path).st_mode
        if stat.S_ISREG(mode):
            assert not mode & 0o6000 and "security.capability" not in os.listxattr(path), path
for name in ("home", "tmp", "cache"):
    os.mkdir("/scratch/" + name, 0o700)
import guestfs
subprocess.run(["/usr/bin/qemu-img", "--version"], check=True, stdout=subprocess.DEVNULL)
for name in ("empty", "work"):
    subprocess.run(["/usr/bin/qemu-img", "create", "-q", "-f", "qcow2", "/scratch/%s.qcow2" % name, "64M"], check=True)
g = guestfs.GuestFS(python_return_dict=True)
g.set_backend("direct")
g.set_backend_setting("force_tcg", "true")
g.set_network(False)
g.add_drive_opts("/scratch/empty.qcow2", format="qcow2", readonly=True)
g.add_drive_opts("/scratch/work.qcow2", format="qcow2", readonly=False)
g.launch()
assert g.list_devices() == ["/dev/sda", "/dev/sdb"]
# The appliance tools the sanitizer relies on (images/vm_sanitize.py): mkfs, the debug shell tune2fs, e2fsck.
# The debug shell sees the kernel device names, which are not always in drive order: address by label.
g.mkfs("ext4", "/dev/sdb", label="smoke")
g.debug("sh", ["/usr/sbin/tune2fs -O ^has_journal LABEL=smoke"])
assert "has_journal" not in g.tune2fs_l("/dev/sdb")["Filesystem features"].split()
g.e2fsck("/dev/sdb", correct=True)
g.lvs()
g.shutdown()
g.close()
print("vm-export-tools smoke ok: uid %d, qemu-img, python3 guestfs, TCG appliance" % uid)
' "$uid"
