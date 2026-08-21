package main

// xdg_wm_base / xdg_surface / xdg_toplevel / xdg_popup, plus wl_surface
// itself (attach/damage/frame/commit) since surface life is tied to xdg roles.

import (
	"encoding/binary"
	"runtime/debug"
	"time"
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
}

func (s *wlSurface) iface() string { return "wl_surface" }
func (s *wlSurface) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		if c.comp.toplevel != nil && c.comp.toplevel.surf == s {
			c.comp.toplevel = nil
			c.comp.focusSent = false
		}
		c.deleteID(id)
	case 1: // attach(buffer, x, y)
		bufID := r.uint()
		if bufID == 0 {
			s.pendingBuf = nil
		} else if b, ok := c.get(bufID).(*wlBuffer); ok {
			s.pendingBuf = b
			s.pendingBuf.id = bufID
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

	// First commit on an xdg surface with no buffer -> send initial configure.
	if s.role != nil && !s.role.configured {
		s.role.sendConfigure(c, comp)
		s.role.configured = true
	}

	if s.content != nil && (comp.toplevel == nil || comp.toplevel.surf == s || isChildOf(s, comp.toplevel.surf)) {
		dmg := s.pendingDamage
		if dmg.empty() {
			dmg = rect{0, 0, s.w, s.h}
		}
		off := offsetOf(s)
		dmg.x0 += off.x
		dmg.x1 += off.x
		dmg.y0 += off.y
		dmg.y1 += off.y
		comp.damage = comp.damage.union(dmg.clip(comp.widthPx, comp.heightPx))
		if !comp.dirty {
			comp.dirtyAt = time.Now()
		}
		comp.dirty = true
		if !comp.focusSent && comp.toplevel != nil && comp.toplevel.surf == s {
			comp.focusSent = true
			comp.keyboardEnter()
			comp.sendModifiers()
		}
		select {
		case comp.renderCh <- struct{}{}:
		default:
		}
	}
	s.pendingDamage = rect{}
}

type point struct{ x, y int }

func offsetOf(s *wlSurface) point {
	if s.sub != nil && s.sub.parent != nil {
		p := offsetOf(s.sub.parent)
		return point{p.x + s.sub.x, p.y + s.sub.y}
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
		c.objects[r.uint()] = xdgPositioner{}
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

type xdgPositioner struct{}

func (xdgPositioner) iface() string { return "xdg_positioner" }
func (xdgPositioner) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 0 {
		c.deleteID(id)
	}
}

// ---- xdg_surface ----

type xdgSurface struct {
	id         uint32
	surf       *wlSurface
	toplevel   *xdgToplevel
	configured bool
}

func (x *xdgSurface) iface() string { return "xdg_surface" }
func (x *xdgSurface) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		if x.surf != nil {
			x.surf.role = nil
		}
		c.deleteID(id)
	case 1: // get_toplevel(new_id)
		tid := r.uint()
		t := &xdgToplevel{id: tid, xdg: x, surf: x.surf, client: c}
		x.toplevel = t
		c.objects[tid] = t
		if c.comp.toplevel == nil {
			c.comp.toplevel = t
		}
	case 2: // get_popup(new_id, parent, positioner)
		pid := r.uint()
		c.objects[pid] = &xdgPopup{id: pid, xdg: x}
		logf("popup created (rendered nowhere yet)")
	case 3: // set_window_geometry
	case 4: // ack_configure(serial)
		r.uint()
	}
}

func (x *xdgSurface) sendConfigure(c *client, comp *compositor) {
	if x.toplevel != nil {
		// states: maximized(1) + activated(4)
		states := make([]byte, 8)
		binary.LittleEndian.PutUint32(states, 1)
		binary.LittleEndian.PutUint32(states[4:], 4)
		c.event(x.toplevel.id, 0, int32(comp.widthPx), int32(comp.heightPx), states)
	}
	c.event(x.id, 0, comp.nextSerial()) // xdg_surface.configure
}

// ---- xdg_toplevel ----

type xdgToplevel struct {
	id     uint32
	xdg    *xdgSurface
	surf   *wlSurface
	client *client
	title  string
}

func (t *xdgToplevel) iface() string { return "xdg_toplevel" }
func (t *xdgToplevel) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		if c.comp.toplevel == t {
			c.comp.toplevel = nil
			c.comp.focusSent = false
		}
		c.deleteID(id)
	case 2: // set_title
		t.title = r.string()
		logf("toplevel title: %q", t.title)
	case 3: // set_app_id
		logf("toplevel app_id: %q", r.string())
	}
	// move/resize/min/max/fullscreen requests ignored: pane is the world.
}

// resize tells the client the pane changed size.
func (t *xdgToplevel) resize(comp *compositor) {
	c := t.client
	states := make([]byte, 8)
	binary.LittleEndian.PutUint32(states, 1)
	binary.LittleEndian.PutUint32(states[4:], 4)
	c.event(t.id, 0, int32(comp.widthPx), int32(comp.heightPx), states)
	c.event(t.xdg.id, 0, comp.nextSerial())
	c.flush()
}

// ---- xdg_popup (accepted, never shown) ----

type xdgPopup struct {
	id  uint32
	xdg *xdgSurface
}

func (p *xdgPopup) iface() string { return "xdg_popup" }
func (p *xdgPopup) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 0 {
		c.deleteID(id)
	}
}
