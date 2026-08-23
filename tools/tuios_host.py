#!/usr/bin/env python3
"""Run tuios on a pty *inside a real terminal* and relay both directions.

tools/tuios_pane.py answers tuios's capability probes itself, which makes the
pane real but the host fake. This one does the opposite: it puts tuios on a
pty and passes every byte through to whatever terminal it is running in, so
tuios probes the real host, gets the real host's answers, and its kitty
graphics passthrough reaches the real host's decoder.

Run it under `wlterm -pixels ... -snapshots DIR -- kitty -e python3
tools/tuios_host.py ...` and the snapshots are what the human would see.

Nothing is captured to disk. The stream is megabytes a second and filling
/tmp is how the shell broke last time; --esc-out keeps counts, not bytes.
"""
import argparse, fcntl, os, pty, re, select, signal, struct, sys, termios, time

ap = argparse.ArgumentParser()
ap.add_argument('--priv', default='/tmp/tuios-host-priv',
                help='throwaway XDG dirs, so tuios neither restores nor '
                     'overwrites the real session')
ap.add_argument('--tuios', default='tuios', help='tuios binary to run')
ap.add_argument('--cmd', required=True, help='typed into the first pane')
ap.add_argument('--seconds', type=float, default=30)
ap.add_argument('--settle', type=float, default=4.0)
ap.add_argument('--pre', default='n,i',
                help='key steps sent 1.5s apart before --cmd '
                     '(tuios: n = new window, i = enter terminal mode)')
ap.add_argument('--type-after', type=float, default=2.0)
ap.add_argument('--post', action='append', default=[],
                help='SECONDS@BYTES sent to tuios at that offset; \\x and \\r '
                     'honoured. Repeatable.')
ap.add_argument('--esc-out', default='',
                help='per-second counts of kitty graphics escapes and bytes '
                     'leaving tuios for the host terminal')
ap.add_argument('--log', default='', help='harness log file')
ap.add_argument('--resize-at', type=float, default=0,
                help='seconds after the start at which to shrink the pty tuios '
                     'sits on, so tuios has to resize its panes')
ap.add_argument('--resize-to', default='', help='COLSxROWS for --resize-at')
a = ap.parse_args()

LOG = open(a.log, 'w') if a.log else open(os.devnull, 'w')
def log(*m): print(*m, file=LOG); LOG.flush()

# The pane must be the real terminal's geometry, pixels included: tuios reports
# it onward to the guest and wlterm sizes its canvas from it.
try:
    ws = fcntl.ioctl(sys.stdin.fileno(), termios.TIOCGWINSZ, b'\0' * 8)
    rows, cols, xpix, ypix = struct.unpack('HHHH', ws)
except OSError:
    rows, cols, xpix, ypix = 50, 200, 2000, 1000
log('host tty: %dx%d cells, %dx%d px' % (cols, rows, xpix, ypix))

os.makedirs(a.priv, exist_ok=True)
pid, mfd = pty.fork()
if pid == 0:
    env = dict(os.environ)
    env['SHELL'] = '/bin/sh'
    env['ENV'] = ''
    env['PS1'] = 'pane$ '
    for k in ('XDG_CONFIG_HOME', 'XDG_STATE_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME'):
        env[k] = a.priv + '/' + k.split('_')[1].lower()
    os.execvpe(a.tuios, [a.tuios], env)

fcntl.ioctl(mfd, termios.TIOCSWINSZ, struct.pack('HHHH', rows, cols, xpix, ypix))

infd = sys.stdin.fileno()
old = termios.tcgetattr(infd)
import tty as _tty
_tty.setraw(infd)
outfd = sys.stdout.fileno()

esc_buckets, esc_n, esc_bytes, esc_sec, carry = [], 0, 0, 0, b''
t0 = time.time()
steps = [x for x in a.pre.split(',') if x] if a.pre else []
pre_sent, typed, post_sent, resized = 0, False, set(), False
try:
    while time.time() - t0 < a.seconds:
        now = time.time() - t0
        while pre_sent < len(steps) and now >= a.settle + 1.5 * pre_sent:
            os.write(mfd, steps[pre_sent].encode()); pre_sent += 1
        if not typed and now >= a.settle + 1.5 * len(steps) + a.type_after:
            os.write(mfd, (a.cmd + '\r').encode()); typed = True
            log('typed at t=%.1f: %s' % (now, a.cmd))
        if a.resize_at and not resized and now >= a.resize_at:
            c, r = (int(x) for x in a.resize_to.split('x'))
            fcntl.ioctl(mfd, termios.TIOCSWINSZ,
                        struct.pack('HHHH', r, c, c * (xpix // max(cols, 1)),
                                    r * (ypix // max(rows, 1))))
            log('pane -> %dx%d cells at t=%.1f' % (c, r, now))
            resized = True
        for i, spec in enumerate(a.post):
            when, _, raw = spec.partition('@')
            if i not in post_sent and now >= float(when):
                os.write(mfd, raw.encode().decode('unicode_escape').encode('latin1'))
                post_sent.add(i)
                log('post %d at t=%.1f' % (i, now))
        r, _, _ = select.select([mfd, infd], [], [], 0.05)
        if infd in r:
            try:
                d = os.read(infd, 65536)
            except OSError:
                d = b''
            if d:
                os.write(mfd, d)
        if mfd in r:
            try:
                d = os.read(mfd, 65536)
            except OSError:
                break
            if not d:
                break
            os.write(outfd, d)
            scan = carry + d
            esc_n += scan.count(b'\x1b_G')
            carry = scan[-2:]
            esc_bytes += len(d)
            while now >= esc_sec + 1:
                esc_buckets.append((esc_sec, esc_n, esc_bytes))
                esc_n = esc_bytes = 0
                esc_sec += 1
finally:
    termios.tcsetattr(infd, termios.TCSADRAIN, old)
    if a.esc_out:
        with open(a.esc_out, 'w') as f:
            for t, n, b in esc_buckets:
                f.write('%d\t%d\t%d\n' % (t, n, b))
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
    LOG.close()
