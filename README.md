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

| | llvmpipe via `wl_shm` | i915 via LINEAR dmabuf |
|---|---|---|
| fps | 50.9 | 51.1 |
| client CPU | 58.0 s | **8.3 s** |
| whole tree CPU | 73.3 s | **23.8 s** |
| wlterm CPU | 4.29 s | 4.17 s |
| compositor read | 226 us | 995 us (783 us of it the GPU fence, 212 us memcpy) |

Same frame rate, a seventh of the client's CPU. The compositor's own memcpy is
the same either way, which is the whole point of the linear modifier.

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
| `-fps` | frame cap |
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
- `wlbench/` -- a minimal animating Wayland client.
- `-snapshots DIR` -- composited PNGs straight out of the canvas.

## Known gaps

Client cursors are dropped, so the pointer works but is invisible. `xdg_popup`
is accepted but menus are drawn nowhere. Clipboard is a stub.
