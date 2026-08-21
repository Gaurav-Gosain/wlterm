#!/bin/bash
# Prove the layered transport reconstructs exactly what was composited.
#
# wlterm emits one image per window, each placed at its own cell, plus the
# chrome underneath. kittydec replays that stream the way a terminal would --
# image store, placements, z-order -- and the result is compared pixel for
# pixel against the compositor's own canvas at the same moment.
cd "$(dirname "$0")"
OUT=${OUT:-/tmp/wlverify}
rm -rf "$OUT"; mkdir -p "$OUT"
BENCH=$PWD/wlbench/wlbench

for layers in per-window single; do
  rm -rf "$OUT/$layers"; mkdir -p "$OUT/$layers/snaps"
  timeout 16 ./wlterm -pixels 1440x800 -cell 9x18 -mode b64 -layers $layers -fps 30 \
    -log "$OUT/$layers/log" -snapshots "$OUT/$layers/snaps" -snapshot-every 400ms \
    -exec "$BENCH -work idle -dur 11" \
    -exec "sleep 1.5; $BENCH -work idle -dur 9" \
    -exec "sleep 3; $BENCH -work idle -dur 8" \
    -exec "sleep 4.5; $BENCH -work idle -dur 6" \
    > "$OUT/$layers/stream.bin" 2>/dev/null
  ./kittydec/kittydec -in "$OUT/$layers/stream.bin" -out "$OUT/$layers/decoded.png" -cell 9x18 -size 1440x800
  last=$(ls "$OUT/$layers/snaps" | tail -1)
  python3 - "$OUT/$layers/decoded.png" "$OUT/$layers/snaps/$last" "$layers" <<'PY'
import sys
from PIL import Image, ImageChops
a = Image.open(sys.argv[1]).convert("RGB")
b = Image.open(sys.argv[2]).convert("RGB")
if a.size != b.size:
    print("%-10s SIZE MISMATCH decoded=%s canvas=%s" % (sys.argv[3], a.size, b.size)); sys.exit(1)
diff = ImageChops.difference(a, b)
bbox = diff.getbbox()
n = sum(1 for p in diff.getdata() if p != (0, 0, 0))
print("%-10s decoded %dx%d, differing pixels: %d of %d  %s"
      % (sys.argv[3], a.size[0], a.size[1], n, a.size[0]*a.size[1],
         "IDENTICAL" if n == 0 else "diff bbox %s" % (bbox,)))
PY
done
