#!/bin/bash
# Single-app wlterm inside a real tuios pane, and what the GPU is worth there.
#
# tools/tuios_pane.py runs tuios on a pty and answers its capability probes the
# way kitty does, so the pane wlterm lands in is a real tuios pane: its pixel
# geometry comes from tuios's own CSI 14t answer and its resizes come from
# tuios's own TIOCSWINSZ. tuios gets throwaway XDG dirs so it neither restores
# nor overwrites the real session.
#
# Four runs:
#   keys     every keystroke reaches the app; a single ctrl+\ does too;
#            two of them inside 700ms quit
#   resize   shrink the host until tuios has to shrink the pane, and watch the
#            toplevel get reconfigured to match
#   llvmpipe kitty rendering on the CPU, presenting through wl_shm
#   gpu      the same kitty on the i915, presenting through a LINEAR dmabuf
#
# The last two are the answer to "vulkan instead of llvmpipe": same workload,
# same pane, same seconds, and the difference is in the CPU column.
set -u
cd "$(dirname "$0")"
OUT=${OUT:-/tmp/wlpane}
rm -rf "$OUT"; mkdir -p "$OUT"
go build -o ./wlterm . || exit 1
W=$PWD/wlterm
H="python3 $PWD/tools/tuios_pane.py"
command -v tuios >/dev/null || { echo "tuios not on PATH"; exit 1; }

echo "== keys: nothing is intercepted except the escape hatch =="
D=$OUT/keys; mkdir -p "$D"
$H --priv "$D/priv" --cols 160 --rows 44 \
   --cmd "$W -mode shm -log $D/w.log -snapshots $D/snaps -snapshot-every 1s -- foot -e sh" \
   --post '20@echo KEYS-REACH-THE-APP\r' \
   --post '25@\x1c' \
   --post '28@ SINGLE-CTRL-BACKSLASH-DID-NOT-QUIT\r' \
   --post '34@\x1c' --post '34.05@\x1c' \
   --seconds 45 >/dev/null 2>&1
grep -E "single-app:|quit requested" "$D/w.log"
echo "last frame: $D/snaps/$(ls "$D/snaps" | tail -1)  (should show both lines echoed)"

echo
echo "== resize: the toplevel follows the pane =="
D=$OUT/resize; mkdir -p "$D"
$H --priv "$D/priv" --cols 160 --rows 44 --resize-at 16 --resize-to 60x16 \
   --cmd "$W -mode shm -log $D/w.log -- foot -e sh -c 'sleep 30'" \
   --seconds 40 >/dev/null 2>&1
grep -E "terminal:|SIGWINCH" "$D/w.log"

# One kitty, one workload, one pane. Only the render path differs.
work='e=$(($(date +%s)+30)); i=0; while [ $(date +%s) -lt $e ]; do echo "frame $i abcdefghijklmnopqrstuvwxyz0123456789"; i=$((i+1)); done'
compare() { # $1 label  $2 wlterm flags  $3 client env
  D=$OUT/$1; mkdir -p "$D"
  $H --priv "$D/priv" --cols 200 --rows 52 --cpu-out "$D/cpu.txt" \
     --cmd "$W $2 -mode shm -log $D/w.log -- env $3 kitty -o font_size=12 -e sh -c '$work'" \
     --seconds 50 >/dev/null 2>&1
  echo "== $1 =="
  grep "stats:" "$D/w.log" | sed 's/^[0-9:.]* //' | tail -n +3 | python3 tools/panestats.py
  echo "  cpu seconds over the pane process tree:"; sed 's/^/    /' "$D/cpu.txt"
}
echo
compare llvmpipe "-no-dmabuf" "LIBGL_ALWAYS_SOFTWARE=1"
echo
compare gpu "" "__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/50_mesa.json"
