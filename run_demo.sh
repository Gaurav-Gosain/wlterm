#!/bin/bash
# Record several real applications tiled and interactive.
#
# Headless: wlterm composites to its own canvas and the snapshot loop writes
# the exact bytes it would transmit. Stdin drives the compositor, so the
# recording is a real session, not a scripted redraw.
cd "$(dirname "$0")"
OUT=${OUT:-/tmp/wldemo}
rm -rf "$OUT"; mkdir -p "$OUT"
export LIBGL_ALWAYS_SOFTWARE=1

# kitty CSI-u press+release: $1 codepoint, $2 mods (1 + bits)
k(){ printf "\033[%s;%s:1u\033[%s;%s:3u" "$1" "$2" "$1" "$2"; }
pfx(){ k 98 5; }
type_str(){ local s="$1"; local i c; for ((i=0;i<${#s};i++)); do c="${s:$i:1}"; k "$(printf '%d' "'$c")" 1; sleep 0.03; done; }

{
  sleep 6                       # let the first three clients map
  pfx; k 9 1;   sleep 1         # focus next
  type_str "echo hello from tile two"; k 13 1; sleep 2
  pfx; k 9 1;   sleep 1
  type_str "uname -sr"; k 13 1; sleep 2
  pfx; k 32 1;  sleep 3         # master-stack
  pfx; k 122 1; sleep 2.5       # zoom
  pfx; k 122 1; sleep 1.5       # unzoom
  pfx; k 32 1;  sleep 3         # back to bsp
  pfx; k 9 1;   sleep 2
  sleep 3
} | timeout 40 ./wlterm -pixels 1600x900 -cell 9x18 -mode shm -fps 60 \
     -log "$OUT/log" -snapshots "$OUT/snaps" -snapshot-every 100ms \
     -spawn "foot -T shell sh -c 'exec sh'" \
     -exec "foot -T foot-one sh -c 'echo a real terminal; exec sh'" \
     -exec "foot -T foot-two sh -c 'echo another one; exec sh'" \
     -exec "gtk-demo" \
     >/dev/null 2>&1

echo "frames: $(ls "$OUT/snaps" | wc -l)"
ffmpeg -y -framerate 10 -i "$OUT/snaps/%06d.png" -c:v libx264 -pix_fmt yuv420p \
    -vf "scale=trunc(iw/2)*2:trunc(ih/2)*2" "$OUT/demo.mp4" 2>/dev/null
echo "video: $OUT/demo.mp4"
