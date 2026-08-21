package main

// Composite committed surfaces into an RGBA canvas and emit it as kitty
// graphics. Three transports:
//   delta: t=s shm + a=f,r=1 damage-rect edits (kitty >= 0.28)
//   shm:   t=s shm + full a=T retransmit each frame (tuios, kitty)
//   b64:   t=d base64 + full a=T retransmit (any kitty-graphics terminal)

import (
	"encoding/base64"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

type renderStats struct {
	frames       atomic.Uint64
	compositeNs  atomic.Uint64
	encodeNs     atomic.Uint64
	writeNs      atomic.Uint64
	latencyNs    atomic.Uint64
	bytesOut     atomic.Uint64
	damagePixels atomic.Uint64
}

var stats renderStats

type renderer struct {
	comp    *compositor
	mode    string // delta | shm | b64
	out     *os.File
	imgSent bool
	shmSeq  int
	maxFPS  int
	stamp   *os.File
	sub     []byte // scratch for damage sub-rect
	b64buf  []byte
	emitBuf []byte
}

func newRenderer(comp *compositor, mode string, out *os.File, maxFPS int) *renderer {
	return &renderer{comp: comp, mode: mode, out: out, maxFPS: maxFPS}
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

// frame composites and emits one frame. Called from render goroutine.
func (r *renderer) frame() {
	comp := r.comp
	comp.mu.Lock()
	if !comp.dirty {
		comp.mu.Unlock()
		return
	}
	comp.dirty = false
	dmg := comp.damage
	comp.damage = rect{}
	dirtyAt := comp.dirtyAt

	t0 := time.Now()
	full := r.prepare()
	if full {
		dmg = rect{0, 0, comp.frameW, comp.frameH}
	}
	dmg = dmg.clip(comp.frameW, comp.frameH)
	r.composite(dmg)
	t1 := time.Now()

	// Collect frame callbacks to complete after emit.
	type cbT struct {
		c  *client
		id uint32
	}
	var cbs []cbT
	if comp.toplevel != nil && comp.toplevel.surf != nil {
		s := comp.toplevel.surf
		for _, id := range s.frameCallbacks {
			cbs = append(cbs, cbT{s.client, id})
		}
		s.frameCallbacks = nil
		for _, ch := range s.children {
			if ch.surf != nil {
				for _, id := range ch.surf.frameCallbacks {
					cbs = append(cbs, cbT{ch.surf.client, id})
				}
				ch.surf.frameCallbacks = nil
			}
		}
	}
	comp.mu.Unlock()

	if !dmg.empty() {
		t2 := time.Now()
		payload := r.encode(dmg)
		t3 := time.Now()
		if len(payload) > 0 {
			r.out.Write(payload)
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
		stats.bytesOut.Add(uint64(len(payload)))
		stats.damagePixels.Add(uint64((dmg.x1 - dmg.x0) * (dmg.y1 - dmg.y0)))
	}

	now := nowMs()
	comp.mu.Lock()
	for _, cb := range cbs {
		cb.c.event(cb.id, 0, now) // wl_callback.done
		cb.c.deleteID(cb.id)
	}
	seen := map[*client]bool{}
	for _, cb := range cbs {
		if !seen[cb.c] {
			seen[cb.c] = true
			cb.c.flush()
		}
	}
	comp.mu.Unlock()
}

// prepare (re)allocates the canvas; returns true if a full repaint is needed.
func (r *renderer) prepare() bool {
	comp := r.comp
	w, h := comp.widthPx, comp.heightPx
	if comp.frameW != w || comp.frameH != h || comp.frame == nil {
		comp.frame = make([]byte, w*h*4)
		comp.frameW, comp.frameH = w, h
		r.imgSent = false // canvas size changed: retransmit root image
		return true
	}
	return false
}

// composite blits the toplevel (and subsurfaces) into comp.frame as RGBA,
// touching only the clip rect.
func (r *renderer) composite(clip rect) {
	comp := r.comp
	w, h := comp.frameW, comp.frameH
	top := comp.toplevel
	if top == nil || top.surf == nil || top.surf.content == nil {
		return
	}
	blitBGRAtoRGBA(comp.frame, w, h, top.surf.content, top.surf.w, top.surf.h, 0, 0, false, clip)
	for _, sub := range top.surf.children {
		if sub.surf != nil && sub.surf.content != nil {
			blitBGRAtoRGBA(comp.frame, w, h, sub.surf.content, sub.surf.w, sub.surf.h, sub.x, sub.y, true, clip)
		}
	}
}

// blitBGRAtoRGBA copies src (BGRA rows) into dst (RGBA canvas) at (ox,oy),
// optionally alpha-blending (premultiplied source, as Wayland argb8888 is).
// Only pixels inside clip (canvas coords) are touched.
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
			b := srow[i*4]
			g := srow[i*4+1]
			rr := srow[i*4+2]
			a := srow[i*4+3]
			if !blend || a == 255 {
				drow[i*4] = rr
				drow[i*4+1] = g
				drow[i*4+2] = b
				drow[i*4+3] = 255
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

// encode produces the kitty escape bytes for this frame's damage.
func (r *renderer) encode(dmg rect) []byte {
	comp := r.comp
	w, h := comp.frameW, comp.frameH
	r.emitBuf = r.emitBuf[:0]

	switch r.mode {
	case "delta":
		if !r.imgSent {
			r.emitFull()
		} else {
			dw := dmg.x1 - dmg.x0
			dh := dmg.y1 - dmg.y0
			if dw*dh*3 > w*h*2 {
				r.emitFull() // damage nearly full: simpler to retransmit
			} else {
				r.extractRect(dmg)
				name := r.writeShm(r.sub)
				if name == "" {
					return nil
				}
				b64 := base64.StdEncoding.EncodeToString([]byte(name))
				r.emitBuf = append(r.emitBuf, fmt.Sprintf(
					"\x1b_Ga=f,i=1,r=1,X=1,x=%d,y=%d,s=%d,v=%d,f=32,t=s,q=2;%s\x1b\\",
					dmg.x0, dmg.y0, dw, dh, b64)...)
			}
		}
	case "shm":
		r.emitFull()
	case "b64":
		r.emitFullB64()
	}
	return r.emitBuf
}

func (r *renderer) extractRect(dmg rect) {
	comp := r.comp
	w := comp.frameW
	dw := dmg.x1 - dmg.x0
	dh := dmg.y1 - dmg.y0
	need := dw * dh * 4
	if cap(r.sub) < need {
		r.sub = make([]byte, need)
	}
	r.sub = r.sub[:need]
	for y := 0; y < dh; y++ {
		so := ((dmg.y0+y)*w + dmg.x0) * 4
		copy(r.sub[y*dw*4:(y+1)*dw*4], comp.frame[so:so+dw*4])
	}
}

// emitFull: home cursor + a=T full image via t=s.
func (r *renderer) emitFull() {
	comp := r.comp
	name := r.writeShm(comp.frame)
	if name == "" {
		return
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(name))
	r.emitBuf = append(r.emitBuf, fmt.Sprintf(
		"\x1b[H\x1b_Ga=T,i=1,f=32,s=%d,v=%d,t=s,q=2,C=1;%s\x1b\\",
		comp.frameW, comp.frameH, b64)...)
	r.imgSent = true
}

func (r *renderer) emitFullB64() {
	comp := r.comp
	need := base64.StdEncoding.EncodedLen(len(comp.frame))
	if cap(r.b64buf) < need {
		r.b64buf = make([]byte, need)
	}
	r.b64buf = r.b64buf[:need]
	base64.StdEncoding.Encode(r.b64buf, comp.frame)

	r.emitBuf = append(r.emitBuf, "\x1b[H"...)
	first := true
	data := r.b64buf
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
				"\x1b_Ga=T,i=1,f=32,s=%d,v=%d,t=d,q=2,C=1,m=%d;", comp.frameW, comp.frameH, m)...)
			first = false
		} else {
			r.emitBuf = append(r.emitBuf, fmt.Sprintf("\x1b_Gm=%d;", m)...)
		}
		r.emitBuf = append(r.emitBuf, chunk...)
		r.emitBuf = append(r.emitBuf, "\x1b\\"...)
	}
	r.imgSent = true
}

// shmRingSize bounds tmpfs usage: at most this many frame files exist at
// once, even under a terminal that reads but never unlinks (tuios). A
// terminal that does unlink (kitty) usually leaves zero behind.
const shmRingSize = 8

// writeShm writes pixels into the next ring slot and returns its POSIX shm
// name. The previous occupant of the slot is unlinked first.
func (r *renderer) writeShm(pixels []byte) string {
	r.shmSeq++
	name := fmt.Sprintf("/wlterm-%d-%d", os.Getpid(), r.shmSeq%shmRingSize)
	os.Remove("/dev/shm" + name) // reclaim slot if the terminal left it
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

// cleanupShm removes every ring slot. Call on every exit path.
func cleanupShm() {
	for i := 0; i < shmRingSize; i++ {
		os.Remove(fmt.Sprintf("/dev/shm/wlterm-%d-%d", os.Getpid(), i))
	}
}

func statsLine() string {
	f := stats.frames.Swap(0)
	if f == 0 {
		return ""
	}
	comp := time.Duration(stats.compositeNs.Swap(0) / f)
	enc := time.Duration(stats.encodeNs.Swap(0) / f)
	wr := time.Duration(stats.writeNs.Swap(0) / f)
	lat := time.Duration(stats.latencyNs.Swap(0) / f)
	by := stats.bytesOut.Swap(0) / f
	px := stats.damagePixels.Swap(0) / f
	return fmt.Sprintf("frames=%d composite=%v encode=%v write=%v commit_to_out=%v avg_bytes=%d avg_dmg_px=%d",
		f, comp, enc, wr, lat, by, px)
}
