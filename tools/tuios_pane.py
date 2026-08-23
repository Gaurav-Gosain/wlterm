#!/usr/bin/env python3
"""Run tuios on a pty, answer its capability probes like kitty would, type a
command into its first pane, and capture the stream.

The point is that wlterm's child sees a *real tuios pane* pty -- the
Xpixel/Ypixel tuios sets, the CSI 14t/16t tuios answers, the kitty-graphics
passthrough tuios's vt performs -- not a terminal we faked underneath it.

Capture is hard-capped. A b64 kitty-graphics stream is megabytes a second and
filling /tmp is how the shell broke last time.
"""
import os, pty, sys, time, fcntl, termios, struct, signal, argparse, select, re

ap = argparse.ArgumentParser()
ap.add_argument('--priv', default='/tmp/wlterm-pane-priv',
                help='throwaway XDG dirs for tuios, so it neither restores nor '
                     'overwrites the real session')
ap.add_argument('--cols', type=int, default=200)
ap.add_argument('--rows', type=int, default=50)
ap.add_argument('--cellw', type=int, default=10)
ap.add_argument('--cellh', type=int, default=20)
ap.add_argument('--cmd', required=True)
ap.add_argument('--out', default='')
ap.add_argument('--seconds', type=float, default=20)
ap.add_argument('--settle', type=float, default=4.0)
ap.add_argument('--pre', default='n,i',
                help='comma-separated key steps sent 1.5s apart before --cmd '
                     '(tuios: n = new window, i = enter terminal mode)')
ap.add_argument('--type-after', type=float, default=2.0, help='delay after the last --pre step')
ap.add_argument('--cap-mb', type=int, default=32)
ap.add_argument('--resize-at', type=float, default=0)
ap.add_argument('--resize-to', default='')
ap.add_argument('--cpu-out', default='', help='sum CPU seconds over the pane process tree')
ap.add_argument('--host-rate', default='',
                help='count kitty graphics escapes leaving tuios for the host '
                     'terminal, per second, without storing the bytes. This is '
                     'the far end of the chain: what the human would see.')
ap.add_argument('--post', action='append', default=[],
                help='SECONDS@BYTES, sent at that offset from the start of the '
                     'run; BYTES honours \\x and \\r escapes. Repeatable.')
a = ap.parse_args()

cols, rows = a.cols, a.rows

def setsize(fd, c, r):
    fcntl.ioctl(fd, termios.TIOCSWINSZ,
                struct.pack('HHHH', r, c, c*a.cellw, r*a.cellh))

os.makedirs(a.priv, exist_ok=True)

pid, mfd = pty.fork()
if pid == 0:
    env = dict(os.environ)
    env['TERM'] = 'xterm-kitty'
    env['KITTY_WINDOW_ID'] = '1'
    # A plain shell in the pane. The login shell here auto-starts a file
    # manager, and typing a command into a file manager drives its menus.
    env['SHELL'] = '/bin/sh'
    env['ENV'] = ''
    env['PS1'] = 'pane$ '
    # A private config and state dir. Without it tuios restores the user's
    # saved session, which here opened a file manager -- and typing a command
    # into a file manager drives its menus.
    priv = a.priv
    env['XDG_CONFIG_HOME'] = priv + '/config'
    env['XDG_STATE_HOME'] = priv + '/state'
    env['XDG_DATA_HOME'] = priv + '/data'
    env['XDG_CACHE_HOME'] = priv + '/cache'
    for k in ('WAYLAND_DISPLAY', 'DISPLAY', 'DBUS_SESSION_BUS_ADDRESS'):
        env.pop(k, None)
    os.execvpe('tuios', ['tuios'], env)

pid_root = pid
setsize(mfd, cols, rows)

# --- the parts of kitty tuios actually probes for -------------------------
G = re.compile(rb'\x1b_G([^;\x1b]*)(?:;[^\x1b]*)?\x1b\\')
CSI = re.compile(rb'\x1b\[(\?)?([0-9;]*)([a-zA-Z$][pyc]?)')

def respond(chunk):
    out = b''
    for m in G.finditer(chunk):
        params = dict(p.split(b'=', 1) for p in m.group(1).split(b',') if b'=' in p)
        # kitty stays silent when q>=1. wlterm sends q=2 on every frame, so
        # this is also what keeps us from answering the guest's own traffic.
        if int(params.get(b'q', b'0')) != 0:
            continue
        out += b'\x1b_Gi=' + params.get(b'i', b'0') + b';OK\x1b\\'
    for m in CSI.finditer(chunk):
        priv, args, fin = m.group(1), m.group(2), m.group(3)
        if fin == b't' and not priv:
            if args == b'14':
                out += b'\x1b[4;%d;%dt' % (rows*a.cellh, cols*a.cellw)
            elif args == b'16':
                out += b'\x1b[6;%d;%dt' % (a.cellh, a.cellw)
        elif fin == b'c' and not priv:
            out += b'\x1b[?62;4;22c'
        elif fin == b'u' and priv and args == b'':
            out += b'\x1b[?1u'          # kitty keyboard: all flags available
        elif fin == b'$p' and priv:
            out += b'\x1b[?%s;1$y' % args
    return out

HZ = os.sysconf('SC_CLK_TCK')

def proc_tree(root):
    """pid -> (name, cpu_seconds) for root and every descendant.

    Sampled repeatedly and kept per pid, because a process that has exited
    has no /proc entry left to read and its time would otherwise vanish.
    """
    kids = {}
    info = {}
    for d in os.listdir('/proc'):
        if not d.isdigit():
            continue
        try:
            with open(f'/proc/{d}/stat') as f:
                st = f.read()
        except OSError:
            continue
        i = st.rindex(')')
        name = st[st.index('(')+1:i]
        f = st[i+2:].split()
        ppid = int(f[1])
        cpu = (int(f[11]) + int(f[12])) / HZ
        kids.setdefault(ppid, []).append(int(d))
        info[int(d)] = (name, cpu)
    out, stack = {}, [root]
    while stack:
        pid = stack.pop()
        if pid in info:
            out[pid] = info[pid]
        stack.extend(kids.get(pid, []))
    return out

cpu_peak = {}

cap = a.cap_mb * 1024 * 1024
written = 0
# Per-second buckets of (kitty graphics escapes, bytes) on tuios's own output.
# Counted in flight and thrown away, because a 30s capture of this stream is
# hundreds of megabytes and the only thing wanted from it is a rate.
host_buckets = []
host_esc = host_bytes = 0
host_sec = 0
host_carry = b''
t0 = time.time()
typed = resized = False
pre_sent = 0
post_sent = set()
out = open(a.out, 'wb') if a.out else None
tail = b''
try:
    while time.time() - t0 < a.seconds:
        now = time.time() - t0
        steps = [x for x in a.pre.split(',') if x] if a.pre else []
        while pre_sent < len(steps) and now >= a.settle + 1.5 * pre_sent:
            os.write(mfd, steps[pre_sent].encode())
            pre_sent += 1
        if not typed and now >= a.settle + 1.5 * len(steps) + a.type_after:
            os.write(mfd, (a.cmd + '\r').encode())
            typed = True
        if a.resize_at and not resized and now >= a.resize_at:
            cols, rows = (int(x) for x in a.resize_to.split('x'))
            setsize(mfd, cols, rows)
            print(f'[harness] host pane -> {cols}x{rows} cells '
                  f'({cols*a.cellw}x{rows*a.cellh} px) at t={now:.1f}s', file=sys.stderr)
            resized = True
        for i, spec in enumerate(a.post):
            when, _, raw = spec.partition('@')
            if i not in post_sent and now >= float(when):
                os.write(mfd, raw.encode().decode('unicode_escape').encode('latin1'))
                post_sent.add(i)
        if a.cpu_out:
            for pid, (name, cpu) in proc_tree(pid_root).items():
                k = (pid, name)
                if cpu > cpu_peak.get(k, -1):
                    cpu_peak[k] = cpu
        rl, _, _ = select.select([mfd], [], [], 0.2)
        if not rl:
            continue
        try:
            data = os.read(mfd, 65536)
        except OSError:
            break
        if not data:
            break
        r = respond(tail + data)
        tail = data[-256:]
        if r:
            os.write(mfd, r)
        if out and written < cap:
            out.write(data[:cap - written])
        written += len(data)
        if a.host_rate:
            scan = host_carry + data
            host_esc += scan.count(b'\x1b_G')
            host_carry = scan[-2:]
            host_bytes += len(data)
            while now >= host_sec + 1:
                host_buckets.append((host_sec, host_esc, host_bytes))
                host_esc = host_bytes = 0
                host_sec += 1
finally:
    if out:
        out.close()
    if a.host_rate:
        with open(a.host_rate, 'w') as f:
            for t, e, b in host_buckets:
                f.write(f'{t}\t{e}\t{b}\n')
    os.write(mfd, b'\x02q')
    time.sleep(0.5)
    for sig in (signal.SIGTERM, signal.SIGKILL):
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            break
        time.sleep(0.3)
    try:
        os.waitpid(pid, os.WNOHANG)
    except ChildProcessError:
        pass
print(f'[harness] {written/1e6:.1f} MB from the pane, {min(written, cap)/1e6:.1f} MB kept',
      file=sys.stderr)
if a.cpu_out:
    tot = {}
    for (pid, name), cpu in cpu_peak.items():
        tot[name] = tot.get(name, 0.0) + cpu
    with open(a.cpu_out, 'w') as f:
        for name, cpu in sorted(tot.items(), key=lambda kv: -kv[1]):
            if cpu >= 0.05:
                f.write(f'{name}\t{cpu:.2f}\n')
        f.write(f'TOTAL\t{sum(tot.values()):.2f}\n')
