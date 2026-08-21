package main

// xdg_wm_base / xdg_surface / xdg_toplevel / xdg_popup, plus wl_surface
// itself (attach/damage/frame/commit) since surface life is tied to xdg roles.

import (
	"encoding/binary"
	"runtime/debug"
)

// ---- wl_surface ----

type wlSurface struct {
	id     uint32
	client *client

	// pending state (set between commits)
	pendingBuf     *wlBuffer
	pendingBufSet  bool
	pendingDamage  rect
	frameCallbacks []uint32

	// committed content, copied out of the shm pool
	content []byte // BGRA rows, w*4 stride
	w, h    int

	role     *xdgSurface
	sub      *wlSubsurface
	children []*wlSubsurface
	mapped   bool
}

func (s *wlSurface) iface() string { return "wl_surface" }
func (s *wlSurface) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		if s.role != nil && s.role.toplevel != nil && s.role.toplevel.win != nil {
			c.comp.removeWindow(s.role.toplevel.win)
		}
		c.comp.surfaceGone(s)
		c.deleteID(id)
	case 1: // attach(buffer, x, y)
		bufID := r.uint()
		if bufID == 0 {
			s.pendingBuf = nil
		} else if b, ok := c.get(bufID).(*wlBuffer); ok {
			s.pendingBuf = b
			s.pendingBuf.id = bufID
		} else {
			s.pendingBuf = nil
		}
		s.pendingBufSet = true
	case 2: // damage(x, y, w, h) - surface coords; buffer scale 1 so same
		x := int(r.int())
		y := int(r.int())
		w := int(r.int())
		h := int(r.int())
		s.pendingDamage = s.pendingDamage.union(rect{x, y, x + w, y + h})
	case 3: // frame(new_id callback)
		s.frameCallbacks = append(s.frameCallbacks, r.uint())
	case 6: // commit
		s.commit(c)
	case 9: // damage_buffer(x, y, w, h)
		x := int(r.int())
		y := int(r.int())
		w := int(r.int())
		h := int(r.int())
		s.pendingDamage = s.pendingDamage.union(rect{x, y, x + w, y + h})
	}
	// set_opaque_region(4), set_input_region(5), transform(7), scale(8),
	// offset(10): ignored.
}

func (s *wlSurface) commit(c *client) {
	comp := c.comp
	if s.pendingBufSet {
		if s.pendingBuf != nil {
			b := s.pendingBuf
			s.copyFromBuffer(b)
			// Release immediately: we copied the pixels.
			c.event(b.id, 0) // wl_buffer.release
			comp.commitCount++
			comp.commitBytes += uint64(b.w * b.h * 4)
		} else {
			s.content = nil
			s.w, s.h = 0, 0
		}
		s.pendingBuf = nil
		s.pendingBufSet = false
	}

	// First commit on an xdg surface: the client is asking to be told how
	// big it may be. With several tiles on screen this is where each client
	// learns its own rectangle rather than the whole pane.
	if s.role != nil && !s.role.configured {
		s.role.sendConfigure(c, comp)
		s.role.configured = true
	}

	// Frame callbacks are owed once this frame reaches the terminal, which
	// is what paces a client against the pty rather than against a clock.
	if len(s.frameCallbacks) > 0 {
		for _, id := range s.frameCallbacks {
			comp.pendingCB = append(comp.pendingCB, frameCB{c, id})
		}
		s.frameCallbacks = nil
		comp.markDirty()
	}

	w := comp.windowForSurface(s)
	if w == nil || w.area.empty() {
		s.pendingDamage = rect{}
		return
	}
	if s.content != nil {
		if !s.mapped {
			s.mapped = true
			if comp.focus == w {
				comp.setKeyboardFocus(s)
			}
		}
		dmg := s.pendingDamage
		if dmg.empty() {
			dmg = rect{0, 0, s.w, s.h}
		}
		off := offsetOf(s)
		dmg.x0 += off.x + w.area.x0
		dmg.x1 += off.x + w.area.x0
		dmg.y0 += off.y + w.area.y0
		dmg.y1 += off.y + w.area.y0
		dmg = dmg.clip(comp.widthPx, comp.heightPx)
		if dmg.x0 < w.area.x0 {
			dmg.x0 = w.area.x0
		}
		if dmg.y0 < w.area.y0 {
			dmg.y0 = w.area.y0
		}
		if dmg.x1 > w.area.x1 {
			dmg.x1 = w.area.x1
		}
		if dmg.y1 > w.area.y1 {
			dmg.y1 = w.area.y1
		}
		if !dmg.empty() {
			w.dmg = w.dmg.union(dmg)
			comp.markDirty()
		}
	}
	s.pendingDamage = rect{}
}

// windowForSurface walks up subsurface parents until it reaches a surface
// with a toplevel role, and returns that toplevel's tile.
func (comp *compositor) windowForSurface(s *wlSurface) *window {
	cur := s
	for i := 0; i < 16; i++ {
		if cur.role != nil && cur.role.toplevel != nil {
			return cur.role.toplevel.win
		}
		if cur.role != nil && cur.role.popup != nil && cur.role.popup.parent != nil {
			cur = cur.role.popup.parent
			continue
		}
		if cur.sub != nil && cur.sub.parent != nil {
			cur = cur.sub.parent
			continue
		}
		return nil
	}
	return nil
}

// surfaceGone clears any seat focus pointing at a destroyed surface.
func (comp *compositor) surfaceGone(s *wlSurface) {
	if comp.pointerFocus == s {
		comp.pointerFocus = nil
	}
	if comp.kbFocus == s {
		comp.kbFocus = nil
	}
	for i, p := range comp.popups {
		if p.xdg != nil && p.xdg.surf == s {
			comp.popups = append(comp.popups[:i], comp.popups[i+1:]...)
			comp.chromeDirty = true
			comp.markDirty()
			break
		}
	}
}

type point struct{ x, y int }

// offsetOf is a surface's position relative to its toplevel's content
// origin, accumulated through subsurface and popup parents.
func offsetOf(s *wlSurface) point {
	if s.sub != nil && s.sub.parent != nil {
		p := offsetOf(s.sub.parent)
		return point{p.x + s.sub.x, p.y + s.sub.y}
	}
	if s.role != nil && s.role.popup != nil && s.role.popup.parent != nil {
		p := offsetOf(s.role.popup.parent)
		return point{p.x + s.role.popup.x, p.y + s.role.popup.y}
	}
	return point{}
}

func isChildOf(s, top *wlSurface) bool {
	if top == nil {
		return false
	}
	cur := s
	for cur.sub != nil && cur.sub.parent != nil {
		cur = cur.sub.parent
		if cur == top {
			return true
		}
	}
	return false
}

// copyFromBuffer copies the client's shm pixels into surface-owned memory,
// converting stride to tight w*4 rows. Keeps BGRA byte order (swizzled later).
// A client can truncate the pool file after validation; SetPanicOnFault turns
// the resulting SIGBUS into a recoverable panic instead of killing us.
func (s *wlSurface) copyFromBuffer(b *wlBuffer) {
	old := debug.SetPanicOnFault(true)
	defer func() {
		debug.SetPanicOnFault(old)
		if err := recover(); err != nil {
			logf("fault copying client buffer (truncated pool?): %v", err)
			s.content = nil
			s.w, s.h = 0, 0
		}
	}()
	need := b.w * b.h * 4
	if cap(s.content) < need {
		s.content = make([]byte, need)
	}
	s.content = s.content[:need]
	s.w, s.h = b.w, b.h
	src := b.pool.data
	for y := 0; y < b.h; y++ {
		so := b.off + y*b.stride
		if so+b.w*4 > len(src) {
			break
		}
		copy(s.content[y*b.w*4:(y+1)*b.w*4], src[so:so+b.w*4])
	}
	if b.format == 1 { // xrgb8888: force alpha opaque
		px := s.content
		for i := 3; i < len(px); i += 4 {
			px[i] = 0xff
		}
	}
}

// ---- xdg_wm_base ----

type xdgWmBase struct{}

func (x *xdgWmBase) iface() string { return "xdg_wm_base" }
func (x *xdgWmBase) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		c.deleteID(id)
	case 1: // create_positioner
		c.objects[r.uint()] = &xdgPositioner{}
	case 2: // get_xdg_surface(new_id, surface)
		xid := r.uint()
		surfID := r.uint()
		xs := &xdgSurface{id: xid}
		if s, ok := c.get(surfID).(*wlSurface); ok {
			xs.surf = s
			s.role = xs
		}
		c.objects[xid] = xs
	case 3: // pong
		r.uint()
	}
}

// xdgPositioner records just enough of the placement request to put a menu
// roughly where the client asked. No constraint solving.
type xdgPositioner struct {
	w, h       int
	ax, ay     int
	offX, offY int
}

func (p *xdgPositioner) iface() string { return "xdg_positioner" }
func (p *xdgPositioner) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		c.deleteID(id)
	case 1: // set_size(w, h)
		p.w, p.h = int(r.int()), int(r.int())
	case 2: // set_anchor_rect(x, y, w, h)
		p.ax, p.ay = int(r.int()), int(r.int())
		aw, ah := int(r.int()), int(r.int())
		_ = aw
		p.ay += ah // anchor below the rect: the common menu case
	case 5: // set_offset(x, y)
		p.offX, p.offY = int(r.int()), int(r.int())
	}
}

// ---- xdg_surface ----

type xdgSurface struct {
	id         uint32
	surf       *wlSurface
	toplevel   *xdgToplevel
	popup      *xdgPopup
	configured bool
}

func (x *xdgSurface) iface() string { return "xdg_surface" }
func (x *xdgSurface) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		if x.popup != nil {
			c.comp.dropPopup(x.popup)
		}
		if x.surf != nil {
			x.surf.role = nil
		}
		c.deleteID(id)
	case 1: // get_toplevel(new_id)
		tid := r.uint()
		t := &xdgToplevel{id: tid, xdg: x, surf: x.surf, client: c}
		x.toplevel = t
		c.objects[tid] = t
		c.comp.addWindow(t)
	case 2: // get_popup(new_id, parent, positioner)
		pid := r.uint()
		parentID := r.uint()
		posID := r.uint()
		p := &xdgPopup{id: pid, xdg: x}
		if ps, ok := c.get(parentID).(*xdgSurface); ok {
			p.parent = ps.surf
		}
		if pos, ok := c.get(posID).(*xdgPositioner); ok {
			p.w, p.h = pos.w, pos.h
			// Minimal placement: anchor rect origin plus the requested
			// offset. No constraint adjustment; a menu near an edge may
			// overhang and get clipped to the tile.
			p.x = pos.ax + pos.offX
			p.y = pos.ay + pos.offY
			if p.w <= 0 {
				p.w, p.h = 1, 1
			}
		}
		x.popup = p
		c.objects[pid] = p
		c.comp.popups = append(c.comp.popups, p)
	case 3: // set_window_geometry
	case 4: // ack_configure(serial)
		r.uint()
	}
}

// sendConfigure tells one client the size of its own tile. This is the
// difference between "every app thinks it owns the screen" and a tiler:
// each toplevel gets its rectangle, and re-gets it whenever the tiling
// changes underneath it.
func (x *xdgSurface) sendConfigure(c *client, comp *compositor) {
	if t := x.toplevel; t != nil {
		w, h := comp.widthPx, comp.heightPx
		if t.win != nil && !t.win.area.empty() {
			w = t.win.area.x1 - t.win.area.x0
			h = t.win.area.y1 - t.win.area.y0
		}
		c.event(t.id, 0, int32(w), int32(h), toplevelStates(comp, t))
		if t.win != nil {
			t.win.sentW, t.win.sentH = w, h
			t.win.sentFocus = comp.focus == t.win
		}
	}
	if p := x.popup; p != nil {
		c.event(p.id, 0, int32(p.x), int32(p.y), int32(p.w), int32(p.h))
	}
	c.event(x.id, 0, comp.nextSerial()) // xdg_surface.configure
}

// toplevelStates: maximized so clients drop their own decorations, plus
// activated only on the focused tile, plus the tiled_* states so clients
// that understand them square off their corners.
func toplevelStates(comp *compositor, t *xdgToplevel) []byte {
	states := []uint32{1} // maximized
	if t.win != nil && comp.focus == t.win {
		states = append(states, 4) // activated
	}
	states = append(states, 5, 6, 7, 8) // tiled left/right/top/bottom
	buf := make([]byte, 4*len(states))
	for i, v := range states {
		binary.LittleEndian.PutUint32(buf[i*4:], v)
	}
	return buf
}

// configureAll re-sends a configure to every tile whose rectangle or focus
// state changed. Called after any relayout.
func (comp *compositor) configureAll() {
	touched := map[*client]bool{}
	for _, w := range comp.windows {
		t := w.top
		if t == nil || t.xdg == nil || !t.xdg.configured {
			continue
		}
		nw, nh := 0, 0
		if !w.area.empty() {
			nw, nh = w.area.x1-w.area.x0, w.area.y1-w.area.y0
		}
		focused := comp.focus == w
		if nw == w.sentW && nh == w.sentH && focused == w.sentFocus {
			continue
		}
		if nw == 0 || nh == 0 {
			continue
		}
		w.sentW, w.sentH, w.sentFocus = nw, nh, focused
		t.client.event(t.id, 0, int32(nw), int32(nh), toplevelStates(comp, t))
		t.client.event(t.xdg.id, 0, comp.nextSerial())
		touched[t.client] = true
	}
	for c := range touched {
		c.flush()
	}
}

// ---- xdg_toplevel ----

type xdgToplevel struct {
	id     uint32
	xdg    *xdgSurface
	surf   *wlSurface
	client *client
	win    *window
	title  string
	appID  string
}

func (t *xdgToplevel) iface() string { return "xdg_toplevel" }
func (t *xdgToplevel) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		c.comp.removeWindow(t.win)
		c.deleteID(id)
	case 2: // set_title
		t.title = r.string()
		if t.win != nil {
			c.comp.chromeDirty = true
			c.comp.markDirty()
		}
	case 3: // set_app_id
		t.appID = r.string()
		if t.win != nil && t.title == "" {
			c.comp.chromeDirty = true
			c.comp.markDirty()
		}
	case 11: // set_fullscreen -> the tile is the world; zoom instead
		if t.win != nil {
			c.comp.zoomed = t.win
			c.comp.relayout()
		}
	case 12: // unset_fullscreen
		if c.comp.zoomed == t.win {
			c.comp.zoomed = nil
			c.comp.relayout()
		}
	}
	// move/resize/minimise requests ignored: the tiler owns geometry.
}

// close asks the client to shut this toplevel down, the polite way a tiling
// WM closes a window.
func (t *xdgToplevel) close() {
	t.client.event(t.id, 1) // xdg_toplevel.close
	t.client.flush()
}

// ---- xdg_popup ----

type xdgPopup struct {
	id      uint32
	xdg     *xdgSurface
	parent  *wlSurface
	x, y    int
	w, h    int
	grabbed bool
}

func (p *xdgPopup) iface() string { return "xdg_popup" }
func (p *xdgPopup) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		c.comp.dropPopup(p)
		c.deleteID(id)
	case 1: // grab(seat, serial)
		p.grabbed = true
	}
}

func (comp *compositor) dropPopup(p *xdgPopup) {
	for i, x := range comp.popups {
		if x == p {
			comp.popups = append(comp.popups[:i], comp.popups[i+1:]...)
			comp.chromeDirty = true
			comp.markDirty()
			return
		}
	}
}
