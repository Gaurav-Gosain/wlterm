package main

// Compositing and output.
//
// Two independent axes:
//
//	transport (-mode):  b64 | shm | delta
//	  b64    base64 payload in the escape itself; works anywhere, costs most
//	  shm    pixels through /dev/shm, ~70 bytes down the pty
//	  delta  shm plus kitty animation-frame edits, so only damage moves
//
//	granularity (-layers): single | per-window
//	  single      one canvas image, retransmitted as a whole
//	  per-window  the chrome is one image and every tile is its own image
//	              placed at its own cell, so a tile that animates costs its
//	              own rectangle and nothing else
//
// per-window is the point of the multi-surface version: with N tiles the
// single-canvas path pays for all N every time any one of them moves a
// pixel, which is the waste already measured on the tuios side.

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type renderStats struct {
	frames      atomic.Uint64
	compositeNs atomic.Uint64
	encodeNs    atomic.Uint64
	writeNs     atomic.Uint64
	paceNs      atomic.Uint64
	idleNs      atomic.Uint64
	emptyWakes  atomic.Uint64
	latencyNs   atomic.Uint64
	ptyBytes    atomic.Uint64
	shmBytes    atomic.Uint64
	dmgPixels   atomic.Uint64
	images      atomic.Uint64
	overlayPx   atomic.Uint64
	overlayOps  atomic.Uint64
	chromeNs    atomic.Uint64
	chromeOps   atomic.Uint64
	shmCopies   atomic.Uint64
	shmCopyNs   atomic.Uint64
	dmaCopies   atomic.Uint64
	dmaCopyNs   atomic.Uint64
	dmaSyncNs   atomic.Uint64
	// Pointer motion: escapes read off stdin, and events actually
	// delivered to a client. The gap between them is what coalescing buys.
	motionIn  atomic.Uint64
	motionOut atomic.Uint64
	// sameFrames counts commits whose pixels matched the frame already on
	// screen, and so cost nothing past the compare.
	sameFrames atomic.Uint64
}

var stats renderStats

type renderer struct {
	comp    *compositor
	mode    string // delta | shm | b64
	layered bool
	out     *os.File
	maxFPS  int
	stamp   *os.File

	rootSent  bool
	rootDirty bool
	// patches counts frame edits sent for each image since it was last
	// transmitted whole. See resyncAfterPatches.
	patches  map[uint32]int
	blendLC  *launcher // single-canvas mode: overlay blended at emit time
	layerSrc *launcher // per-window mode: the image being emitted is a layer
	rootDmg  rect
	shmSeq   int
	sub      []byte
	b64buf   []byte
	emitBuf  []byte

	// frameFn is what loop() paces. Only a test replaces it, so that the
	// pacing can be checked against a frame of known cost without a
	// compositor, a client or a terminal.
	frameFn func()
	// now and sleep are the clock loop() paces against. A test replaces
	// them with a fake one, so the rate it checks does not depend on how
	// busy the machine running it is.
	now   func() time.Time
	sleep func(time.Duration)
}

func newRenderer(comp *compositor, mode string, layered bool, out *os.File, maxFPS int) *renderer {
	r := &renderer{comp: comp, mode: mode, layered: layered, out: out, maxFPS: maxFPS,
		patches: map[uint32]int{}}
	r.frameFn = r.frame
	r.now, r.sleep = time.Now, time.Sleep
	return r
}

// loop paces frames from the start of one to the start of the next, so
// -fps is a rate rather than a gap between frames.
//
// It used to sleep minInterval measured from the *end* of the previous frame,
// which adds a whole frame's cost to every period: with a 5ms frame, -fps 60
// produced 21.7ms periods, i.e. 46fps. The error is invisible in a benchmark
// run at -fps 1000, where minInterval is 1ms and the frame cost dominates
// anyway, and that is exactly where wlterm's fast numbers were measured.
func (r *renderer) loop() {
	// -fps 0 is uncapped, and is also what keeps a zero from dividing.
	var minInterval time.Duration
	if r.maxFPS > 0 {
		minInterval = time.Second / time.Duration(r.maxFPS)
	}
	var next time.Time
	var prevEnd time.Time
	for range r.comp.renderCh {
		recv := r.now()
		// A wake with nothing to draw must not consume a slot of the frame
		// budget. Two markDirty calls straddling a frame leave a token behind
		// for a frame that has already been drawn, and paying a full interval
		// for that token is a cap coming out below what was asked for.
		r.comp.mu.Lock()
		dirty := r.comp.dirty
		r.comp.mu.Unlock()
		if !dirty {
			stats.emptyWakes.Add(1)
			continue
		}
		if !prevEnd.IsZero() {
			stats.idleNs.Add(uint64(recv.Sub(prevEnd)))
		}
		if d := next.Sub(recv); d > 0 {
			r.sleep(d)
			stats.paceNs.Add(uint64(d))
		}
		start := r.now()
		// Advance the deadline on its own grid, so a Sleep that overshoots by
		// the scheduler's granularity is absorbed by the next slot instead of
		// accumulating as lost rate. Resync when we are more than a slot late,
		// so a long stall is never repaid as a burst of back-to-back frames.
		next = next.Add(minInterval)
		if next.Before(start) {
			next = start.Add(minInterval)
		}
		r.frameFn()
		prevEnd = r.now()
	}
}

// emitItem is one image transmission planned under the lock and written
// outside it.
type emitItem struct {
	imgID    uint32
	area     rect      // canvas region the image covers
	dmg      damageSet // the regions that actually changed
	col, row int       // cell position of the placement
	z        int
	full     bool
}

func (r *renderer) frame() {
	comp := r.comp
	comp.mu.Lock()
	if !comp.dirty {
		comp.mu.Unlock()
		return
	}
	comp.dirty = false
	dirtyAt := comp.dirtyAt

	t0 := time.Now()
	if r.prepare() {
		comp.chromeLayout = true
	}
	if comp.chromeLayout || comp.chromeDirty {
		full := comp.chromeLayout
		comp.chromeLayout, comp.chromeDirty = false, false
		tc := time.Now()
		r.rootDmg = r.rootDmg.union(r.drawBackdrop(full))
		stats.chromeNs.Add(uint64(time.Since(tc)))
		stats.chromeOps.Add(1)
		// In single-app mode with per-window layers the root image is
		// entirely covered by the one tile at z=1, so it is composited
		// (the tile is extracted from this canvas) but never transmitted.
		r.rootDirty = !(comp.single && r.layered)
		if full {
			// The ground was repainted under every tile, so every tile owes
			// a recomposite. This is the expensive path, and it only runs
			// when the tiling itself changed.
			for _, w := range comp.windows {
				if !w.area.empty() {
					w.needsFull = true
					w.dmg.set(w.area)
				}
			}
		}
	}

	var items []emitItem
	for wi, w := range comp.windows {
		if w.area.empty() {
			continue
		}
		// Stacking order for the terminal. Tiles never overlap, so this
		// only matters in single-app mode, where a dialog floats over the
		// application. overlayZ keeps the launcher above all of them.
		wz := 1
		if w.float {
			wz = 1 + wi
			if wz >= overlayZ {
				wz = overlayZ - 1
			}
		}
		if w.dmg.empty() && !w.needsFull {
			continue
		}
		full := w.needsFull || !r.layered ||
			w.imgW != w.area.x1-w.area.x0 || w.imgH != w.area.y1-w.area.y0 ||
			r.mode != "delta"
		dmg := w.dmg
		if full {
			dmg.set(w.area)
		}
		dmg.clipTo(w.area.clip(comp.frameW, comp.frameH))
		for i := 0; i < dmg.n; i++ {
			r.compositeWindow(w, dmg.r[i])
		}
		items = append(items, emitItem{
			imgID: w.imgID, area: w.area, dmg: dmg,
			col: w.cell.x + 1, row: w.cell.y + 1, z: wz, full: full,
		})
		w.dmg.clear()
		w.needsFull = false
		w.placed = r.layered
		if r.layered {
			w.imgW, w.imgH = w.area.x1-w.area.x0, w.area.y1-w.area.y0
		}
	}
	t1 := time.Now()

	// The launcher overlay. It is planned here, under the lock, and written
	// outside it like everything else.
	//
	// In per-window mode it is its own image at its own cell with z above
	// the tiles, so opening it, typing in it and closing it never cost a
	// tile a single byte. In single-canvas mode there is only one image, so
	// it is blended into whatever rectangle is being transmitted; comp.frame
	// itself is never touched either way, which is what makes closing the
	// panel a repaint of pixels the canvas already holds rather than a
	// recomposite of everything underneath.
	var (
		lcItem   emitItem
		lcSend   bool
		lcDelete bool
		lcSrc    *launcher
	)
	r.blendLC = nil
	if lc := comp.lc; lc != nil {
		if lc.open {
			if lc.change != lcNone || !lc.placed {
				if !lc.placed {
					lc.change = lcAll
				}
				d := lc.repaint()
				lc.dmg = d
			}
			full := !lc.placed || r.mode != "delta" ||
				lc.imgW != lc.pixW || lc.imgH != lc.pixH
			if full {
				lc.dmg.set(lc.area)
			}
			if !lc.dmg.empty() {
				if r.layered {
					lcItem = emitItem{
						imgID: lc.imgID, area: lc.area, dmg: lc.dmg,
						col: lc.cell.x + 1, row: lc.cell.y + 1, z: overlayZ, full: full,
					}
					lcSrc = lc
					lcSend = true
					lc.placed = true
					lc.imgW, lc.imgH = lc.pixW, lc.pixH
				} else {
					for i := 0; i < lc.dmg.n; i++ {
						r.rootDmg = r.rootDmg.union(lc.dmg.r[i])
					}
					r.rootDirty = true
					lc.placed = true
				}
				lc.dmg.clear()
			}
			if !r.layered {
				r.blendLC = lc
			}
		} else if lc.closing {
			// Dismissal. In per-window mode this is one delete escape and
			// zero pixels: the tiles under the panel were never overwritten,
			// so the terminal already holds what is underneath.
			lc.closing = false
			lc.placed = false
			lc.imgW, lc.imgH = 0, 0
			if r.layered {
				lcDelete = true
			} else {
				r.rootDmg = r.rootDmg.union(lc.area)
				r.rootDirty = true
			}
		}
	}

	freed := comp.freeImages
	comp.freeImages = nil
	cbs := comp.pendingCB
	comp.pendingCB = nil
	if lcDelete {
		freed = append(freed, comp.lc.imgID)
		logf("overlay dismiss: one delete escape, 0 px, %d tiles touched", len(items))
	}
	rootDirty := r.rootDirty
	rootDmg := r.rootDmg.clip(comp.frameW, comp.frameH)
	r.rootDirty = false
	r.rootDmg = rect{}
	frameW, frameH := comp.frameW, comp.frameH
	comp.mu.Unlock()

	if !rootDirty && len(items) == 0 && len(freed) == 0 && !lcSend {
		r.completeCallbacks(cbs)
		return
	}

	t2 := time.Now()
	r.emitBuf = r.emitBuf[:0]
	shmBytes := 0
	for _, id := range freed {
		r.emitBuf = append(r.emitBuf, fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)...)
	}
	if r.layered {
		if rootDirty {
			var d damageSet
			d.set(rootDmg)
			shmBytes += r.emitImage(0, rect{0, 0, frameW, frameH}, d, 1, 1, 0,
				r.mode != "delta" || !r.rootSent)
		}
		for _, it := range items {
			shmBytes += r.emitImage(it.imgID, it.area, it.dmg, it.col, it.row, it.z, it.full)
		}
		if lcSend {
			before := len(r.emitBuf)
			n := r.emitLayer(lcSrc, lcItem)
			shmBytes += n
			stats.overlayPx.Add(uint64(lcItem.dmg.pixels()))
			stats.overlayOps.Add(1)
			logf("overlay %s: rects=%d px=%d shm_bytes=%d pty_bytes=%d tiles_touched=%d",
				map[bool]string{true: "transmit", false: "patch"}[lcItem.full],
				lcItem.dmg.n, lcItem.dmg.pixels(), n, len(r.emitBuf)-before, len(items))
		}
	} else {
		// One canvas: union everything that moved and send that.
		var dmg damageSet
		if rootDirty {
			dmg.add(rootDmg)
		}
		for _, it := range items {
			for i := 0; i < it.dmg.n; i++ {
				dmg.add(it.dmg.r[i])
			}
		}
		if !dmg.empty() {
			shmBytes += r.emitImage(0, rect{0, 0, frameW, frameH}, dmg, 1, 1, 0,
				r.mode != "delta" || !r.rootSent)
		}
	}
	t3 := time.Now()

	if len(r.emitBuf) > 0 {
		r.out.Write(r.emitBuf)
		if r.stamp != nil {
			fmt.Fprintf(r.stamp, "%d\n", time.Now().UnixMilli())
		}
	}
	t4 := time.Now()

	stats.frames.Add(1)
	stats.compositeNs.Add(uint64(t1.Sub(t0)))
	stats.encodeNs.Add(uint64(t3.Sub(t2)))
	stats.writeNs.Add(uint64(t4.Sub(t3)))
	stats.latencyNs.Add(uint64(t4.Sub(dirtyAt)))
	stats.ptyBytes.Add(uint64(len(r.emitBuf)))
	stats.shmBytes.Add(uint64(shmBytes))
	stats.images.Add(uint64(len(items)))
	px := 0
	for _, it := range items {
		px += it.dmg.pixels()
	}
	stats.dmgPixels.Add(uint64(px))

	r.completeCallbacks(cbs)
}

func (r *renderer) completeCallbacks(cbs []frameCB) {
	if len(cbs) == 0 {
		return
	}
	comp := r.comp
	now := nowMs()
	comp.mu.Lock()
	seen := map[*client]bool{}
	for _, cb := range cbs {
		if cb.c.dead {
			continue
		}
		cb.c.event(cb.id, 0, now)
		cb.c.deleteID(cb.id)
		seen[cb.c] = true
	}
	comp.mu.Unlock()
	for c := range seen {
		c.flush()
	}
}

// prepare (re)allocates the canvas; returns true if everything must be
// repainted.
func (r *renderer) prepare() bool {
	comp := r.comp
	w, h := comp.widthPx, comp.heightPx
	if comp.frameW != w || comp.frameH != h || comp.frame == nil {
		comp.frame = make([]byte, w*h*4)
		comp.frameW, comp.frameH = w, h
		r.rootSent = false
		for _, win := range comp.windows {
			win.imgW, win.imgH = 0, 0
			win.needsFull = true
			win.dmg.set(win.area)
		}
		return true
	}
	return false
}

// compositeWindow blits one tile's surfaces into the canvas, clipped both to
// the damage rect and to the tile's own rectangle so a guest can never paint
// over the frame or over its neighbour.
func (r *renderer) compositeWindow(w *window, clip rect) {
	comp := r.comp
	if w.top == nil || w.top.surf == nil {
		return
	}
	clip = clip.clip(comp.frameW, comp.frameH)
	if clip.x0 < w.area.x0 {
		clip.x0 = w.area.x0
	}
	if clip.y0 < w.area.y0 {
		clip.y0 = w.area.y0
	}
	if clip.x1 > w.area.x1 {
		clip.x1 = w.area.x1
	}
	if clip.y1 > w.area.y1 {
		clip.y1 = w.area.y1
	}
	if clip.empty() {
		return
	}
	root := w.top.surf
	if root.content == nil {
		fillRect(comp.frame, comp.frameW, comp.frameH, clip, pal.empty)
		return
	}
	// A floating dialog is blended, not stamped: a toolkit commits its drop
	// shadow and its rounded corners as transparent pixels around the
	// window, and stamping those over the application below paints a black
	// box around the dialog. Blending needs the ground back first, so the
	// application underneath is recomposited into the same rectangle.
	blend := false
	if w.float {
		blend = true
		for _, base := range comp.windows {
			if base == w || base.float || base.area.empty() {
				continue
			}
			r.compositeWindow(base, clip)
			break
		}
	} else if root.w < w.area.x1-w.area.x0 || root.h < w.area.y1-w.area.y0 {
		// A client that has not resized to its new tile yet leaves a
		// margin; paint it with the tile ground rather than stale pixels.
		fillRect(comp.frame, comp.frameW, comp.frameH, clip, pal.empty)
	}
	blitBGRAtoRGBA(comp.frame, comp.frameW, comp.frameH, root.content, root.w, root.h,
		w.area.x0, w.area.y0, blend, clip)
	for _, sub := range root.children {
		if sub.surf != nil && sub.surf.content != nil {
			off := offsetOf(sub.surf)
			blitBGRAtoRGBA(comp.frame, comp.frameW, comp.frameH, sub.surf.content,
				sub.surf.w, sub.surf.h, w.area.x0+off.x, w.area.y0+off.y, true, clip)
		}
	}
	for _, p := range comp.popups {
		if p.xdg == nil || p.xdg.surf == nil || p.xdg.surf.content == nil {
			continue
		}
		if comp.windowForSurface(p.xdg.surf) != w {
			continue
		}
		off := offsetOf(p.xdg.surf)
		blitBGRAtoRGBA(comp.frame, comp.frameW, comp.frameH, p.xdg.surf.content,
			p.xdg.surf.w, p.xdg.surf.h, w.area.x0+off.x, w.area.y0+off.y, true, clip)
	}
}

// blitBGRAtoRGBA copies src (BGRA rows) into dst (RGBA canvas) at (ox,oy),
// optionally alpha-blending. Only pixels inside clip are touched.
func blitBGRAtoRGBA(dst []byte, dw, dh int, src []byte, sw, sh, ox, oy int, blend bool, clip rect) {
	x0, y0 := max(clip.x0, ox), max(clip.y0, oy)
	x1, y1 := min(clip.x1, ox+sw), min(clip.y1, oy+sh)
	x0, y0 = max(x0, 0), max(y0, 0)
	x1, y1 = min(x1, dw), min(y1, dh)
	if x1 <= x0 || y1 <= y0 {
		return
	}
	for dy := y0; dy < y1; dy++ {
		sy := dy - oy
		srow := src[(sy*sw+(x0-ox))*4 : (sy*sw+(x1-ox))*4]
		drow := dst[(dy*dw+x0)*4 : (dy*dw+x1)*4]
		n := x1 - x0
		if !blend {
			// The opaque path is the whole cost of a full-pane frame, and it
			// is a channel swap, not a copy: BGRA in, RGBA out. Doing it a
			// byte at a time is four loads and four stores per pixel. One
			// 32-bit load, a few shifts and one 32-bit store do the same
			// swap, and the alpha is a constant.
			for i := 0; i < n; i++ {
				px := binary.LittleEndian.Uint32(srow[i*4 : i*4+4])
				binary.LittleEndian.PutUint32(drow[i*4:i*4+4],
					0xff000000|(px&0x0000ff)<<16|px&0x00ff00|px>>16&0x0000ff)
			}
			continue
		}
		for i := 0; i < n; i++ {
			b, g, rr, a := srow[i*4], srow[i*4+1], srow[i*4+2], srow[i*4+3]
			if !blend || a == 255 {
				drow[i*4], drow[i*4+1], drow[i*4+2], drow[i*4+3] = rr, g, b, 255
			} else if a > 0 {
				ia := uint32(255 - a)
				drow[i*4] = uint8(uint32(rr) + uint32(drow[i*4])*ia/255)
				drow[i*4+1] = uint8(uint32(g) + uint32(drow[i*4+1])*ia/255)
				drow[i*4+2] = uint8(uint32(b) + uint32(drow[i*4+2])*ia/255)
				drow[i*4+3] = 255
			}
		}
	}
}

// emitImage appends the escapes for one image and returns the bytes that
// travelled through shared memory (zero for base64).
//
// imgID 0 means the chrome layer; it is transmitted as kitty image 1.
func (r *renderer) emitImage(imgID uint32, area rect, dmg damageSet, col, row, z int, full bool) int {
	kid := imgID
	if imgID == 0 {
		kid = 1
	}
	aw, ah := area.x1-area.x0, area.y1-area.y0
	if aw <= 0 || ah <= 0 {
		return 0
	}

	// A run of frame edits is broken up by transmitting the image whole.
	//
	// A patch is a difference from pixels the host is holding, so it is only
	// as good as the host's copy. A host that quietly dropped the image --
	// kitty enforces a storage quota and will -- would otherwise be patched
	// forever and show the last thing it had, because a frame edit naming an
	// image that is gone is an error nobody here reads. One transmission
	// restores the picture and costs a second's worth of nothing.
	//
	// At the default cap this is one whole frame a second, and a guest that is
	// not repainting never reaches it: the count only advances on frames that
	// actually changed something.
	if r.mode == "delta" && !full && r.patches[kid] >= resyncAfterPatches {
		full = true
	}

	if r.mode == "delta" && !full {
		// Animation-frame edits: patch each damaged rectangle in place.
		// Coordinates are relative to the image, so subtract its origin.
		sent := 0
		for i := 0; i < dmg.n; i++ {
			d := dmg.r[i]
			dw, dh := d.x1-d.x0, d.y1-d.y0
			if dw <= 0 || dh <= 0 {
				continue
			}
			r.extract(d)
			name := r.writeShm(r.sub)
			if name == "" {
				continue
			}
			r.emitBuf = append(r.emitBuf, fmt.Sprintf(
				"\x1b_Ga=f,i=%d,r=1,X=1,x=%d,y=%d,s=%d,v=%d,f=32,t=s,q=2;%s\x1b\\",
				kid, d.x0-area.x0, d.y0-area.y0, dw, dh,
				base64.StdEncoding.EncodeToString([]byte(name)))...)
			sent += len(r.sub)
		}
		r.patches[kid]++
		return sent
	}

	r.patches[kid] = 0
	r.extract(area)
	// Place the image at its cell. C=1 keeps the cursor where it was, so
	// several placements can be emitted back to back.
	pos := fmt.Sprintf("\x1b[%d;%dH", row, col)
	if r.mode == "b64" {
		r.emitB64(pos, kid, aw, ah, z)
		if imgID == 0 {
			r.rootSent = true
		}
		return 0
	}
	name := r.writeShm(r.sub)
	if name == "" {
		return 0
	}
	r.emitBuf = append(r.emitBuf, pos...)
	r.emitBuf = append(r.emitBuf, fmt.Sprintf(
		"\x1b_Ga=T,i=%d,p=1,z=%d,f=32,s=%d,v=%d,t=s,q=2,C=1;%s\x1b\\",
		kid, z, aw, ah, base64.StdEncoding.EncodeToString([]byte(name)))...)
	if imgID == 0 {
		r.rootSent = true
	}
	return len(r.sub)
}

func (r *renderer) emitB64(pos string, kid uint32, w, h, z int) {
	need := base64.StdEncoding.EncodedLen(len(r.sub))
	if cap(r.b64buf) < need {
		r.b64buf = make([]byte, need)
	}
	r.b64buf = r.b64buf[:need]
	base64.StdEncoding.Encode(r.b64buf, r.sub)

	r.emitBuf = append(r.emitBuf, pos...)
	data := r.b64buf
	first := true
	for len(data) > 0 {
		chunk := data
		if len(chunk) > 4096 {
			chunk = chunk[:4096]
		}
		data = data[len(chunk):]
		m := 0
		if len(data) > 0 {
			m = 1
		}
		if first {
			r.emitBuf = append(r.emitBuf, fmt.Sprintf(
				"\x1b_Ga=T,i=%d,p=1,z=%d,f=32,s=%d,v=%d,t=d,q=2,C=1,m=%d;", kid, z, w, h, m)...)
			first = false
		} else {
			r.emitBuf = append(r.emitBuf, fmt.Sprintf("\x1b_Gm=%d;", m)...)
		}
		r.emitBuf = append(r.emitBuf, chunk...)
		r.emitBuf = append(r.emitBuf, "\x1b\\"...)
	}
}

// extract copies a canvas rectangle into the scratch buffer as tight rows.
// When a single-canvas overlay is active it is blended in afterwards, so the
// overlay reaches the terminal without ever being written into comp.frame.
func (r *renderer) extract(a rect) {
	comp := r.comp
	if l := r.layerSrc; l != nil {
		r.extractFrom(l.pix, l.pixW, l.area.x0, l.area.y0, a)
		return
	}
	r.extractFrom(comp.frame, comp.frameW, 0, 0, a)
	if r.blendLC != nil {
		r.blendLC.blendInto(r.sub, a)
	}
}

// extractFrom copies the canvas-space rectangle `a` out of a source buffer
// whose pixel (0,0) sits at canvas (ox,oy) and whose stride is srcW.
func (r *renderer) extractFrom(src []byte, srcW, ox, oy int, a rect) {
	aw, ah := a.x1-a.x0, a.y1-a.y0
	need := aw * ah * 4
	if cap(r.sub) < need {
		r.sub = make([]byte, need)
	}
	r.sub = r.sub[:need]
	if aw == srcW && ox == 0 {
		copy(r.sub, src[(a.y0-oy)*srcW*4:(a.y1-oy)*srcW*4])
		return
	}
	for y := 0; y < ah; y++ {
		so := ((a.y0+y-oy)*srcW + (a.x0 - ox)) * 4
		copy(r.sub[y*aw*4:(y+1)*aw*4], src[so:so+aw*4])
	}
}

// overlayZ places the launcher above every tile. Tiles are placed at z=1 and
// the chrome canvas at z=0, so the panel is the only thing in front of a
// guest's pixels.
const overlayZ = 5

// emitLayer transmits an image whose pixels come from a layer buffer rather
// than from the canvas. Same escapes, same shm ring, different source.
func (r *renderer) emitLayer(lc *launcher, it emitItem) int {
	r.layerSrc = lc
	defer func() { r.layerSrc = nil }()
	return r.emitImage(it.imgID, it.area, it.dmg, it.col, it.row, it.z, it.full)
}

// resyncAfterPatches is how many frame edits an image may take before it is
// transmitted whole again. One second of them at the default cap.
const resyncAfterPatches = 120

// shmRingSize bounds tmpfs usage: at most this many frame files exist at
// once, even under a terminal that reads but never unlinks. With several
// images per frame the ring has to be deeper than the single-surface
// version's, or a frame can lap itself.
const shmRingSize = 32

// shuttingDown stops the renderer creating new ring slots once teardown has
// begun. Without it the render goroutine can write a fresh frame in the gap
// between cleanupShm() and process exit, and leave it behind: every stray
// file found on this machine came from exactly that race.
var shuttingDown atomic.Bool

func (r *renderer) writeShm(pixels []byte) string {
	if shuttingDown.Load() {
		return ""
	}
	r.shmSeq++
	name := fmt.Sprintf("/wlterm-%d-%d", os.Getpid(), r.shmSeq%shmRingSize)
	os.Remove("/dev/shm" + name)
	f, err := os.OpenFile("/dev/shm"+name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		logf("shm create: %v", err)
		return ""
	}
	_, werr := f.Write(pixels)
	f.Close()
	if werr != nil {
		logf("shm write: %v", werr)
		os.Remove("/dev/shm" + name)
		return ""
	}
	return name
}

func cleanupShm() {
	shuttingDown.Store(true)
	for i := 0; i < shmRingSize; i++ {
		os.Remove(fmt.Sprintf("/dev/shm/wlterm-%d-%d", os.Getpid(), i))
	}
}

// sweepOrphanShm removes ring slots left by earlier wlterm processes that no
// longer exist. Only files matching our own naming are touched, and only
// when their owning pid is gone.
func sweepOrphanShm() {
	ents, err := os.ReadDir("/dev/shm")
	if err != nil {
		return
	}
	for _, e := range ents {
		var pid, slot int
		if n, _ := fmt.Sscanf(e.Name(), "wlterm-%d-%d", &pid, &slot); n != 2 {
			continue
		}
		if pid == os.Getpid() {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			continue // still running
		}
		os.Remove("/dev/shm/" + e.Name())
		logf("swept orphaned shm slot from pid %d", pid)
	}
}

// sweepOrphanRuntime removes private runtime directories left by wlterm
// processes that are gone. A client's own helper daemons can recreate the
// directory microseconds after teardown removed it, so the reliable time to
// collect them is at the next start.
func sweepOrphanRuntime(base string) {
	ents, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range ents {
		var pid int
		n, _ := fmt.Sscanf(e.Name(), "wl-%d", &pid)
		if n != 1 {
			// The name used to be longer. Sweep those too, so an upgrade
			// does not strand the directories the old build left behind.
			n, _ = fmt.Sscanf(e.Name(), "wlterm-rt-%d", &pid)
		}
		if n != 1 {
			continue
		}
		if pid == os.Getpid() {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			continue
		}
		unmountUnder(base + "/" + e.Name())
		os.RemoveAll(base + "/" + e.Name())
		logf("swept orphaned runtime dir from pid %d", pid)
	}
}

func statsLine(elapsed time.Duration) string {
	f := stats.frames.Swap(0)
	if f == 0 {
		return ""
	}
	secs := elapsed.Seconds()
	comp := time.Duration(stats.compositeNs.Swap(0) / f)
	enc := time.Duration(stats.encodeNs.Swap(0) / f)
	wr := time.Duration(stats.writeNs.Swap(0) / f)
	lat := time.Duration(stats.latencyNs.Swap(0) / f)
	pty := stats.ptyBytes.Swap(0)
	shm := stats.shmBytes.Swap(0)
	mIn := stats.motionIn.Swap(0)
	mOut := stats.motionOut.Swap(0)
	imgs := stats.images.Swap(0)
	px := stats.dmgPixels.Swap(0)
	var b strings.Builder
	pace := time.Duration(stats.paceNs.Swap(0) / f)
	idle := time.Duration(stats.idleNs.Swap(0) / f)
	// composite+encode+write+pace+idle accounts for the whole period, so a
	// frame rate that disappoints can be attributed rather than guessed at:
	// idle is the guest rendering, pace is our own frame cap.
	fmt.Fprintf(&b, "fps=%.1f composite=%v encode=%v write=%v pace=%v idle=%v commit_to_out=%v ", float64(f)/secs, comp, enc, wr, pace, idle, lat)
	fmt.Fprintf(&b, "pty_bytes_per_s=%d shm_bytes_per_s=%d ", int(float64(pty)/secs), int(float64(shm)/secs))
	fmt.Fprintf(&b, "images_per_frame=%.2f dmg_px_per_frame=%d", float64(imgs)/float64(f), px/f)
	// Every field here is one whitespace-free key=value, so the line stays
	// parseable by splitting on spaces. dmabuf_sync is broken out because it
	// is not compositor CPU: it is the implicit fence, i.e. the client's own
	// render finishing, and only read-minus-sync is work we do.
	if n := stats.shmCopies.Swap(0); n > 0 {
		fmt.Fprintf(&b, " shm_read=%v shm_reads=%d", time.Duration(stats.shmCopyNs.Swap(0)/n), n)
	}
	if n := stats.dmaCopies.Swap(0); n > 0 {
		fmt.Fprintf(&b, " dmabuf_read=%v dmabuf_sync=%v dmabuf_reads=%d",
			time.Duration(stats.dmaCopyNs.Swap(0)/n), time.Duration(stats.dmaSyncNs.Swap(0)/n), n)
	}
	if n := stats.emptyWakes.Swap(0); n > 0 {
		fmt.Fprintf(&b, " empty_wakes=%d", n)
	}
	if mIn > 0 || mOut > 0 {
		fmt.Fprintf(&b, " motion_in=%d motion_out=%d", mIn, mOut)
	}
	if n := stats.sameFrames.Swap(0); n > 0 {
		fmt.Fprintf(&b, " same_frames=%d", n)
	}
	if ops := stats.chromeOps.Swap(0); ops > 0 {
		fmt.Fprintf(&b, " chrome_repaints=%d chrome=%v", ops, time.Duration(stats.chromeNs.Swap(0)/ops))
	} else {
		stats.chromeNs.Store(0)
	}
	if ops := stats.overlayOps.Swap(0); ops > 0 {
		fmt.Fprintf(&b, " overlay_updates=%d overlay_px=%d", ops, stats.overlayPx.Swap(0))
	}
	return b.String()
}
