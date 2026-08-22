package main

// Seat routing across many surfaces.
//
// The single-toplevel version had one implicit focus: every key went to the
// only client and the pointer never entered or left anything. With several
// tiles that is a protocol violation waiting to happen (it is exactly what
// segfaulted foot during the first experiment), so focus is now explicit:
// wl_keyboard.leave/enter on every focus change, and wl_pointer.enter/leave
// as the pointer crosses a tile boundary, with surface-local coordinates.

// setFocus moves the tiler's focus and takes the keyboard with it.
func (comp *compositor) setFocus(w *window) {
	if comp.focus == w {
		return
	}
	comp.focus = w
	if w != nil && w.top != nil && w.top.surf != nil && w.top.surf.mapped {
		comp.setKeyboardFocus(w.top.surf)
	} else {
		comp.setKeyboardFocus(nil)
	}
	// The activated state is part of the configure, so clients repaint
	// their own title bars and cursors correctly.
	comp.configureAll()
	// Nothing in single-app mode is drawn differently when focus moves:
	// there is no focus ring of wlterm's own and no dock to update.
	if !comp.single {
		comp.chromeDirty = true
	}
	comp.markDirty()
}

// setKeyboardFocus delivers leave to the old surface and enter to the new.
func (comp *compositor) setKeyboardFocus(s *wlSurface) {
	if comp.kbFocus == s {
		return
	}
	old := comp.kbFocus
	comp.kbFocus = s
	if old != nil {
		for _, k := range comp.seatState.keyboards {
			if k.c == old.client {
				k.c.event(k.id, 2, comp.nextSerial(), old.id) // leave
				k.c.flush()
			}
		}
	}
	if s != nil {
		for _, k := range comp.seatState.keyboards {
			if k.c == s.client {
				k.c.event(k.id, 1, comp.nextSerial(), s.id, []byte{}) // enter
				k.c.event(k.id, 4, comp.nextSerial(), comp.seatState.mods, uint32(0), uint32(0), uint32(0))
				k.c.flush()
			}
		}
	}
}

func (comp *compositor) key(code uint32, pressed bool) {
	s := comp.kbFocus
	if s == nil {
		return
	}
	state := uint32(0)
	if pressed {
		state = 1
	}
	for _, k := range comp.seatState.keyboards {
		if k.c != s.client {
			continue
		}
		k.c.event(k.id, 3, comp.nextSerial(), nowMs(), code, state)
		k.c.flush()
	}
}

func (comp *compositor) setModifiers(mask uint32) {
	if comp.seatState.mods == mask {
		return
	}
	comp.seatState.mods = mask
	comp.sendModifiers()
}

func (comp *compositor) sendModifiers() {
	s := comp.kbFocus
	if s == nil {
		return
	}
	for _, k := range comp.seatState.keyboards {
		if k.c != s.client {
			continue
		}
		k.c.event(k.id, 4, comp.nextSerial(), comp.seatState.mods, uint32(0), uint32(0), uint32(0))
		k.c.flush()
	}
}

// ---- pointer ----

// surfaceAt resolves a canvas pixel to the deepest surface under it and the
// surface-local coordinates, checking popups above subsurfaces above the
// toplevel.
func (comp *compositor) surfaceAt(w *window, x, y int) (*wlSurface, int, int) {
	if w == nil || w.top == nil || w.top.surf == nil || !w.contains(x, y) {
		return nil, 0, 0
	}
	root := w.top.surf
	lx, ly := x-w.area.x0, y-w.area.y0

	for i := len(comp.popups) - 1; i >= 0; i-- {
		p := comp.popups[i]
		if p.xdg == nil || p.xdg.surf == nil || p.xdg.surf.content == nil {
			continue
		}
		if comp.windowForSurface(p.xdg.surf) != w {
			continue
		}
		off := offsetOf(p.xdg.surf)
		ps := p.xdg.surf
		if lx >= off.x && lx < off.x+ps.w && ly >= off.y && ly < off.y+ps.h {
			return ps, lx - off.x, ly - off.y
		}
	}
	for i := len(root.children) - 1; i >= 0; i-- {
		sub := root.children[i]
		if sub.surf == nil || sub.surf.content == nil {
			continue
		}
		off := offsetOf(sub.surf)
		if lx >= off.x && lx < off.x+sub.surf.w && ly >= off.y && ly < off.y+sub.surf.h {
			return sub.surf, lx - off.x, ly - off.y
		}
	}
	return root, lx, ly
}

func (comp *compositor) setPointerFocus(s *wlSurface, lx, ly int) {
	if comp.pointerFocus == s {
		return
	}
	old := comp.pointerFocus
	comp.pointerFocus = s
	if old != nil {
		for _, p := range comp.seatState.pointers {
			if p.c == old.client {
				p.c.event(p.id, 1, comp.nextSerial(), old.id) // leave
				p.c.event(p.id, 5)                            // frame
				p.c.flush()
			}
		}
	}
	if s != nil {
		for _, p := range comp.seatState.pointers {
			if p.c == s.client {
				p.c.event(p.id, 0, comp.nextSerial(), s.id, fixed(float64(lx)), fixed(float64(ly)))
				p.c.event(p.id, 5)
				p.c.flush()
			}
		}
	}
}

func (comp *compositor) pointerMotion(x, y float64) {
	ix, iy := int(x), int(y)
	comp.seatState.lastPX, comp.seatState.lastPY = ix, iy
	// The launcher is modal: while it is up, the pointer belongs to it and
	// no guest sees motion under the panel.
	if lc := comp.lc; lc != nil && lc.open {
		lc.hover(ix, iy)
		return
	}
	w := comp.windowAt(ix, iy)
	s, lx, ly := comp.surfaceAt(w, ix, iy)
	comp.setPointerFocus(s, lx, ly)
	if s == nil {
		return
	}
	fx, fy := fixed(float64(lx)), fixed(float64(ly))
	comp.seatState.lastX, comp.seatState.lastY = fx, fy
	for _, p := range comp.seatState.pointers {
		if p.c != s.client {
			continue
		}
		p.c.event(p.id, 2, nowMs(), fx, fy) // motion
		p.c.event(p.id, 5)                  // frame
		p.c.flush()
	}
}

func (comp *compositor) pointerButton(btn uint32, pressed bool) {
	ix, iy := comp.seatState.lastPX, comp.seatState.lastPY
	if lc := comp.lc; lc != nil && lc.open {
		lc.click(btn, pressed, ix, iy)
		return
	}
	// Click to focus: a press anywhere in a tile takes the keyboard, which
	// is the behaviour a tiler needs when the pointer can wander over a
	// divider and sit on nothing.
	if pressed {
		if w := comp.windowAt(ix, iy); w != nil && w != comp.focus {
			comp.setFocus(w)
		}
	}
	s := comp.pointerFocus
	if s == nil {
		return
	}
	state := uint32(0)
	if pressed {
		state = 1
	}
	for _, p := range comp.seatState.pointers {
		if p.c != s.client {
			continue
		}
		p.c.event(p.id, 3, comp.nextSerial(), nowMs(), btn, state)
		p.c.event(p.id, 5)
		p.c.flush()
	}
}

func (comp *compositor) pointerAxis(vertical bool, value float64) {
	if lc := comp.lc; lc != nil && lc.open {
		if vertical {
			if value > 0 {
				lc.move(3)
			} else {
				lc.move(-3)
			}
		}
		return
	}
	s := comp.pointerFocus
	if s == nil {
		return
	}
	axis := uint32(0)
	if !vertical {
		axis = 1
	}
	for _, p := range comp.seatState.pointers {
		if p.c != s.client {
			continue
		}
		p.c.event(p.id, 4, nowMs(), axis, fixed(value))
		p.c.event(p.id, 5)
		p.c.flush()
	}
}
