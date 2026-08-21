package main

// wl_seat, wl_pointer, wl_keyboard. The keymap is generated once with
// `xkbcli compile-keymap` and shipped to every keyboard via memfd.

import (
	"os/exec"
	"syscall"
	"time"
)

type wlSeat struct {
	version uint32
}

func (s *wlSeat) iface() string { return "wl_seat" }
func (s *wlSeat) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // get_pointer
		pid := r.uint()
		c.objects[pid] = wlPointer{}
		c.comp.seatState.pointers = append(c.comp.seatState.pointers, seatRes{c, pid})
	case 1: // get_keyboard
		kid := r.uint()
		c.objects[kid] = wlKeyboard{}
		c.comp.seatState.keyboards = append(c.comp.seatState.keyboards, seatRes{c, kid})
		km := c.comp.keymap()
		if km != nil {
			c.event(kid, 0, uint32(1), fdArg(km.fd), uint32(km.size)) // keymap xkb_v1
		}
		c.event(kid, 5, int32(30), int32(400)) // repeat_info: rate 30, delay 400ms
		// If the toplevel is already up, focus it now.
		if t := c.comp.toplevel; t != nil && t.client == c && c.comp.focusSent {
			c.event(kid, 1, c.comp.nextSerial(), t.surf.id, []byte{}) // enter
			c.comp.sendModifiers()
		}
	case 2: // get_touch
		c.objects[r.uint()] = wlTouch{}
	case 3: // release
		c.deleteID(id)
	}
}

type seatRes struct {
	c  *client
	id uint32
}

type seatState struct {
	pointers  []seatRes
	keyboards []seatRes
	ptrIn     bool // pointer has entered the surface
	mods      uint32
	lastX     wlFixed
	lastY     wlFixed
}

type wlPointer struct{}

func (wlPointer) iface() string { return "wl_pointer" }
func (wlPointer) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // set_cursor: ignored, terminal owns the cursor
	case 1: // release
		c.comp.seatState.dropPointer(c, id)
		c.deleteID(id)
	}
}

type wlKeyboard struct{}

func (wlKeyboard) iface() string { return "wl_keyboard" }
func (wlKeyboard) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 0 {
		c.comp.seatState.dropKeyboard(c, id)
		c.deleteID(id)
	}
}

type wlTouch struct{}

func (wlTouch) iface() string { return "wl_touch" }
func (wlTouch) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 0 {
		c.deleteID(id)
	}
}

func (ss *seatState) dropPointer(c *client, id uint32) {
	for i, p := range ss.pointers {
		if p.c == c && p.id == id {
			ss.pointers = append(ss.pointers[:i], ss.pointers[i+1:]...)
			return
		}
	}
}
func (ss *seatState) dropKeyboard(c *client, id uint32) {
	for i, k := range ss.keyboards {
		if k.c == c && k.id == id {
			ss.keyboards = append(ss.keyboards[:i], ss.keyboards[i+1:]...)
			return
		}
	}
}

type keymapInfo struct {
	fd   int
	size int
}

var cachedKeymap *keymapInfo

// keymap compiles a keymap once and keeps it in a memfd.
func (comp *compositor) keymap() *keymapInfo {
	comp.keymapOnce.Do(func() {
		out, err := exec.Command("xkbcli", "compile-keymap", "--layout", "us").Output()
		if err != nil {
			logf("xkbcli failed: %v", err)
			return
		}
		fd, err := memfdCreate("wlterm-keymap")
		if err != nil {
			logf("memfd: %v", err)
			return
		}
		if _, err := syscall.Write(fd, out); err != nil {
			logf("keymap write: %v", err)
			return
		}
		cachedKeymap = &keymapInfo{fd: fd, size: len(out)}
	})
	return cachedKeymap
}

func memfdCreate(name string) (int, error) {
	nameb, _ := syscall.BytePtrFromString(name)
	r0, _, errno := syscall.Syscall(319 /* memfd_create */, pointerOf(nameb), 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r0), nil
}

func nowMs() uint32 { return uint32(time.Now().UnixNano() / 1e6) }

// ---- event fan-out, called with comp.mu held ----

func (comp *compositor) focusedSurface() *wlSurface {
	if comp.toplevel != nil {
		return comp.toplevel.surf
	}
	return nil
}

func (comp *compositor) pointerMotion(x, y float64) {
	surf := comp.focusedSurface()
	if surf == nil {
		return
	}
	ss := &comp.seatState
	fx, fy := fixed(x), fixed(y)
	ss.lastX, ss.lastY = fx, fy
	for _, p := range ss.pointers {
		if p.c != surf.client {
			continue
		}
		if !ss.ptrIn {
			p.c.event(p.id, 0, comp.nextSerial(), surf.id, fx, fy) // enter
		}
		p.c.event(p.id, 2, nowMs(), fx, fy) // motion
		p.c.event(p.id, 5)                  // frame
		p.c.flush()
	}
	ss.ptrIn = true
}

func (comp *compositor) pointerButton(btn uint32, pressed bool) {
	surf := comp.focusedSurface()
	if surf == nil {
		return
	}
	ss := &comp.seatState
	state := uint32(0)
	if pressed {
		state = 1
	}
	for _, p := range ss.pointers {
		if p.c != surf.client {
			continue
		}
		if !ss.ptrIn {
			p.c.event(p.id, 0, comp.nextSerial(), surf.id, ss.lastX, ss.lastY)
			ss.ptrIn = true
		}
		p.c.event(p.id, 3, comp.nextSerial(), nowMs(), btn, state) // button
		p.c.event(p.id, 5)                                         // frame
		p.c.flush()
	}
}

func (comp *compositor) pointerAxis(vertical bool, value float64) {
	surf := comp.focusedSurface()
	if surf == nil {
		return
	}
	axis := uint32(0) // vertical
	if !vertical {
		axis = 1
	}
	for _, p := range comp.seatState.pointers {
		if p.c != surf.client {
			continue
		}
		p.c.event(p.id, 4, nowMs(), axis, fixed(value)) // axis
		p.c.event(p.id, 5)                              // frame
		p.c.flush()
	}
}

func (comp *compositor) key(code uint32, pressed bool) {
	surf := comp.focusedSurface()
	if surf == nil {
		return
	}
	state := uint32(0)
	if pressed {
		state = 1
	}
	for _, k := range comp.seatState.keyboards {
		if k.c != surf.client {
			continue
		}
		k.c.event(k.id, 3, comp.nextSerial(), nowMs(), code, state) // key
		k.c.flush()
	}
}

// setModifiers takes an xkb mod mask (shift=1, ctrl=4, mod1=8, mod4=64).
func (comp *compositor) setModifiers(mask uint32) {
	if comp.seatState.mods == mask {
		return
	}
	comp.seatState.mods = mask
	comp.sendModifiers()
}

func (comp *compositor) sendModifiers() {
	surf := comp.focusedSurface()
	if surf == nil {
		return
	}
	for _, k := range comp.seatState.keyboards {
		if k.c != surf.client {
			continue
		}
		k.c.event(k.id, 4, comp.nextSerial(), comp.seatState.mods, uint32(0), uint32(0), uint32(0))
		k.c.flush()
	}
}

// keyboardEnter is sent once the toplevel's first buffer lands.
func (comp *compositor) keyboardEnter() {
	surf := comp.focusedSurface()
	if surf == nil {
		return
	}
	for _, k := range comp.seatState.keyboards {
		if k.c != surf.client {
			continue
		}
		k.c.event(k.id, 1, comp.nextSerial(), surf.id, []byte{})
		k.c.flush()
	}
}
