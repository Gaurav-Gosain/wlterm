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
		// repeat_info arrived in wl_seat version 4. Sending it to a client
		// that bound v1-v3 is an event it has no listener slot for, and a
		// libwayland client aborts on that rather than ignoring it: vkcube
		// died with "listener function for opcode 5 of wl_keyboard is NULL"
		// before this check existed.
		if s.version >= 4 {
			c.event(kid, 5, int32(30), int32(400)) // repeat_info: rate 30, delay 400ms
		}
		// If this client already owns the focused surface, hand it focus
		// now: a client may bind the keyboard after its window was mapped.
		if f := c.comp.kbFocus; f != nil && f.client == c {
			c.event(kid, 1, c.comp.nextSerial(), f.id, []byte{})
			c.event(kid, 4, c.comp.nextSerial(), c.comp.seatState.mods, uint32(0), uint32(0), uint32(0))
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
	mods      uint32
	lastX     wlFixed
	lastY     wlFixed
	lastPX    int // last pointer position in canvas pixels
	lastPY    int
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

// dropClient forgets every seat resource a disconnected client held.
func (ss *seatState) dropClient(c *client) {
	ps := ss.pointers[:0]
	for _, p := range ss.pointers {
		if p.c != c {
			ps = append(ps, p)
		}
	}
	ss.pointers = ps
	ks := ss.keyboards[:0]
	for _, k := range ss.keyboards {
		if k.c != c {
			ks = append(ks, k)
		}
	}
	ss.keyboards = ks
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
