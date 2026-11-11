#!/bin/bash
# fio-class.sh <storage> <seconds>: benchmark one Proxmox storage on this node (plan S19). Prints
# {"p99_ms": <sync-write p99>, "iops": <randread>}. The scratch volume never outlives the script.
set -euo pipefail
S=$1 RUNTIME=$2 VMID=999999
VOL=$(pvesm alloc "$S" "$VMID" "" 4G --format raw | sed -n "s/^successfully created '\(.*\)'$/\1/p")
trap 'pvesm free "$VOL" >/dev/null 2>&1 || true' EXIT
FILE=$(pvesm path "$VOL")
fio_json() { fio --name="$1" --filename="$FILE" --ioengine=libaio --bs=4k --direct=1 --runtime="$RUNTIME" --time_based \
  --output-format=json "${@:2}" 2>/dev/null; }
W=$(fio_json syncw --rw=randwrite --iodepth=1 --fsync=1)
R=$(fio_json randr --rw=randread --iodepth=32)
python3 - "$W" "$R" <<'PY'
import json, sys
w, r = (json.loads(a)["jobs"][0] for a in sys.argv[1:3])
# With --fsync=1 the write latency alone misses the flush: count both.
pct = lambda d: d.get("percentile", {}).get("99.000000", 0) / 1e6
print(json.dumps({"p99_ms": round(pct(w["write"]["clat_ns"]) + pct(w["sync"]["lat_ns"]), 3), "iops": round(r["read"]["iops"])}))
PY
