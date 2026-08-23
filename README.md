# wlterm

A Wayland compositor that lives inside a terminal pane. Clients render into
`wl_shm` or a LINEAR `dmabuf`; wlterm composites them and writes the pixels to
its own stdout as kitty graphics. Keyboard and mouse escape sequences flow the
other way and become `wl_keyboard` and `wl_pointer` events.

Pure Go. No cgo, no libwayland, no wlroots, no EGL. The wire protocol is
hand-implemented.

```sh
go build -o wlterm .
./wlterm -- foot          # a Wayland terminal in your terminal
```

## Single-app mode (the default)

One toplevel, filling the pane, and nothing else. It is the mode for running a
Wayland program inside a multiplexer, because a multiplexer already draws the
border, the title and the focus ring, and already owns the leader key.

- **No chrome.** wlterm draws no frame, no title, no focus ring, no dock. The
  client gets the pane exactly, in pixels, not quantised to whole cells: at a
  10x20 cell that is 1280x720 where the tiling mode would hand it 1260x640.
  What is left of the chrome layer is one flood fill, run once per resize
  rather than per frame, and it is the smaller half of the saving: the tile
  covers the canvas, so the root image is composited but never transmitted.
- **No leader key.** Every keystroke goes to the app. This is the concrete
  reason single-app exists: wlterm's leader was `ctrl+b` and so is tuios's, so
  inside a tuios pane the leader never arrived and nothing it guarded was
  reachable.
- **Resizes with the pane.** `SIGWINCH` re-probes the host for the pane's pixel
  geometry and reconfigures the toplevel to match.
- **Exits when the app exits**, and on `SIGHUP`/`SIGTERM`, so closing the pane
  closes it.

The escape hatch is `ctrl+\` **tapped twice inside 700ms**. A single press
still reaches the app, and tuios does not bind it. `-quit-key none` intercepts
nothing at all; `-quit-key ctrl+q` picks something else.

### The transport picks itself, and being inside a multiplexer is the deciding fact

`-mode auto` picks `shm` whenever a multiplexer marker (`TUIOS_SESSION`,
`TMUX`, `ZELLIJ`, `STY`) is in the environment, and `delta` only when `TERM`
itself says kitty.

Delta mode patches an image in place with kitty animation frames, which is 81
bytes instead of megabytes for a small update, but tuios's vt acknowledges
`a=f` and drops it, so under tuios delta mode freezes silently rather than
failing. The heuristic this replaces also keyed off `KITTY_WINDOW_ID` -- and
that variable is inherited straight through a multiplexer while `TERM` is
rewritten, so `wlterm -- foot` in a tuios pane inside kitty used to pick
exactly the transport that cannot work there.

Inside tuios `shm` costs nothing anyway: the pty carries a ~70 byte escape per
frame (measured at 245 B/s with an idle client) while the pixels ride tmpfs.

## Multi-surface mode

`-multi` restores the tiling compositor: BSP and master-stack layouts, a
tuios-shaped frame per window, a dock, a leader key (`-prefix`, default
`ctrl+b`) and an application launcher over desktop entries with frecency.

```sh
./wlterm -multi -- foot
```

Nothing about it was deleted to make single-app the default.

## The frame cap, and why it read 49

`-fps` names a rate. It did not used to deliver one.

The render loop slept the cap interval measured from the **end** of the
previous frame, so every period came out as the interval *plus* a whole
frame's cost. With a 5ms frame, `-fps 60` produced 21.7ms periods: a cap of
60 delivering 46. On top of that, every frame left one spare token in the
render channel -- a commit calls `markDirty` for its damage and again for the
frame callback it owes -- and the loop paid a full interval for that token
before discovering there was nothing to draw. Two independent leaks, both
paid once per frame.

It hid because every benchmark here ran `-fps 1000`, where the interval is
1ms and the frame cost swamps the error. Only `run_pane.sh` used the default,
and that is the run that reported 49.

The cap is now honoured to the tenth. Headless, 1280x720, `wl_shm`,
`wlbench -work full`:

| `-fps` | before | after |
|---|---|---|
| 30 | 22.5 | **30.0** |
| 60 | 45.8 | **60.0** |
| 120 | 73.3 | **120.0** |
| 1000 | 164.4 | **190.7** |

`stats:` grew `pace=` and `idle=` for this, so `composite + encode + write +
pace + idle` now accounts for the whole period and a disappointing frame rate
can be attributed instead of guessed at: `idle` is the guest rendering,
`pace` is our own cap. `empty_wakes=` counts the tokens that had nothing
behind them.

**The default is 120, not 60.** Sixty is a monitor's number and nothing in
this chain is a monitor. tuios coalesces a pane no faster than every 8ms
(125fps) and kitty repaints no faster than its `repaint_delay` (10ms, 100fps),
so a cap of 60 was throwing away half the smoothness the host would have
displayed. `-fps 0` is uncapped.

### Where a frame's time goes

Uncapped in a tuios pane at 980x440, kitty on the i915 through a LINEAR
dmabuf, 213.9 fps, so 4895 us a frame:

| stage | | |
|---|---|---|
| `idle` | 2646 us | the guest rendering, and our readback of what it drew |
| `composite` | 1489 us | blending the tile onto the canvas |
| `encode` | 740 us | the pixels into a `/dev/shm` slot |
| `write` | 19 us | ~46 bytes of kitty graphics escape down the pty |

wlterm's own share is 2248 us, so the compositor alone would run at about
445fps. The guest is what it waits for, which is the right answer: a
compositor should be cheaper than the thing it is compositing.

**Nothing downstream throttles this.** Counting the escapes tuios forwards to
the host terminal (`tuios_pane.py --host-rate`) gives exactly two per frame at
every rate tried -- 120.0/s at 60fps, 237.9/s at 118.9fps, 431.2/s at
213.9fps -- for 0.01 to 0.03 MB/s. In `shm` mode the pixels ride tmpfs and
what crosses the pty is a filename, so tuios's coalescer never sees a backlog
worth pacing and its graphics pacer never sees a frame worth holding. That is
what the transport was for, and it is why `-mode shm` is chosen inside a
multiplexer.

## dmabuf, and what the GPU is worth

wlterm's output is kitty graphics, so every frame has to reach system memory to
be encoded no matter how it was drawn. GPU rendering only pays if getting the
pixels back is cheap.

So wlterm advertises `zwp_linux_dmabuf_v1` with exactly one modifier:
`DRM_FORMAT_MOD_LINEAR`. A linear dmabuf on an integrated GPU is ordinary
cacheable system memory, so the readback is the same `mmap` and `memcpy` that
`wl_shm` already used. No EGL context, no `glReadPixels`, no GPU-to-CPU
readback path at all. Buffers that are not linear are refused.

Measured in a real tuios pane at 980x440, same kitty, same workload, 30s
(`./run_pane.sh`):

| uncapped (`-fps 0`) | llvmpipe via `wl_shm` | i915 via LINEAR dmabuf |
|---|---|---|
| fps | 109.6 | **213.9** |
| whole pane tree CPU | 144.4 s | **46.6 s** |
| guest CPU | 119.7 s | **9.2 s** |
| compositor read | 231 us | 904 us (676 us of it the GPU fence, 228 us memcpy) |

Twice the frame rate on a third of the CPU.

That is not what this table used to say. It used to report 49.5 against 49.6
-- *the same frame rate* -- and conclude that the GPU bought CPU and nothing
else. The frame rates were equal because both were pinned by wlterm's own
broken frame cap, which sat at 49 whatever was underneath it. The guest was
rendering inside our sleep, so making the guest seven times cheaper moved
nothing. Fix the cap and the GPU is worth exactly what you would expect it to
be worth. **A benchmark where two very different configurations agree to
three significant figures is measuring the harness, not the subject.**

The compositor's read looks worse until you split it: 676 us of it is
`DMA_BUF_IOCTL_SYNC` waiting on the implicit fence, which is the client's
render finishing rather than any work of ours. What we actually do is the
`memcpy`, and that is the same size either way. That is the whole point of the
linear modifier.

At the default cap of 120 the i915 path is held by the cap: 118.9 fps for
33.6 s of tree CPU. llvmpipe reaches 108.7 with the cap barely engaging at all
(22 us of `pace` a frame), because its own render cost lands just under the
cap. Which is what a cap should do: bound the fast path and stay out of the
way of the slow one.

**Vulkan needs no Vulkan-specific code.** `vkcube` renders through
`VK_KHR_wayland_surface` on the Intel GPU and Mesa's WSI allocates a dmabuf
like everything else; LINEAR is all wlterm offers, so it allocates linear.

### The render node matters more than anything else here

wlterm prefers a node whose driver is known to hand out CPU-cacheable buffers
(`i915`, `xe`, `amdgpu`, ...) and warns loudly when the first large read runs
below 1 GB/s.

**NVIDIA is a hard no.** On the machine this was built on, `renderD128`
(nvidia) `mmap`s successfully and then reads at 0.015 GB/s: 242 ms for one
1280x720 frame. A compositor that accepts such a buffer looks hung rather than
broken. `-drm NODE` overrides the choice; `-no-dmabuf` forces clients back onto
`wl_shm`.

## Flags

| flag | meaning |
|---|---|
| `-multi` | multi-surface tiling mode (default: single app) |
| `-quit-key` | escape hatch, tapped twice; `none` intercepts nothing |
| `-mode` | `auto` \| `delta` \| `shm` \| `b64` transport |
| `-layers` | `per-window` \| `single` image granularity |
| `-fps` | frame cap, `0` for uncapped (default 120) |
| `-no-dmabuf` | do not advertise `zwp_linux_dmabuf_v1` |
| `-drm NODE` | render node to advertise as dmabuf `main_device` |
| `-prefix` | leader key, `-multi` only |
| `-spawn` / `-term` | commands the launcher uses, `-multi` only |
| `-exec CMD` | extra client at startup, repeatable |
| `-isolate` | private runtime dir and session bus per child (default on) |
| `-pixels` / `-cell` | headless: no tty setup, fixed canvas |
| `-snapshots` | write composited PNGs for verification |

## Safety

wlterm crashed a desktop session once, and the rules come from that.

- It binds **its own** socket under a private runtime dir and never dials or
  inherits the host's `WAYLAND_DISPLAY`.
- Children get a private `XDG_RUNTIME_DIR` and a private session bus. Stripping
  `WAYLAND_DISPLAY` alone is not enough: most GTK and KDE apps are launched
  through the bus, which routes the request to the copy already running on the
  host desktop, opens a window there, and exits 0 looking like a success.
- Client buffers are treated as hostile. `wl_shm` pools and dmabuf fds are
  `fstat`-validated before mapping, buffers are bounds-checked, and reads run
  under `debug.SetPanicOnFault` because a client can truncate after validation.
- Outbound frames use a fixed 8-slot ring of `/dev/shm` names, unlinked on
  reuse and on every exit path, so a terminal that never unlinks (tuios does
  not) cannot make us leak tmpfs at frame rate.
- Every resource-owning object is swept when a client disconnects.

## Verifying

- `./run_pane.sh` -- everything above, inside a real tuios pane.
- `./run_safety.sh`, `dmaevil/` -- hostile clients. Every case must end with
  the client refused and the compositor alive.
- `kittydec/` -- decodes wlterm's own output stream back into PNGs, which is
  how "it rendered" is checked rather than assumed.
- `wlbench/` -- a minimal animating Wayland client. `-fps N` against it is how
  the frame cap is checked: it has to come back as N.
- `-snapshots DIR` -- composited PNGs straight out of the canvas.

## Known gaps

Client cursors are dropped, so the pointer works but is invisible. `xdg_popup`
is accepted but menus are drawn nowhere. Clipboard is a stub.
