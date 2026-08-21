package main

// Drawing the launcher panel.
//
// The panel is repainted in full on every change (a few hundred kilobytes of
// fill plus some glyph blitting, tens of microseconds) and only its DAMAGE
// is computed precisely, because redrawing is cheap and transmitting is not.
// That split is why an arrow key costs two row rectangles and a counter
// rather than a panel.
//
// Every ink comes from panelPal, which is derived from the panel's own
// ground rather than the canvas's: see initPanelPalette. Matched characters
// are lifted to the loudest ink the panel allows AND underlined, so the
// highlight never rests on colour alone.

import (
	"fmt"
	"time"
)

// repaint redraws the panel and returns the canvas-space rectangles that
// have to be retransmitted.
func (lc *launcher) repaint() damageSet {
	t0 := time.Now()
	defer func() { lc.drawNs = time.Since(t0) }()

	geomChanged := lc.layout()
	if geomChanged {
		lc.change = lcAll
	}
	w, h := lc.pixW, lc.pixH
	dst := lc.pix[:w*h*4]
	p := panelPal

	fillRect(dst, w, h, rect{0, 0, w, h}, p.fill)

	if len(lc.sig) != lc.visible {
		lc.sig = make([]uint64, lc.visible)
		lc.prevSig = make([]uint64, lc.visible)
		lc.change = lcAll
	}

	rad := lc.comp.cellW / 2
	if rad > lc.comp.cellH/2 {
		rad = lc.comp.cellH / 2
	}
	if rad < 2 {
		rad = 2
	}
	hookedRect(dst, w, h, rect{0, 0, w, h}, lc.border, rad, p.ring, rect{})

	prevScroll := lc.scrollAt
	lc.drawPrompt(dst, w, h)
	lc.drawList(dst, w, h)

	var d damageSet
	off := func(r rect) rect {
		if r.empty() {
			return r
		}
		return rect{r.x0 + lc.area.x0, r.y0 + lc.area.y0, r.x1 + lc.area.x0, r.y1 + lc.area.y0}
	}
	switch lc.change {
	case lcAll:
		d.set(off(rect{0, 0, w, h}))
	default:
		// The counter always moves with the selection; the rest of the
		// prompt only when the query does.
		if lc.change == lcList {
			d.add(off(lc.promptRect()))
		} else {
			d.add(off(lc.counterRect()))
		}
		// Only the rows whose drawn content actually differs. Typing a
		// character that narrows 26 results to 8 leaves ten blank rows
		// blank, and those cost nothing.
		for k := 0; k < lc.visible; k++ {
			if lc.sig[k] != lc.prevSig[k] {
				d.add(off(lc.rowSlab(k)))
			}
		}
		if lc.scrollAt != prevScroll {
			d.add(off(prevScroll))
			d.add(off(lc.scrollAt))
		}
	}
	copy(lc.prevSig, lc.sig)
	lc.change = lcNone
	return d
}

func (lc *launcher) promptRect() rect {
	return rect{lc.border, lc.border, lc.pixW - lc.border, lc.listTop}
}

func (lc *launcher) listRect() rect {
	return rect{lc.border, lc.listTop, lc.pixW - lc.border, lc.pixH - lc.border}
}

// rowRect is the slab one result occupies, in panel-local pixels. An index
// outside the visible page returns an empty rectangle, which damageSet
// drops.
func (lc *launcher) rowRect(i int) rect {
	if i < lc.top || i >= lc.top+lc.visible {
		return rect{}
	}
	return lc.rowSlab(i - lc.top)
}

// rowSlab is the k-th visible slot, independent of which result is in it.
func (lc *launcher) rowSlab(k int) rect {
	if k < 0 || k >= lc.visible {
		return rect{}
	}
	y := lc.listTop + k*lc.rowH
	if y+lc.rowH > lc.pixH-lc.border {
		return rect{}
	}
	in := lc.border + lc.pad/2
	return rect{in, y, lc.pixW - in, y + lc.rowH}
}

func (lc *launcher) counterRect() rect {
	a := lc.atlas
	if a == nil {
		return rect{}
	}
	wid := 12 * a.w
	return rect{lc.pixW - lc.border - lc.pad - wid, lc.border + lc.pad,
		lc.pixW - lc.border, lc.border + lc.pad + lc.rowH}
}

func (lc *launcher) drawPrompt(dst []byte, w, h int) {
	a := lc.atlas
	p := panelPal
	y := lc.border + lc.pad
	ph := a.h + 2
	py := y + (lc.rowH-ph)/2

	// The mode chip, the same idiom the dock uses for "wlterm": a filled
	// pill in the accent with ContrastText on it.
	chip := "run"
	x := lc.border + lc.pad
	cw := len(chip)*a.w + 2*a.w
	pill(dst, w, h, rect{x, py, x + cw, py + ph}, p.prompt)
	drawText(dst, w, h, x+a.w, py+1, chip, contrastText(p.prompt), a)

	// Query text, then a caret. The caret is a solid bar and does not
	// blink: a blinking caret is an animation, and an animation in an
	// overlay is a retransmission every half second for no information.
	qx := x + cw + a.w
	counter := lc.statusLine()
	cx := w - lc.border - lc.pad - len(counter)*a.w
	maxChars := (cx - a.w - qx) / a.w
	q := string(lc.query)
	if maxChars > 0 && len(q) > maxChars {
		q = q[len(q)-maxChars:] // keep the tail: that is where the caret is
	}
	drawText(dst, w, h, qx, py+1, q, p.hit, a)
	caret := qx + len(q)*a.w
	if caret+2 < cx {
		fillRect(dst, w, h, rect{caret, py, caret + 2, py + ph}, p.hit)
	}

	drawText(dst, w, h, cx, py+1, counter, p.dim, a)

	// The hairline between the prompt and the list, in the structure class
	// measured against the panel.
	hy := lc.listTop - lc.pad/2
	fillRect(dst, w, h, rect{lc.border + lc.pad, hy, w - lc.border - lc.pad, hy + 1}, p.rule)
}

// rowSig summarises everything drawn into one row slot, so a repaint can
// tell which slots really changed. It is a hash, not a comparison of the
// pixels: comparing pixels would mean keeping a second copy of the panel.
func rowSig(label, sub string, pos []int, selected bool) uint64 {
	h := uint64(14695981039346656037)
	mix := func(b byte) { h ^= uint64(b); h *= 1099511628211 }
	for i := 0; i < len(label); i++ {
		mix(label[i])
	}
	mix(0)
	for i := 0; i < len(sub); i++ {
		mix(sub[i])
	}
	mix(0)
	for _, p := range pos {
		mix(byte(p))
		mix(byte(p >> 8))
	}
	if selected {
		mix(1)
	}
	return h
}

func (lc *launcher) drawList(dst []byte, w, h int) {
	a := lc.atlas
	p := panelPal
	for k := range lc.sig {
		lc.sig[k] = 0
	}
	lc.scrollAt = rect{}
	if len(lc.rows) == 0 {
		msg := "no match"
		if len(lc.entries) == 0 {
			msg = "no applications found"
		}
		if len(lc.sig) > 0 {
			lc.sig[0] = rowSig(msg, "", nil, false)
		}
		drawText(dst, w, h, (w-len(msg)*a.w)/2, lc.listTop+lc.rowH/2, msg, p.dim, a)
		return
	}
	barW := 4
	if lc.comp.cellH >= 24 {
		barW = 5
	}
	for i := lc.top; i < lc.top+lc.visible && i < len(lc.rows); i++ {
		r := lc.rowRect(i)
		if r.empty() {
			break
		}
		row := lc.rows[i]
		base, hit, dim := p.text, p.hit, p.dim
		if i == lc.sel {
			fillRect(dst, w, h, r, p.selFill)
			// The bar is the non-hue half of the selection signal: it
			// survives a monochrome screenshot and a themed palette.
			fillRect(dst, w, h, rect{r.x0, r.y0, r.x0 + barW, r.y1}, p.selBar)
			base, hit, dim = p.selText, p.selHit, p.selDim
		}
		tx := r.x0 + barW + lc.pad
		ty := r.y0 + (lc.rowH-a.h)/2

		// The sub-line (generic name, comment, or the binary) is right
		// aligned and yields to the label whenever the label needs room.
		avail := (r.x1 - lc.pad - tx) / a.w
		sub := row.e.sub
		if row.via != "" && row.via != sub {
			// Say why this row is here when the reason is not on screen.
			sub = row.via
		}
		subW := 0
		if sub != "" && avail > 24 {
			maxSub := avail / 3
			sub = truncate(sub, maxSub)
			subW = len(sub) + 2
		} else {
			sub, subW = "", 0
		}
		label := truncate(row.e.label, avail-subW)
		pos := row.pos
		if len(label) < len(row.e.label) {
			// Truncation moved the tail; drop highlights that fell off it.
			pos = clipPositions(pos, len(label))
		}
		lc.sig[i-lc.top] = rowSig(label, sub, pos, i == lc.sel)
		drawTextHL(dst, w, h, tx, ty, label, base, hit, a, pos)
		if sub != "" {
			sx := r.x1 - lc.pad - len(sub)*a.w
			if sx > tx+len(label)*a.w+a.w {
				drawText(dst, w, h, sx, ty, sub, dim, a)
			}
		}
	}

	// A scroll indicator, drawn in the structure class: it is decoration
	// over a list whose real position is already in the counter.
	if len(lc.rows) > lc.visible {
		track := lc.listRect()
		x := track.x1 - 2
		th := track.y1 - track.y0
		kh := th * lc.visible / len(lc.rows)
		if kh < 4 {
			kh = 4
		}
		ky := track.y0 + (th-kh)*lc.top/maxInt(1, len(lc.rows)-lc.visible)
		lc.scrollAt = rect{x, ky, x + 2, ky + kh}
		fillRect(dst, w, h, lc.scrollAt, panelPal.rule)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clipPositions(pos []int, n int) []int {
	out := pos[:0:0]
	for _, p := range pos {
		if p < n {
			out = append(out, p)
		}
	}
	return out
}

// drawTextHL draws s with the characters at pos lifted to `hit` and
// underlined. The underline is what keeps the highlight legible when the two
// inks are close, the same reason the focused pane's rule is drawn bold as
// well as recoloured.
func drawTextHL(dst []byte, w, h, x, y int, s string, base, hit rgb, a *fontAtlas, pos []int) {
	if len(pos) == 0 {
		drawText(dst, w, h, x, y, s, base, a)
		return
	}
	mark := make([]bool, len(s))
	for _, p := range pos {
		if p >= 0 && p < len(s) {
			mark[p] = true
		}
	}
	for i := 0; i < len(s); i++ {
		c, col := base, base
		if mark[i] {
			c, col = hit, hit
		}
		drawText(dst, w, h, x+i*a.w, y, s[i:i+1], c, a)
		if mark[i] {
			fillRect(dst, w, h, rect{x + i*a.w, y + a.h, x + (i+1)*a.w, y + a.h + 1}, col)
		}
	}
}

// blendInto composites the panel into a canvas-rect that has already been
// extracted into buf. This is the single-canvas path: with one image there
// is nowhere to put an overlay except in the image, so it is blended at
// transmission time and comp.frame itself is still never touched.
func (lc *launcher) blendInto(buf []byte, area rect) {
	if !lc.open || lc.pix == nil {
		return
	}
	x0, y0 := maxInt(area.x0, lc.area.x0), maxInt(area.y0, lc.area.y0)
	x1, y1 := minInt(area.x1, lc.area.x1), minInt(area.y1, lc.area.y1)
	if x1 <= x0 || y1 <= y0 {
		return
	}
	aw := area.x1 - area.x0
	for y := y0; y < y1; y++ {
		so := ((y-lc.area.y0)*lc.pixW + (x0 - lc.area.x0)) * 4
		do := ((y-area.y0)*aw + (x0 - area.x0)) * 4
		copy(buf[do:do+(x1-x0)*4], lc.pix[so:so+(x1-x0)*4])
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (lc *launcher) debugLine() string {
	return fmt.Sprintf("launcher rows=%d sel=%d visible=%d panel=%dx%d at cell %d,%d",
		len(lc.rows), lc.sel, lc.visible, lc.pixW, lc.pixH, lc.cell.x, lc.cell.y)
}
