#!/usr/bin/env python3
"""Measure the three transports against a real terminal, bare and under tuios.

The host is a real kitty, run as a client of an outer headless wlterm, so its
pixels can be captured and its CPU can be attributed. Every run uses the same
guest (wlbench) and the same seconds.

Reported per run:
  size           the canvas wlterm was given, which is the pane inside tuios
                 and the whole window bare. It differs between the two
                 contexts, so rows are only comparable within one.
  out fps        frames wlterm produced
  seen fps       distinct pictures the host terminal actually presented,
                 counted by the outer compositor. wlterm never waits for the
                 host, so out fps on its own says nothing about what reached
                 the screen; this is the column that does.
  pty B/frame    bytes of escape down the pty
  shm KB/frame   pixels that rode /dev/shm (0 for b64, which inlines them)
  cpu            seconds, per process name, over the whole tree

The outer wlterm is harness overhead common to every row; it is reported so
that a row which moved only that number can be recognised as noise.
"""
import argparse, os, re, signal, subprocess, sys, time

HZ = os.sysconf('SC_CLK_TCK')

def proc_tree(root):
    kids, info = {}, {}
    for d in os.listdir('/proc'):
        if not d.isdigit():
            continue
        try:
            with open('/proc/%s/stat' % d) as f:
                st = f.read()
        except OSError:
            continue
        i = st.rindex(')')
        name = st[st.index('(') + 1:i]
        f = st[i + 2:].split()
        kids.setdefault(int(f[1]), []).append(int(d))
        info[int(d)] = (name, (int(f[11]) + int(f[12])) / HZ)
    out, stack = {}, [root]
    while stack:
        pid = stack.pop()
        if pid in info:
            out[pid] = info[pid]
        stack.extend(kids.get(pid, []))
    return out

STAT = re.compile(r'fps=([\d.]+).*?pty_bytes_per_s=(\d+) shm_bytes_per_s=(\d+)')
SIZE = re.compile(r'size=(\d+x\d+)')
FPS = re.compile(r'fps=([\d.]+)')


def mean_fps(path, skip=3):
    """Mean of a wlterm log's own per-second fps, dropping the warm-up."""
    v = [float(m.group(1)) for m in
         (FPS.search(l) for l in open(path, errors='replace')) if m]
    v = v[skip:]
    return sum(v) / len(v) if v else 0.0


def canvas(path):
    for line in open(path, errors='replace'):
        m = SIZE.search(line)
        if m:
            return m.group(1)
    return '?'


def parse_stats(path, skip=3):
    """Mean of the per-second stats lines, dropping the first few while the
    guest is still starting up."""
    rows = []
    try:
        for line in open(path, errors='replace'):
            m = STAT.search(line)
            if m:
                rows.append((float(m.group(1)), int(m.group(2)), int(m.group(3))))
    except OSError:
        return None
    rows = rows[skip:]
    if not rows:
        return None
    n = len(rows)
    fps = sum(r[0] for r in rows) / n
    pty = sum(r[1] for r in rows) / n
    shm = sum(r[2] for r in rows) / n
    return fps, pty, shm

def run(a, mode, work, ctx, outdir):
    d = os.path.join(outdir, '%s-%s-%s' % (ctx, mode, work))
    os.makedirs(d, exist_ok=True)
    inner_log = os.path.join(d, 'inner.log')
    W = os.path.abspath(a.wlterm)
    guest = '%s/wlbench/wlbench -w %d -h %d -work %s -dur %d' % (
        os.path.dirname(W), a.gw, a.gh, work, a.seconds)
    inner = '%s -mode %s -fps %d -log %s -- %s' % (W, mode, a.fps, inner_log, guest)
    if ctx == 'bare':
        term = ['kitty', '-o', 'font_size=11', '-o', 'confirm_os_window_close=0',
                '-e', 'sh', '-c', inner + '; sleep 2']
    else:
        term = ['kitty', '-o', 'font_size=11', '-o', 'confirm_os_window_close=0',
                '-e', 'python3', os.path.join(os.path.dirname(__file__), 'tuios_host.py'),
                '--tuios', a.tuios, '--priv', os.path.join(d, 'priv'),
                '--log', os.path.join(d, 'h.log'), '--seconds', str(a.seconds + 12),
                '--esc-out', os.path.join(d, 'esc.txt'), '--cmd', inner]
    cmd = [W, '-pixels', '%dx%d' % (a.cw, a.ch), '-cell', '10x20', '-mode', 'shm',
           '-fps', '0', '-log', os.path.join(d, 'outer.log'), '--',
           'env', 'LIBGL_ALWAYS_SOFTWARE=1'] + term
    p = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    peak = {}
    end = time.time() + a.seconds + (24 if ctx == 'tuios' else 8)
    while time.time() < end and p.poll() is None:
        for pid, (name, cpu) in proc_tree(p.pid).items():
            k = (pid, name)
            if cpu > peak.get(k, -1):
                peak[k] = cpu
        time.sleep(0.25)
    if p.poll() is None:
        p.send_signal(signal.SIGTERM)
        time.sleep(1)
        if p.poll() is None:
            p.kill()
    p.wait()
    cpu = {}
    for (_, name), c in peak.items():
        cpu[name] = cpu.get(name, 0.0) + c
    seen = 0.0
    try:
        seen = mean_fps(os.path.join(d, 'outer.log'))
    except OSError:
        pass
    try:
        size = canvas(inner_log)
    except OSError:
        size = '?'
    return parse_stats(inner_log), cpu, seen, size

ap = argparse.ArgumentParser()
ap.add_argument('--wlterm', default='./wlterm')
ap.add_argument('--tuios', default='tuios')
ap.add_argument('--out', default='/tmp/afbench')
ap.add_argument('--seconds', type=int, default=15)
ap.add_argument('--cw', type=int, default=1600)
ap.add_argument('--ch', type=int, default=900)
ap.add_argument('--gw', type=int, default=800)
ap.add_argument('--gh', type=int, default=400)
ap.add_argument('--ctx', default='bare,tuios')
ap.add_argument('--modes', default='shm,delta,b64')
ap.add_argument('--works', default='rect,full')
ap.add_argument('--fps', type=int, default=60,
                help='frame cap for the wlterm under test. A cap is the honest '
                     'setting for a cost comparison: uncapped, each transport '
                     'runs at its own rate and the CPU columns are measuring '
                     'different amounts of work. 0 uncaps and measures the '
                     'ceiling instead.')
a = ap.parse_args()
os.makedirs(a.out, exist_ok=True)

print('%-6s %-6s %-5s %9s %8s %9s %10s %11s   %s' %
      ('ctx', 'mode', 'work', 'size', 'out fps', 'seen fps', 'pty B/fr',
       'shm KB/fr', 'cpu seconds'))
for ctx in a.ctx.split(','):
    for work in a.works.split(','):
        for mode in a.modes.split(','):
            st, cpu, seen, size = run(a, mode, work, ctx, a.out)
            if st is None:
                print('%-6s %-6s %-5s %9s' % (ctx, mode, work, 'NO DATA'))
                continue
            fps, pty, shm = st
            interesting = ('wlterm', 'kitty', 'tuios', 'wlbench', 'python3')
            cs = ' '.join('%s=%.1f' % (k, v) for k, v in
                          sorted(cpu.items(), key=lambda kv: -kv[1])
                          if k in interesting and v >= 0.05)
            print('%-6s %-6s %-5s %9s %8.1f %9.1f %10.0f %11.1f   %s' %
                  (ctx, mode, work, size, fps, seen, pty / max(fps, 1e-9),
                   shm / max(fps, 1e-9) / 1024, cs))
            sys.stdout.flush()
