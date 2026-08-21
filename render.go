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
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type renderStats struct {
	frames      atomic.Uint64
	compositeNs atomic.Uint64
	encodeNs    atomic.Uint64
	writeNs     atomic.Uint64
	latencyNs   atomic.Uint64
	ptyBytes    atomic.Uint64
	shmBytes    atomic.Uint64
	dmgPixels   atomic.Uint64
	images      atomic.Uint64
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
	rootDmg   rect
	shmSeq    int
	sub       []byte
	b64buf    []byte
	emitBuf   []byte
}

func newRenderer(comp *compositor, mode string, layered bool, out *os.File, maxFPS int) *renderer {
	return &renderer{comp: comp, mode: mode, layered: layered, out: out, maxFPS: maxFPS}
}

func (r *renderer) loop() {
	minInterval := time.Second / time.Duration(r.maxFPS)
	last := time.Time{}
	for range r.comp.renderCh {
		if d := time.Since(last); d < minInterval {
			time.Sleep(minInterval - d)
		}
		r.frame()
		last = time.Now()
	}
}

// emitItem is one image transmission planned under the lock and written
// outside it.
type emitItem struct {
	imgID    uint32
	area     rect // canvas region the image covers
	dmg      rect // canvas region that actually changed
	col, row int  // cell position of the placement
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
		r.rootDmg = r.rootDmg.union(r.drawChrome(full))
		r.rootDirty = true
		if full {
			// The ground was repainted under every tile, so every tile owes
			// a recomposite. This is the expensive path, and it only runs
			// when the tiling itself changed.
			for _, w := range comp.windows {
				if !w.area.empty() {
					w.needsFull = true
					w.dmg = w.area
				}
			}
		}
	}

	var items []emitItem
	for _, w := range comp.windows {
		if w.area.empty() {
			continue
		}
		if w.dmg.empty() && !w.needsFull {
			continue
		}
		dmg := w.dmg.union(rect{})
		if w.needsFull || w.imgW != w.area.x1-w.area.x0 || w.imgH != w.area.y1-w.area.y0 {
			dmg = w.area
		}
		dmg = dmg.clip(comp.frameW, comp.frameH)
		r.compositeWindow(w, dmg)
		full := w.needsFull || !r.layered ||
			w.imgW != w.area.x1-w.area.x0 || w.imgH != w.area.y1-w.area.y0 ||
			r.mode != "delta"
		items = append(items, emitItem{
			imgID: w.imgID, area: w.area, dmg: dmg,
			col: w.cell.x + 1, row: w.cell.y + 1, z: 1, full: full,
		})
		w.dmg = rect{}
		w.needsFull = false
		w.placed = true
	}
	t1 := time.Now()

	freed := comp.freeImages
	comp.freeImages = nil
	cbs := comp.pendingCB
	comp.pendingCB = nil
	rootDirty := r.rootDirty
	rootDmg := r.rootDmg.clip(comp.frameW, comp.frameH)
	r.rootDirty = false
	r.rootDmg = rect{}
	frameW, frameH := comp.frameW, comp.frameH
	comp.mu.Unlock()

	if !rootDirty && len(items) == 0 && len(freed) == 0 {
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
			shmBytes += r.emitImage(0, rect{0, 0, frameW, frameH}, rootDmg, 1, 1, 0,
				r.mode != "delta" || !r.rootSent)
		}
		for _, it := range items {
			shmBytes += r.emitImage(it.imgID, it.area, it.dmg, it.col, it.row, it.z, it.full)
		}
	} else {
		// One canvas: union everything that moved and send that.
		dmg := rect{}
		if rootDirty {
			dmg = rootDmg
		}
		for _, it := range items {
			dmg = dmg.union(it.dmg)
		}
		if !dmg.empty() {
			shmBytes += r.emitImage(0, rect{0, 0, frameW, frameH}, dmg, 1, 1, 0, r.mode != "delta" || !r.rootSent)
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
		px += (it.dmg.x1 - it.dmg.x0) * (it.dmg.y1 - it.dmg.y0)
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
			win.dmg = win.area
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
	// A client that has not resized to its new tile yet leaves a margin;
	// paint it with the tile ground rather than stale pixels.
	if root.w < w.area.x1-w.area.x0 || root.h < w.area.y1-w.area.y0 {
		fillRect(comp.frame, comp.frameW, comp.frameH, clip, pal.empty)
	}
	blitBGRAtoRGBA(comp.frame, comp.frameW, comp.frameH, root.content, root.w, root.h,
		w.area.x0, w.area.y0, false, clip)
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
func (r *renderer) emitImage(imgID uint32, area, dmg rect, col, row, z int, full bool) int {
	kid := imgID
	if imgID == 0 {
		kid = 1
	}
	aw, ah := area.x1-area.x0, area.y1-area.y0
	if aw <= 0 || ah <= 0 {
		return 0
	}

	if r.mode == "delta" && !full {
		// Animation-frame edit: patch the damage rect in place. Coordinates
		// are relative to the image, so subtract the image origin.
		dw, dh := dmg.x1-dmg.x0, dmg.y1-dmg.y0
		if dw <= 0 || dh <= 0 {
			return 0
		}
		r.extract(dmg)
		name := r.writeShm(r.sub)
		if name == "" {
			return 0
		}
		r.emitBuf = append(r.emitBuf, fmt.Sprintf(
			"\x1b_Ga=f,i=%d,r=1,X=1,x=%d,y=%d,s=%d,v=%d,f=32,t=s,q=2;%s\x1b\\",
			kid, dmg.x0-area.x0, dmg.y0-area.y0, dw, dh,
			base64.StdEncoding.EncodeToString([]byte(name)))...)
		return len(r.sub)
	}

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
func (r *renderer) extract(a rect) {
	comp := r.comp
	w := comp.frameW
	aw, ah := a.x1-a.x0, a.y1-a.y0
	need := aw * ah * 4
	if cap(r.sub) < need {
		r.sub = make([]byte, need)
	}
	r.sub = r.sub[:need]
	if aw == w {
		copy(r.sub, comp.frame[a.y0*w*4:a.y1*w*4])
		return
	}
	for y := 0; y < ah; y++ {
		so := ((a.y0+y)*w + a.x0) * 4
		copy(r.sub[y*aw*4:(y+1)*aw*4], comp.frame[so:so+aw*4])
	}
}

// shmRingSize bounds tmpfs usage: at most this many frame files exist at
// once, even under a terminal that reads but never unlinks. With several
// images per frame the ring has to be deeper than the single-surface
// version's, or a frame can lap itself.
const shmRingSize = 32

func (r *renderer) writeShm(pixels []byte) string {
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
	for i := 0; i < shmRingSize; i++ {
		os.Remove(fmt.Sprintf("/dev/shm/wlterm-%d-%d", os.Getpid(), i))
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
	imgs := stats.images.Swap(0)
	px := stats.dmgPixels.Swap(0)
	var b strings.Builder
	fmt.Fprintf(&b, "fps=%.1f composite=%v encode=%v write=%v commit_to_out=%v ", float64(f)/secs, comp, enc, wr, lat)
	fmt.Fprintf(&b, "pty_bytes_per_s=%d shm_bytes_per_s=%d ", int(float64(pty)/secs), int(float64(shm)/secs))
	fmt.Fprintf(&b, "images_per_frame=%.2f dmg_px_per_frame=%d", float64(imgs)/float64(f), px/f)
	return b.String()
}
