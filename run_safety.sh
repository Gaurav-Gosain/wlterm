#!/bin/bash
# The hardening that came out of crashing the desktop, re-checked against
# the multi-client version: a hostile client must be refused and killed
# without taking the compositor or the other clients down with it.
cd "$(dirname "$0")"
OUT=${OUT:-/tmp/wlsafety}
rm -rf "$OUT"; mkdir -p "$OUT"
BENCH=$PWD/wlbench/wlbench
timeout 20 ./wlterm -pixels 1280x720 -cell 9x18 -mode shm -fps 60 -log "$OUT/log" \
  -exec "$BENCH -work rect -dur 14" \
  -exec "sleep 3; $BENCH -work evil" \
  -exec "sleep 5; $BENCH -work evil" \
  -exec "sleep 7; $BENCH -work rect -dur 6" \
  -snapshots "$OUT/snaps" -snapshot-every 1s >/dev/null 2>&1
echo "== refusals =="; grep -E "refusing|fault|protocol" "$OUT/log"
echo "== survivors =="; grep -E "window added|window removed|client gone" "$OUT/log" | tail -8
echo "== still rendering at the end =="; grep "stats:" "$OUT/log" | tail -2
echo "== leftover shm =="; ls /dev/shm | grep wlterm || echo "(none)"
