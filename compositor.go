package main

// Core compositor state and the wl_* globals: display, registry, callback,
// compositor, surface, region, shm, shm_pool, buffer, output, subcompositor,
// and a stub data_device_manager.

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// maxPoolSize caps a single client pool mmap (matches tuios's shm cap).
const maxPoolSize = 512 * 1024 * 1024

var logFile *os.File

func logf(format string, args ...any) {
	if logFile != nil {
		fmt.Fprintf(logFile, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
	}
}

type compositor struct {
	mu sync.Mutex

	widthPx, heightPx int // pane size in pixels
	cellW, cellH      int

	serial    uint32
	clients   map[*client]bool
	seatState seatState

	// single is the default mode: one toplevel filling the pane, no chrome,
	// no prefix key, no launcher. The tiling fields below still exist (a
	// window is still how a toplevel is tracked) but the BSP tree, the
	// layout modes and every keybinding are inert.
	single bool

	// Tiling state. windows is creation order (master-stack reads it as
	// master-first); root is the BSP tree over the same set.
	windows     []*window
	root        *node
	focus       *window
	zoomed      *window
	mode        layoutMode
	masterRatio float64
	nextSplit   splitDir
	nextImgID   uint32
	region      cellRect
	popups      []*xdgPopup

	// Seat focus is explicit now that there is more than one surface.
	kbFocus      *wlSurface
	pointerFocus *wlSurface

	// Compositor keybindings: a tuios-style prefix, then a command key.
	prefixArmed bool
	prefixName  string
	prefixCode  uint32
	prefixMods  uint32
	swallow     map[uint32]bool
	spawnCmd    []string
	termCmd     []string // wrapper for Terminal=true desktop entries
	spawn       func(spawnReq)
	quitFn      func()

	// busSid is the session id of our private dbus-daemon. A service the
	// bus activates is a child of the bus, not of the process that asked
	// for it, so its windows carry the bus's session. Both count as ours.
	busSid int

	// lc is the application launcher: an overlay on the chrome layer with
	// its own image, never a Wayland client.
	lc         *launcher
	freeImages []uint32 // image ids whose placements must be deleted

	// Two levels of chrome repaint. A layout change repaints the whole
	// canvas and costs every tile a recomposite; a focus or title change
	// only touches the rules, which live outside every content rectangle,
	// so guest pixels and guest transmissions are left alone.
	chromeLayout bool
	chromeDirty  bool
	pendingCB    []frameCB

	// frame is the composited output canvas (RGBA).
	frame       []byte
	frameW      int
	frameH      int
	damage      rect
	dirty       bool
	dirtyAt     time.Time
	renderCh    chan struct{}
	keymapOnce  sync.Once
	commitCount uint64
	commitBytes uint64
}

// frameCB is a wl_callback owed to a client once the frame it committed
// into has actually been written to the terminal.
type frameCB struct {
	c  *client
	id uint32
}

type rect struct{ x0, y0, x1, y1 int }

func (r rect) empty() bool { return r.x1 <= r.x0 || r.y1 <= r.y0 }
func (r rect) union(o rect) rect {
	if r.empty() {
		return o
	}
	if o.empty() {
		return r
	}
	if o.x0 < r.x0 {
		r.x0 = o.x0
	}
	if o.y0 < r.y0 {
		r.y0 = o.y0
	}
	if o.x1 > r.x1 {
		r.x1 = o.x1
	}
	if o.y1 > r.y1 {
		r.y1 = o.y1
	}
	return r
}
func (r rect) clip(w, h int) rect {
	if r.x0 < 0 {
		r.x0 = 0
	}
	if r.y0 < 0 {
		r.y0 = 0
	}
	if r.x1 > w {
		r.x1 = w
	}
	if r.y1 > h {
		r.y1 = h
	}
	return r
}

func (comp *compositor) nextSerial() uint32 {
	comp.serial++
	return comp.serial
}

func (comp *compositor) markDirty() {
	if !comp.dirty {
		comp.dirtyAt = time.Now()
	}
	comp.dirty = true
	select {
	case comp.renderCh <- struct{}{}:
	default:
	}
}

// clientGone drops every window a disconnected client owned. One client can
// own several toplevels, and several clients can be connected at once, so
// this is a sweep rather than a single check.
// releaser is implemented by objects that own a kernel resource -- an fd, a
// mapping -- which the object's own destroy request would normally free. A
// client that just disconnects never sends those requests, so the compositor
// has to sweep on its way out. Without this, dmaevil -case fdstorm leaks 2000
// fds per connection and walks the process into EMFILE.
type releaser interface{ releaseResources() }

func (comp *compositor) clientGone(c *client) {
	swept := 0
	for id, obj := range c.objects {
		if r, ok := obj.(releaser); ok {
			r.releaseResources()
			swept++
		}
		delete(c.objects, id)
	}
	if swept > 0 {
		logf("client gone: released %d resource-owning objects", swept)
	}
	delete(comp.clients, c)
	comp.seatState.dropClient(c)
	var doomed []*window
	for _, w := range comp.windows {
		if w.top != nil && w.top.client == c {
			doomed = append(doomed, w)
		}
	}
	for _, w := range doomed {
		comp.removeWindow(w)
	}
	live := comp.popups[:0]
	for _, p := range comp.popups {
		if p.xdg == nil || p.xdg.surf == nil || p.xdg.surf.client != c {
			live = append(live, p)
		}
	}
	comp.popups = live
	if comp.pointerFocus != nil && comp.pointerFocus.client == c {
		comp.pointerFocus = nil
	}
	if comp.kbFocus != nil && comp.kbFocus.client == c {
		comp.kbFocus = nil
	}
	logf("client gone: %d windows left", len(comp.windows))
}

// ---- wl_display (object 1) ----

type wlDisplay struct{}

func (wlDisplay) iface() string { return "wl_display" }
func (wlDisplay) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // sync(callback new_id)
		cb := r.uint()
		c.event(cb, 0, c.comp.nextSerial()) // wl_callback.done
		c.deleteID(cb)
	case 1: // get_registry(new_id)
		reg := r.uint()
		c.objects[reg] = wlRegistry{}
		sendGlobals(c, reg)
	}
}

// Global registry. name -> (interface, version)
type globalDef struct {
	name    uint32
	iface   string
	version uint32
}

var globalList = []globalDef{
	{1, "wl_compositor", 4},
	{2, "wl_shm", 1},
	{3, "wl_seat", 5},
	{4, "wl_output", 3},
	{5, "xdg_wm_base", 2},
	{6, "wl_subcompositor", 1},
	{7, "wl_data_device_manager", 3},
	{8, "zxdg_decoration_manager_v1", 1},
	{9, "zwp_linux_dmabuf_v1", 4},
}

func sendGlobals(c *client, reg uint32) {
	for _, g := range globalList {
		if g.iface == "zwp_linux_dmabuf_v1" && !dmabufReady {
			continue // no usable DRM node: do not promise what we cannot map
		}
		c.event(reg, 0, g.name, g.iface, g.version) // wl_registry.global
	}
}

type wlRegistry struct{}

func (wlRegistry) iface() string { return "wl_registry" }
func (wlRegistry) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode != 0 { // bind(name, interface, version, new_id)
		return
	}
	name := r.uint()
	iface := r.string()
	version := r.uint()
	newID := r.uint()
	logf("bind %s v%d -> id %d", iface, version, newID)
	_ = name
	switch iface {
	case "wl_compositor":
		c.objects[newID] = wlCompositor{}
	case "wl_shm":
		c.objects[newID] = wlShm{}
		c.event(newID, 0, uint32(0)) // format argb8888
		c.event(newID, 0, uint32(1)) // format xrgb8888
	case "wl_seat":
		c.objects[newID] = &wlSeat{version: version}
		c.event(newID, 0, uint32(3)) // capabilities: pointer|keyboard
		if version >= 2 {
			c.event(newID, 1, "seat0") // name
		}
	case "wl_output":
		c.objects[newID] = wlOutput{}
		comp := c.comp
		// geometry: x,y,phys_w,phys_h,subpixel,make,model,transform
		c.event(newID, 0, int32(0), int32(0), int32(comp.widthPx*254/9600), int32(comp.heightPx*254/9600), int32(0), "wlterm", "pane", int32(0))
		// mode: flags(current|preferred), w, h, refresh mHz
		c.event(newID, 1, uint32(3), int32(comp.widthPx), int32(comp.heightPx), int32(60000))
		if version >= 2 {
			c.event(newID, 3, int32(1)) // scale 1
			c.event(newID, 2)           // done
		}
	case "xdg_wm_base":
		c.objects[newID] = &xdgWmBase{}
	case "wl_subcompositor":
		c.objects[newID] = wlSubcompositor{}
	case "wl_data_device_manager":
		c.objects[newID] = wlDataDeviceManager{}
	case "zxdg_decoration_manager_v1":
		c.objects[newID] = xdgDecorationManager{}
	case "zwp_linux_dmabuf_v1":
		if !dmabufReady {
			c.protoError(id, 0, "dmabuf unavailable")
			return
		}
		if version > 4 {
			version = 4
		}
		c.objects[newID] = zwpLinuxDmabuf{version: version}
		if version < 4 {
			sendLegacyFormats(c, newID, version)
		}
	default:
		logf("bind of unknown global %q", iface)
		c.protoError(id, 0, "unknown global "+iface)
	}
}

// ---- wl_compositor ----

type wlCompositor struct{}

func (wlCompositor) iface() string { return "wl_compositor" }
func (wlCompositor) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // create_surface
		sid := r.uint()
		c.objects[sid] = &wlSurface{id: sid, client: c}
	case 1: // create_region
		rid := r.uint()
		c.objects[rid] = &wlRegion{}
	}
}

type wlRegion struct{}

func (wlRegion) iface() string { return "wl_region" }
func (wlRegion) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 0 { // destroy
		c.deleteID(id)
	} // add/subtract ignored: single fullscreen surface, input everywhere
}

// ---- wl_shm ----

type wlShm struct{}

func (wlShm) iface() string { return "wl_shm" }
func (wlShm) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // create_pool(new_id, fd, size)
		pid := r.uint()
		fd := r.fd()
		size := r.int()
		// A pool whose backing file is smaller than its declared size is a
		// SIGBUS grenade (this exact hole crashes Hyprland). fstat and refuse.
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil || size <= 0 || int64(size) > st.Size || int64(size) > maxPoolSize {
			logf("refusing pool: declared %d, file %d", size, st.Size)
			c.protoError(id, 1, "invalid pool size")
			syscall.Close(fd)
			return
		}
		data, err := syscall.Mmap(fd, 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			logf("mmap pool failed: %v", err)
			c.protoError(id, 2, "mmap failed")
			syscall.Close(fd)
			return
		}
		c.objects[pid] = &shmPool{fd: fd, data: data}
	case 1: // release (v2)
		c.deleteID(id)
	}
}

type shmPool struct {
	fd   int
	data []byte
	refs int // live buffers
	dead bool
}

func (p *shmPool) iface() string { return "wl_shm_pool" }
func (p *shmPool) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // create_buffer(new_id, offset, w, h, stride, format)
		bid := r.uint()
		off := r.int()
		w := r.int()
		h := r.int()
		stride := r.int()
		format := r.uint()
		if off < 0 || w <= 0 || h <= 0 || stride < w*4 ||
			int64(off)+int64(stride)*int64(h) > int64(len(p.data)) {
			logf("refusing buffer: off=%d %dx%d stride=%d pool=%d", off, w, h, stride, len(p.data))
			c.protoError(id, 1, "buffer exceeds pool")
			return
		}
		c.objects[bid] = &wlBuffer{pool: p, off: int(off), w: int(w), h: int(h), stride: int(stride), opaque: format == 1}
		p.refs++
	case 1: // destroy
		p.dead = true
		p.maybeFree()
		c.deleteID(id)
	case 2: // resize(size)
		size := r.int()
		var st syscall.Stat_t
		if err := syscall.Fstat(p.fd, &st); err != nil || size <= 0 || int64(size) > st.Size || int64(size) > maxPoolSize {
			logf("refusing pool resize: declared %d, file %d", size, st.Size)
			c.protoError(id, 1, "invalid pool size")
			return
		}
		syscall.Munmap(p.data)
		data, err := syscall.Mmap(p.fd, 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			logf("mmap pool resize failed: %v", err)
			c.protoError(id, 2, "mmap failed")
			return
		}
		p.data = data
	}
}

func (p *shmPool) releaseResources() {
	p.dead = true
	p.refs = 0
	p.maybeFree()
}

func (p *shmPool) maybeFree() {
	if p.dead && p.refs == 0 {
		syscall.Munmap(p.data)
		syscall.Close(p.fd)
		p.data = nil
	}
}

// wlBuffer is a client buffer wlterm can read with the CPU. It is backed
// either by an shm pool or by a mmap'd LINEAR dmabuf; both are just bytes.
type wlBuffer struct {
	pool              *shmPool   // shm backing, nil for dmabuf
	dma               *dmaBuffer // dmabuf backing, nil for shm
	id                uint32
	off, w, h, stride int
	opaque            bool // xrgb8888: the alpha channel is meaningless
	yflip             bool // dmabuf y_invert flag
}

// pixels returns the mapped bytes. The caller must be inside a
// SetPanicOnFault guard: a client can shrink either backing at any time.
func (b *wlBuffer) pixels() []byte {
	if b.dma != nil {
		return b.dma.data
	}
	if b.pool != nil {
		return b.pool.data
	}
	return nil
}

func (b *wlBuffer) releaseResources() {
	if b.dma != nil {
		b.dma.release()
		b.dma = nil
	}
}

func (b *wlBuffer) iface() string { return "wl_buffer" }
func (b *wlBuffer) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 0 { // destroy
		if b.dma != nil {
			b.dma.release()
			b.dma = nil
		}
		if b.pool != nil {
			b.pool.refs--
			b.pool.maybeFree()
		}
		c.deleteID(id)
	}
}

// ---- wl_output ----

type wlOutput struct{}

func (wlOutput) iface() string { return "wl_output" }
func (wlOutput) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 0 { // release
		c.deleteID(id)
	}
}

// ---- wl_subcompositor / wl_subsurface ----

type wlSubcompositor struct{}

func (wlSubcompositor) iface() string { return "wl_subcompositor" }
func (wlSubcompositor) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0:
		c.deleteID(id)
	case 1: // get_subsurface(new_id, surface, parent)
		sid := r.uint()
		surfID := r.uint()
		parentID := r.uint()
		sub := &wlSubsurface{}
		if s, ok := c.get(surfID).(*wlSurface); ok {
			sub.surf = s
			s.sub = sub
		}
		if p, ok := c.get(parentID).(*wlSurface); ok {
			sub.parent = p
			p.children = append(p.children, sub)
		}
		c.objects[sid] = sub
	}
}

type wlSubsurface struct {
	surf   *wlSurface
	parent *wlSurface
	x, y   int
}

func (s *wlSubsurface) iface() string { return "wl_subsurface" }
func (s *wlSubsurface) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		if s.parent != nil {
			for i, ch := range s.parent.children {
				if ch == s {
					s.parent.children = append(s.parent.children[:i], s.parent.children[i+1:]...)
					break
				}
			}
		}
		if s.surf != nil {
			s.surf.sub = nil
		}
		c.deleteID(id)
	case 1: // set_position(x, y)
		s.x = int(r.int())
		s.y = int(r.int())
	}
	// place_above/below/set_sync/set_desync ignored
}

// ---- wl_data_device_manager stubs (clipboard black hole) ----

type wlDataDeviceManager struct{}

func (wlDataDeviceManager) iface() string { return "wl_data_device_manager" }
func (wlDataDeviceManager) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // create_data_source
		c.objects[r.uint()] = wlDataSource{}
	case 1: // get_data_device
		c.objects[r.uint()] = wlDataDevice{}
	}
}

type wlDataSource struct{}

func (wlDataSource) iface() string { return "wl_data_source" }
func (wlDataSource) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 1 { // destroy
		c.deleteID(id)
	}
}

type wlDataDevice struct{}

func (wlDataDevice) iface() string { return "wl_data_device" }
func (wlDataDevice) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 2 { // release
		c.deleteID(id)
	}
}

// ---- zxdg_decoration_manager_v1 ----

type xdgDecorationManager struct{}

func (xdgDecorationManager) iface() string { return "zxdg_decoration_manager_v1" }
func (xdgDecorationManager) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0:
		c.deleteID(id)
	case 1: // get_toplevel_decoration(new_id, toplevel)
		did := r.uint()
		c.objects[did] = xdgToplevelDecoration{}
		c.event(did, 0, uint32(2)) // configure: server-side
	}
}

type xdgToplevelDecoration struct{}

func (xdgToplevelDecoration) iface() string { return "zxdg_toplevel_decoration_v1" }
func (xdgToplevelDecoration) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0:
		c.deleteID(id)
	case 1: // set_mode(mode) -> insist on server-side
		c.event(id, 0, uint32(2))
	}
}
