#!/usr/bin/env python3
"""Re-parse the scaling logs into a table. Bytes per frame matters as much as
bytes per second: tiles shrink as N grows, so frame rates are not comparable
across N on their own."""
import re, sys, os, glob

def dur(v, unit):
    return float(v) * {"n": 1e-3, "µ": 1.0, "m": 1000.0, "": 1e6}[unit]

rows = []
for path in sorted(glob.glob(sys.argv[1] + "/*.log")):
    parts = os.path.basename(path)[:-4].split("-")
    load, n = parts[0], parts[-1]
    mode = parts[-2]
    layers = "-".join(parts[1:-2])
    f = p = s = c = 0.0
    k = 0
    for line in open(path, errors="ignore"):
        if "stats:" not in line:
            continue
        m = dict(re.findall(r"(\w+)=([\d.]+)", line))
        if "fps" not in m:
            continue
        k += 1
        if k <= 2:
            continue  # skip startup and first configure
        f += float(m["fps"])
        p += float(m["pty_bytes_per_s"])
        s += float(m["shm_bytes_per_s"])
        cm = re.search(r"composite=([\d.]+)(n|µ|m|)s", line)
        if cm:
            c += dur(cm.group(1), cm.group(2))
    k -= 2
    if k <= 0:
        continue
    fps, pty, shm, comp = f / k, p / k, s / k, c / k
    rows.append((load, layers, mode, int(n), fps, pty, shm, comp))

hdr = ("load", "layers", "mode", "N", "fps", "pty B/s", "shm MB/s", "shm KB/frame", "composite")
print("%-5s %-10s %-5s %-2s %7s %9s %10s %13s %11s" % hdr)
order = {"one": 0, "all": 1}
for r in sorted(rows, key=lambda r: (order[r[0]], r[1], r[2], r[3])):
    load, layers, mode, n, fps, pty, shm, comp = r
    print("%-5s %-10s %-5s %-2d %7.1f %9.0f %10.1f %13.1f %9.0fus"
          % (load, layers, mode, n, fps, pty, shm / 1e6, shm / max(fps, 1e-9) / 1024, comp))
