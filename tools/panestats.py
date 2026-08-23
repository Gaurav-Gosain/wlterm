#!/usr/bin/env python3
"""Average wlterm's per-second stats lines, read on stdin.

Splits the dmabuf ingest cost into the GPU fence wait and the memcpy, because
those are two different things wearing one number: the fence is the client's
own render finishing, and only the memcpy is compositor CPU.
"""
import sys, re

UNIT = {'ns': 1e-3, 'µs': 1.0, 'ms': 1e3, 's': 1e6, 'm': 6e7, 'h': 3.6e9}


def us(s):
    # Go durations: "0s", "873.2µs", "1.5ms", "1m2s". Sum every component so a
    # zero and a compound value both parse.
    parts = re.findall(r'([\d.]+)(ns|µs|ms|h|m|s)', s)
    if not parts:
        raise ValueError('not a duration: ' + s)
    return sum(float(v) * UNIT[u] for v, u in parts)


rows = [dict(p.split('=', 1) for p in line.split() if '=' in p) for line in sys.stdin]
rows = [r for r in rows if 'fps' in r]
if not rows:
    sys.exit('no stats lines')


def avg(key, conv=float):
    vals = [conv(r[key]) for r in rows if key in r]
    return sum(vals) / len(vals) if vals else 0.0


stage = ['composite', 'encode', 'write', 'pace', 'idle']
tot = sum(avg(k, us) for k in stage)
print(f"  samples={len(rows)} fps={avg('fps'):.1f} "
      f"commit_to_out={avg('commit_to_out', us) / 1000:.1f}ms")
print("  period " + f"{tot:.0f}us = " + " + ".join(
    f"{k} {avg(k, us):.0f}us" for k in stage if k in rows[0]))
if any('shm_read' in r for r in rows):
    print(f"  shm_read={avg('shm_read', us):.0f}us")
if any('dmabuf_read' in r for r in rows):
    read, sync = avg('dmabuf_read', us), avg('dmabuf_sync', us)
    print(f"  dmabuf_read={read:.0f}us "
          f"(GPU fence {sync:.0f}us, memcpy {read - sync:.0f}us)")
chrome = [r for r in rows if 'chrome' in r]
if chrome:
    n = sum(int(r['chrome_repaints']) for r in chrome)
    print(f"  chrome: {n} repaint(s) over {len(rows)}s, "
          f"{sum(us(r['chrome']) for r in chrome) / len(chrome):.0f}us each")
