#!/bin/bash
# How the compositor scales with the number of surfaces.
#
# Two workloads:
#   one   a single animating tile, the rest simply on screen
#   all   every tile animating
# crossed with the two transmission granularities (single canvas vs one
# image per window) and the two shm transports.
cd "$(dirname "$0")"
BENCH=$PWD/wlbench/wlbench
DUR=${DUR:-8}
OUT=${OUT:-/tmp/wlscale}
rm -rf "$OUT"; mkdir -p "$OUT"

printf "%-6s %-11s %-6s %-3s %8s %10s %14s %12s %9s\n" \
  load layers mode N fps pty_B/s shm_B/s composite dmg_px

for load in one all; do
for layers in single per-window; do
for mode in shm delta; do
for n in 1 2 4; do
  args=()
  for ((i=0;i<n;i++)); do
    w=rect
    if [ "$load" = one ] && [ $i -gt 0 ]; then w=idle; fi
    args+=(-exec "$BENCH -work $w -dur $((DUR+4))")
  done
  log=$OUT/$load-$layers-$mode-$n.log
  timeout $((DUR+8)) ./wlterm -pixels 1440x800 -cell 9x18 -mode $mode -layers $layers \
      -fps 1000 -log "$log" "${args[@]}" >/dev/null 2>&1
  # Ignore the first two seconds of samples: startup and first configure.
  line=$(grep "stats:" "$log" | tail -n +3 | python3 -c '
import sys,re
f=p=s=c=d=0; n=0
for l in sys.stdin:
    m=dict(re.findall(r"(\w+)=([\d.]+)", l))
    if "fps" not in m: continue
    f+=float(m["fps"]); p+=float(m["pty_bytes_per_s"]); s+=float(m["shm_bytes_per_s"])
    d+=float(m["dmg_px_per_frame"]); n+=1
    cm=re.search(r"composite=([\d.]+)(m?)s", l)
    if cm: c+=float(cm.group(1))*(1 if cm.group(2)=="m" else 1000)
print("%.1f %.0f %.0f %.3f %.0f"%(f/n,p/n,s/n,c/n,d/n) if n else "0 0 0 0 0")')
  set -- $line
  printf "%-6s %-11s %-6s %-3d %8s %10s %14s %10sms %9s\n" "$load" "$layers" "$mode" "$n" "$1" "$2" "$3" "$4" "$5"
done; done; done; done
