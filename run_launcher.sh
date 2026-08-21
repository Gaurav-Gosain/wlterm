#!/bin/bash
# What the application launcher costs, and whether it launches real things.
#
# Two runs:
#   cost    delta transport, idle clients, one operation per second, so the
#           per-operation "overlay ..." log lines are unambiguous
#   launch  a Terminal=true entry and a GTK application, checked against the
#           framebuffer rather than against their exit codes
#
# The interesting output is the "overlay" lines: pixels and bytes per
# operation, plus how many TILES the operation forced to recomposite. That
# last column should be zero everywhere.
cd "$(dirname "$0")"
OUT=${OUT:-/tmp/wllauncher}
rm -rf "$OUT"; mkdir -p "$OUT"
go build -o ./wlterm . || exit 1

# kitty-keyboard press+release for one unicode codepoint.
k() { printf "\033[%s;%su" "$1" "${2:-1}"; printf "\033[%s;%s:3u" "$1" "${2:-1}"; }
type_str() {
  local s="$1" i c
  for ((i=0;i<${#s};i++)); do printf -v c "%d" "'${s:$i:1}"; k "$c"; sleep "${2:-0.9}"; done
}

cost_keys() {
  sleep 4
  k 98 5; sleep 0.3; k 100        # ctrl+b d: summon
  sleep 2
  type_str "fire" 1.2             # four keystrokes
  sleep 1
  printf "\033[1;1:1B"; sleep 1.2 # arrow down
  printf "\033[1;1:1B"; sleep 1.2
  printf "\033[1;1:1A"; sleep 1.2 # arrow up
  printf "\033[27u"; sleep 2      # escape: dismiss
  k 92 5; k 92 5; sleep 0.5
}

launch_keys() {
  sleep 4
  k 98 5; sleep 0.3; k 100
  sleep 1.5; type_str "btop" 0.15; sleep 1.5
  printf "\033[13u"; sleep 9      # Terminal=true -> wrapped into foot
  k 98 5; sleep 0.3; k 100
  sleep 1.2; type_str "thunar file m" 0.12; sleep 1.5
  printf "\033[13u"; sleep 14     # a GTK application through the private bus
  k 92 5; k 92 5; sleep 1
}

echo "== cost (delta, two idle clients) =="
cost_keys | ./wlterm -pixels 1280x720 -cell 10x20 -mode delta -layers per-window \
  -log "$OUT/cost.log" -exec foot -exec foot >/dev/null 2>&1
grep -E "overlay |launcher (open|closed|scan)" "$OUT/cost.log"

echo
echo "== launch (verified against the framebuffer) =="
launch_keys | ./wlterm -pixels 1400x800 -cell 10x20 -mode shm -layers per-window \
  -log "$OUT/launch.log" -snapshots "$OUT/snaps" -snapshot-every 500ms >/dev/null 2>&1
grep -E "launcher: launching|VERIFIED|UNVERIFIED|WARNING child" "$OUT/launch.log"

echo
echo "== leaks =="
echo "shm slots left:      $(ls /dev/shm 2>/dev/null | grep -c wlterm)"
echo "runtime dirs left:   $(ls "${XDG_RUNTIME_DIR:-/tmp}" 2>/dev/null | grep -c wlterm-rt)"
echo "snapshots:           $(ls "$OUT/snaps" 2>/dev/null | wc -l) in $OUT/snaps"
