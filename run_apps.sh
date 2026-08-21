#!/bin/bash
# Which real applications actually run under wlterm, one at a time.
cd "$(dirname "$0")"
OUT=${OUT:-/tmp/wlapps}
rm -rf "$OUT"; mkdir -p "$OUT"
export LIBGL_ALWAYS_SOFTWARE=1
try(){
  local name="$1"; shift
  rm -rf "$OUT/$name"; mkdir -p "$OUT/$name"
  timeout 20 ./wlterm -pixels 1280x760 -cell 9x18 -mode shm -fps 30 \
      -log "$OUT/$name/log" -snapshots "$OUT/$name/snaps" -snapshot-every 2s \
      -exec "$*" >/dev/null 2>&1
  local win=$(grep -c "window added" "$OUT/$name/log")
  local title=$(grep -oP 'toplevel title: \K.*' "$OUT/$name/log" | tail -1)
  local err=$(grep -cE "refusing|fault copying|protocol" "$OUT/$name/log")
  printf "%-14s windows=%-2s errors=%-2s frames=%s\n" "$name" "$win" "$err" "$(ls "$OUT/$name/snaps" 2>/dev/null | wc -l)"
}
try foot     "foot -T foot sh -c 'echo hello; exec sh'"
try thunar   "thunar /usr/share"
try evince   "evince"
try kitty    "kitty -o confirm_os_window_close=0 -e sh -c 'echo kitty; exec sh'"
try alacritty "alacritty -e sh -c 'echo alacritty; exec sh'"
try wev      "wev"
try mpv      "mpv --no-config --demuxer-lavf-o=lavfi --force-window=yes av://lavfi:testsrc=size=640x480:rate=15 --length=12"
