#!/usr/bin/env python3
"""Average wlterm's per-second stats lines, read on stdin.

Splits the dmabuf ingest cost into the GPU fence wait and the memcpy, because
those are two different things wearing one number: the fence is the client's
own render finishing, and only the memcpy is compositor CPU.
"""
import sys, re

UNIT = {'ms': 1e3, 'µs': 1.0, 's': 1e6}


def us(s):
    m = re.match(r'([\d.]+)(ms|µs|s)', s)
    return float(m.group(1)) * UNIT[m.group(2)]


rows = [dict(p.split('=', 1) for p in line.split() if '=' in p) for line in sys.stdin]
rows = [r for r in rows if 'fps' in r]
if not rows:
    sys.exit('no stats lines')


def avg(key, conv=float):
    vals = [conv(r[key]) for r in rows if key in r]
    return sum(vals) / len(vals) if vals else 0.0


print(f"  samples={len(rows)} fps={avg('fps'):.1f} "
      f"composite={avg('composite', us):.0f}us encode={avg('encode', us):.0f}us "
      f"commit_to_out={avg('commit_to_out', us) / 1000:.1f}ms")
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
