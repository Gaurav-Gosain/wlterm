package main

// The frame: ground, dividers, title badges, dock.
//
// tuios draws pane frames with box-drawing glyphs in the terminal's own font
// and, since c534c76 ("draw the structure at a third contrast class"), draws
// the rules *between* panes in a third ink that measures ~1.9:1 against the
// ground, well under the 3:1 WCAG floor for marks, because a divider carries
// nothing of its own. The focused pane keeps its ring at the 3:1 mark floor,
// drawn bold, with the corners bent into the pane so the signal never rests
// on hue alone.
//
// wlterm emits pixels rather than glyphs, so the same vocabulary is
// reproduced geometrically: a hairline down the middle of the reserved
// divider cell, square where it is structure, thicker and hooked inward
// where it belongs to the focused window. The inks are not hard-coded; they
// are computed from the ground with tuios's own algorithms, so the ratios
// come out at the same numbers and are logged at startup.

import (
	"fmt"
	"math"
	"strings"
)

type rgb struct{ r, g, b uint8 }

func hexc(s string) rgb {
	var r, g, b uint8
	fmt.Sscanf(strings.TrimPrefix(s, "#"), "%02x%02x%02x", &r, &g, &b)
	return rgb{r, g, b}
}

func (c rgb) hex() string { return fmt.Sprintf("#%02X%02X%02X", c.r, c.g, c.b) }

// charmtone, the ramp tuios's chrome is built from.
var (
	pepper  = hexc("#201F26") // Canvas
	bbq     = hexc("#2D2C36") // Panel
	butter  = hexc("#FFFAF1") // Fg
	smoke   = hexc("#BFBCC8") // FgDim
	squid   = hexc("#858392") // FgMute
	charple = hexc("#6B50FF") // Accent
)

// The three contrast classes, in tuios's ladder order.
const (
	structureTarget = 1.9 // decorative rules: a target, not a floor
	markFloor       = 3.0 // non-text marks: borders, glyphs, cursor
	contrastFloor   = 4.5 // chrome text
)

// Pane border inks, before lifting. tuios distinguishes window mode from
// terminal mode; wlterm always forwards keys to the guest, so the focused
// ring uses the terminal-mode green.
var (
	focusInkBase = hexc("#AAFFAA")
	groundColor  = pepper
)

type palette struct {
	ground    rgb
	structure rgb // dividers, dock hairline: ~1.9:1
	focus     rgb // focused ring: >= 3:1
	text      rgb // legible on the ground
	dim       rgb
	accent    rgb // a mark: bars and rules, lifted to the mark floor
	chip      rgb // a fill that carries ContrastText at the text floor
	empty     rgb // a tile whose client has not drawn yet
}

var pal palette

func initPalette() {
	pal = palette{
		ground:    groundColor,
		structure: structureInk(groundColor),
		focus:     readableAt(focusInkBase, groundColor, markFloor),
		text:      butter,
		dim:       smoke,
		accent:    charple,
		chip:      chipFill(charple, contrastFloor),
		empty:     bbq,
	}
	initPanelPalette()
}

// relLum is the WCAG 2.x relative luminance of a colour.
func relLum(c rgb) float64 {
	f := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*f(c.r) + 0.7152*f(c.g) + 0.0722*f(c.b)
}

func contrast(a, b rgb) float64 {
	la, lb := relLum(a), relLum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func mix(a, b rgb, t float64) rgb {
	l := func(x, y uint8) uint8 {
		return uint8(float64(x) + (float64(y)-float64(x))*t + 0.5)
	}
	return rgb{l(a.r, b.r), l(a.g, b.g), l(a.b, b.b)}
}

// contrastText picks whichever of the two chrome extremes reads better on a
// given ground, exactly as tuios's ContrastText does.
func contrastText(bg rgb) rgb {
	if contrast(butter, bg) >= contrast(pepper, bg) {
		return butter
	}
	return pepper
}

// inkAt starts from the most legible ink on this ground and blends it back
// toward the ground, bisecting for the least blend that still measures at or
// under `target`. Because contrast falls monotonically as the blend grows,
// the result sits essentially exactly on the target, which makes it usable
// both for quieting an ink down to the structure class and for finding the
// quietest ink that still clears a legibility floor.
//
// A fixed grey cannot do this job, because the quietest a single ink can be
// against both black and white at once is 4.58:1, louder than the labels a
// rule is meant to sit beneath.
func inkAt(bg rgb, target float64) rgb {
	ink := contrastText(bg)
	if contrast(ink, bg) <= target {
		return ink
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 16; i++ {
		mid := (lo + hi) / 2
		if contrast(mix(ink, bg, mid), bg) > target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return mix(ink, bg, hi)
}

// structureInk is tuios's Structure(): the decorative-rule class, ~1.9:1.
func structureInk(bg rgb) rgb { return inkAt(bg, structureTarget) }

// inkAtLeast is inkAt for a floor rather than a target: the quietest ink
// that still measures at or above it. inkAt lands a hair below its number,
// which is right for a target and wrong for a floor, and "a hair below 4.5"
// is not 4.5.
func inkAtLeast(bg rgb, floor float64) rgb {
	ink := contrastText(bg)
	if contrast(ink, bg) <= floor {
		return ink
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 20; i++ {
		mid := (lo + hi) / 2
		if contrast(mix(ink, bg, mid), bg) >= floor {
			lo = mid
		} else {
			hi = mid
		}
	}
	return mix(ink, bg, lo)
}

// chipFill prepares a fill that ContrastText can actually sit on. An accent
// chosen as an accent is not chosen to carry text: charple measures 4.41:1
// against the most legible chrome ink, just under the text floor. Rather
// than pick a different ink for the label and give up ContrastText, the fill
// is pushed away from its own text ink until the pair clears the floor.
// Contrast only rises as the fill moves away, so the bisection is safe and
// the hue is preserved.
func chipFill(c rgb, floor float64) rgb {
	ink := contrastText(c)
	if contrast(ink, c) >= floor {
		return c
	}
	away := pepper
	if ink == pepper {
		away = butter
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 20; i++ {
		mid := (lo + hi) / 2
		if contrast(ink, mix(c, away, mid)) >= floor {
			hi = mid
		} else {
			lo = mid
		}
	}
	return mix(c, away, hi)
}

// readableAt lifts an ink away from its ground until it clears a floor,
// blending toward whichever extreme reads on that ground. Borders are marks,
// so they are lifted to 3:1 and no further.
func readableAt(c, bg rgb, floor float64) rgb {
	if contrast(c, bg) >= floor {
		return c
	}
	target := contrastText(bg)
	lo, hi := 0.0, 1.0
	for i := 0; i < 16; i++ {
		mid := (lo + hi) / 2
		if contrast(mix(c, target, mid), bg) < floor {
			lo = mid
		} else {
			hi = mid
		}
	}
	return mix(c, target, hi)
}

// ---- the launcher panel ----
//
// The panel sits on its own ground, so every ink in it is recomputed against
// THAT ground rather than inherited from the canvas. A dim ink that measures
// 4.5:1 on the canvas measures something else on a lighter panel, and
// "quiet" is a ratio, not a colour.

type panelPalette struct {
	fill    rgb // the panel ground
	ring    rgb // border: a mark, so >= 3:1 against the canvas ground
	rule    rgb // hairline inside the panel: the structure class, ~1.9:1
	text    rgb // primary row text
	dim     rgb // secondary text, sitting exactly on the 4.5:1 floor
	hit     rgb // matched characters: the loudest ink the panel allows
	selFill rgb // the selected row's slab
	selBar  rgb // the accent bar owning the selected row: a mark, >= 3:1
	selText rgb
	selDim  rgb
	selHit  rgb
	prompt  rgb // the prompt chip fill
}

var panelPal panelPalette

func initPanelPalette() {
	fill := pal.empty
	// A slab, not a hue swap: the selection has to read in a monochrome
	// screenshot, and a fill this close to the panel keeps every text ratio
	// on the row within a hair of the unselected ones.
	sel := mix(fill, contrastText(fill), 0.11)
	panelPal = panelPalette{
		fill: fill,
		// The ring is the accent, not the focused-pane green. Two green
		// rings on one screen would have the panel and the tile behind it
		// both claiming focus; the accent already means "wlterm's own
		// surface" everywhere else in the frame, and it ties the panel to
		// the dock chip it is drawn with.
		ring:    readableAt(pal.accent, pal.ground, markFloor),
		rule:    inkAt(fill, structureTarget),
		text:    inkAtLeast(fill, 7.0),
		dim:     inkAtLeast(fill, contrastFloor),
		hit:     contrastText(fill),
		selFill: sel,
		selBar:  readableAt(pal.accent, sel, markFloor),
		selText: inkAtLeast(sel, 7.0),
		selDim:  inkAtLeast(sel, contrastFloor),
		selHit:  contrastText(sel),
		prompt:  chipFill(pal.accent, contrastFloor),
	}
}

// reportPanelContrast logs the launcher's ratios the same way the frame's
// are logged, so "quiet rules, legible text" stays a measured property when
// a second surface with its own ground is added.
func reportPanelContrast() string {
	p := panelPal
	return fmt.Sprintf(
		"panel fill=%s ring=%s (%.2f:1 on canvas, floor %.1f) rule=%.2f:1 "+
			"text=%.2f:1 dim=%.2f:1 hit=%.2f:1 | sel fill=%s bar=%.2f:1 "+
			"text=%.2f:1 dim=%.2f:1 hit=%.2f:1 | prompt=%s chip-text=%.2f:1",
		p.fill.hex(), p.ring.hex(), contrast(p.ring, pal.ground), markFloor,
		contrast(p.rule, p.fill), contrast(p.text, p.fill),
		contrast(p.dim, p.fill), contrast(p.hit, p.fill),
		p.selFill.hex(), contrast(p.selBar, p.selFill),
		contrast(p.selText, p.selFill), contrast(p.selDim, p.selFill),
		contrast(p.selHit, p.selFill),
		p.prompt.hex(), contrast(contrastText(p.prompt), p.prompt))
}

// reportContrast logs every relationship in the frame so "quiet rules,
// legible text" is a measured property rather than an asserted one.
func reportContrast() string {
	return fmt.Sprintf(
		"ground=%s structure=%s (%.3f:1, target %.1f) focus=%s (%.2f:1, floor %.1f) "+
			"title-on-structure=%.2f:1 title-on-focus=%.2f:1 text-on-ground=%.2f:1",
		pal.ground.hex(), pal.structure.hex(), contrast(pal.structure, pal.ground), structureTarget,
		pal.focus.hex(), contrast(pal.focus, pal.ground), markFloor,
		contrast(contrastText(pal.structure), pal.structure),
		contrast(contrastText(pal.focus), pal.focus),
		contrast(pal.text, pal.ground)) +
		fmt.Sprintf(" chip=%s chip-text=%.2f:1 (floor %.1f)",
			pal.chip.hex(), contrast(contrastText(pal.chip), pal.chip), contrastFloor)
}

// ---- primitives ----

func putPx(dst []byte, w, h, x, y int, c rgb, a uint8) {
	if x < 0 || y < 0 || x >= w || y >= h || a == 0 {
		return
	}
	o := (y*w + x) * 4
	if a == 255 {
		dst[o], dst[o+1], dst[o+2], dst[o+3] = c.r, c.g, c.b, 255
		return
	}
	ia := uint32(255 - a)
	dst[o] = uint8((uint32(c.r)*uint32(a) + uint32(dst[o])*ia) / 255)
	dst[o+1] = uint8((uint32(c.g)*uint32(a) + uint32(dst[o+1])*ia) / 255)
	dst[o+2] = uint8((uint32(c.b)*uint32(a) + uint32(dst[o+2])*ia) / 255)
	dst[o+3] = 255
}

func fillRect(dst []byte, w, h int, r rect, c rgb) {
	r = r.clip(w, h)
	if r.empty() {
		return
	}
	row := make([]byte, (r.x1-r.x0)*4)
	for i := 0; i < len(row); i += 4 {
		row[i], row[i+1], row[i+2], row[i+3] = c.r, c.g, c.b, 255
	}
	for y := r.y0; y < r.y1; y++ {
		copy(dst[(y*w+r.x0)*4:], row)
	}
}

// pill fills a rounded slab, the pixel equivalent of a powerline badge with
// half-circle caps.
func pill(dst []byte, w, h int, r rect, c rgb) {
	r = r.clip(w, h)
	if r.empty() {
		return
	}
	rad := float64(r.y1-r.y0) / 2
	cy := float64(r.y0) + rad
	for y := r.y0; y < r.y1; y++ {
		dy := (float64(y) + 0.5) - cy
		inset := rad - math.Sqrt(math.Max(0, rad*rad-dy*dy))
		x0 := r.x0 + int(inset)
		x1 := r.x1 - int(inset)
		if x1 > x0 {
			fillRect(dst, w, h, rect{x0, y, x1, y + 1}, c)
		}
	}
}

// hookedRect draws a one- or two-pixel rule around r with the corners bent
// inward by rad, which is what tuios's rounded corner glyphs do: the rule
// hooks toward whichever pane owns it.
func hookedRect(dst []byte, w, h int, r rect, t, rad int, c rgb, skip rect) {
	drawH := func(x0, x1, y int) {
		if skip.x1 > skip.x0 && y >= skip.y0 && y < skip.y1 {
			if a0, a1 := x0, min(x1, skip.x0); a1 > a0 {
				fillRect(dst, w, h, rect{a0, y, a1, y + t}, c)
			}
			if b0, b1 := max(x0, skip.x1), x1; b1 > b0 {
				fillRect(dst, w, h, rect{b0, y, b1, y + t}, c)
			}
			return
		}
		fillRect(dst, w, h, rect{x0, y, x1, y + t}, c)
	}
	drawH(r.x0+rad, r.x1-rad, r.y0)
	drawH(r.x0+rad, r.x1-rad, r.y1-t)
	fillRect(dst, w, h, rect{r.x0, r.y0 + rad, r.x0 + t, r.y1 - rad}, c)
	fillRect(dst, w, h, rect{r.x1 - t, r.y0 + rad, r.x1, r.y1 - rad}, c)
	if rad > 0 {
		arc(dst, w, h, r.x0+rad, r.y0+rad, rad, t, c, 2)
		arc(dst, w, h, r.x1-rad-t, r.y0+rad, rad, t, c, 1)
		arc(dst, w, h, r.x0+rad, r.y1-rad-t, rad, t, c, 3)
		arc(dst, w, h, r.x1-rad-t, r.y1-rad-t, rad, t, c, 0)
	}
}

// arc draws a quarter circle of thickness t. quad: 0=SE 1=NE 2=NW 3=SW.
func arc(dst []byte, w, h, cx, cy, rad, t int, c rgb, quad int) {
	if rad <= 0 {
		return
	}
	steps := rad * 8
	for i := 0; i <= steps; i++ {
		s, co := math.Sincos(float64(i) / float64(steps) * math.Pi / 2)
		var dx, dy float64
		switch quad {
		case 0:
			dx, dy = co, s
		case 1:
			dx, dy = co, -s
		case 2:
			dx, dy = -co, -s
		default:
			dx, dy = -co, s
		}
		for k := 0; k < t; k++ {
			rr := float64(rad - k)
			putPx(dst, w, h, cx+int(dx*rr+0.5), cy+int(dy*rr+0.5), c, 255)
		}
	}
}

// ---- text ----

func pickAtlas(cellW, cellH int) *fontAtlas {
	var best *fontAtlas
	for i := range fontAtlases {
		a := &fontAtlases[i]
		if a.w <= cellW && a.h <= cellH {
			if best == nil || a.h > best.h {
				best = a
			}
		}
	}
	if best == nil && len(fontAtlases) > 0 {
		best = &fontAtlases[0]
	}
	return best
}

// truncate trims to n cells, tuios-style: cut and mark with an ellipsis
// rather than letting the badge overrun its rule.
func truncate(s string, n int) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r < 32 || r > 126 {
			r = '?'
		}
		out = append(out, byte(r))
	}
	if n <= 0 {
		return ""
	}
	if len(out) <= n {
		return string(out)
	}
	if n <= 3 {
		return string(out[:n])
	}
	return string(out[:n-3]) + "..."
}

func drawText(dst []byte, w, h, x, y int, s string, c rgb, a *fontAtlas) {
	glyph := a.w * a.h
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch < 32 || ch > 126 {
			ch = '?'
		}
		off := int(ch-32) * glyph
		if off+glyph > len(a.pix) {
			continue
		}
		g := a.pix[off : off+glyph]
		for gy := 0; gy < a.h; gy++ {
			base := gy * a.w
			for gx := 0; gx < a.w; gx++ {
				if v := g[base+gx]; v != 0 {
					putPx(dst, w, h, x+i*a.w+gx, y+gy, c, v)
				}
			}
		}
	}
}

// ---- the frame ----

// drawWindowRule draws the rule that surrounds one tile. It runs down the
// middle of the reserved divider cell, so two neighbours land on exactly the
// same pixel line and the grid never doubles up.
func (r *renderer) drawWindowRule(w *window, focused bool) {
	comp := r.comp
	dst, cw, chh := comp.frame, comp.frameW, comp.frameH
	cellW, cellH := comp.cellW, comp.cellH

	box := rect{
		w.area.x0 - cellW/2, w.area.y0 - cellH/2,
		w.area.x1 + cellW/2, w.area.y1 + cellH/2,
	}
	col := pal.structure
	t := 1
	rad := 0
	if focused {
		col = pal.focus
		// "bold as well, so the signal survives themes where the two
		// colours are close, and so it is not carried by hue alone".
		t = 2
		if cellH >= 24 {
			t = 3
		}
		// Corners bent into the focused pane.
		rad = cellW / 2
		if rad > cellH/2 {
			rad = cellH / 2
		}
		if rad < 2 {
			rad = 2
		}
	}

	hookedRect(dst, cw, chh, box, t, rad, col, rect{})
}

// badgeBox is where a window's title badge sits: inline in its bottom rule,
// centred, the way tuios places a bottom-positioned pane title.
func (r *renderer) badgeBox(w *window, focused bool) (rect, string, *fontAtlas) {
	comp := r.comp
	cellW, cellH := comp.cellW, comp.cellH
	box := rect{
		w.area.x0 - cellW/2, w.area.y0 - cellH/2,
		w.area.x1 + cellW/2, w.area.y1 + cellH/2,
	}
	atlas := pickAtlas(cellW, cellH)
	title := w.title()
	if title == "" || atlas == nil {
		return rect{}, "", nil
	}
	label := truncate(title, (box.x1-box.x0)/atlas.w-6)
	if label == "" {
		return rect{}, "", nil
	}
	tw := len(label) * atlas.w
	ph := atlas.h + 2
	py := box.y1 - ph/2
	px0 := (box.x0+box.x1)/2 - (tw+2*atlas.w)/2
	return rect{px0, py, px0 + tw + 2*atlas.w, py + ph}, label, atlas
}

// drawBadge paints one title badge. Badges are drawn after every rule, so a
// neighbour's rule never strikes through a title.
func (r *renderer) drawBadge(w *window, focused bool) {
	comp := r.comp
	box, label, atlas := r.badgeBox(w, focused)
	if atlas == nil {
		return
	}
	col := pal.structure
	if focused {
		col = pal.focus
	}
	pill(comp.frame, comp.frameW, comp.frameH, box, col)
	drawText(comp.frame, comp.frameW, comp.frameH, box.x0+atlas.w, box.y0+1,
		label, contrastText(col), atlas)
}

// drawDock reproduces tuios's two-row dock: a full-width hairline in the
// structure ink on the pane-facing edge, then the bar itself.
func (r *renderer) drawDock() {
	comp := r.comp
	dst, cw, chh := comp.frame, comp.frameW, comp.frameH
	cellW, cellH := comp.cellW, comp.cellH
	rows := chh / cellH
	if rows < dockRows+1 {
		return
	}
	hairY := (rows - dockRows) * cellH
	barY := (rows - dockRows + 1) * cellH

	// Row A: the hairline, drawn through the middle of its cell.
	fillRect(dst, cw, chh, rect{cellW / 2, hairY + cellH/2, cw - cellW/2, hairY + cellH/2 + 1}, pal.structure)

	atlas := pickAtlas(cellW, cellH)
	if atlas == nil {
		return
	}
	ph := atlas.h + 2
	py := barY + (cellH-ph)/2

	// Mode chip: a filled pill in the accent, the way tuios badges the
	// active mode. " Z" is appended while a tile is zoomed.
	label := "wlterm"
	if comp.zoomed != nil {
		label += " Z"
	}
	if comp.prefixArmed {
		label += " ^"
	}
	x := cellW
	pw := len(label)*atlas.w + 2*atlas.w
	pill(dst, cw, chh, rect{x, py, x + pw, py + ph}, pal.chip)
	drawText(dst, cw, chh, x+atlas.w, py+1, label, contrastText(pal.chip), atlas)

	// Trailing counter, right aligned: layout mode and pane count.
	idx := 0
	for i, w := range comp.windows {
		if w == comp.focus {
			idx = i + 1
		}
	}
	right := fmt.Sprintf("%s %d/%d", comp.mode, idx, len(comp.windows))
	rx := cw - cellW - len(right)*atlas.w
	drawText(dst, cw, chh, rx, py+1, right, pal.dim, atlas)

	// Hint strip in the middle, dimmed: the bindings that matter.
	hint := fmt.Sprintf("%s then d apps | n split | tab focus | x close | z zoom | space layout", comp.prefixName)
	hx := x + pw + 2*cellW
	if hx+len(hint)*atlas.w < rx-cellW {
		drawText(dst, cw, chh, hx, py+1, hint, pal.structure, atlas)
	}
}

// ruleBand is the region a window's rule and badge occupy: one cell out
// from its content rectangle on every side. Neighbouring tiles are exactly
// two cells apart, so a band never reaches into another tile's content.
func (comp *compositor) ruleBand(w *window) [4]rect {
	cw, ch := comp.cellW, comp.cellH
	o := rect{w.area.x0 - cw, w.area.y0 - ch, w.area.x1 + cw, w.area.y1 + ch}
	return [4]rect{
		{o.x0, o.y0, o.x1, w.area.y0},
		{o.x0, w.area.y1, o.x1, o.y1},
		{o.x0, w.area.y0, w.area.x0, w.area.y1},
		{w.area.x1, w.area.y0, o.x1, w.area.y1},
	}
}

// drawBackdrop repaints whatever lives under the guests and returns the
// canvas region it touched.
//
// In single-app mode that is one flood fill and nothing else. There is no
// frame, no title, no focus ring and no dock, because tuios already draws
// all four around the pane and a second set inside it is noise. The fill
// still has to happen: a client whose surface is smaller than the pane (one
// that has not yet acked its configure, or one that simply ignores the size)
// would otherwise expose whatever the canvas held before.
func (r *renderer) drawBackdrop(full bool) rect {
	comp := r.comp
	if !comp.single {
		return r.drawChrome(full)
	}
	if !full {
		return rect{}
	}
	all := rect{0, 0, comp.frameW, comp.frameH}
	fillRect(comp.frame, comp.frameW, comp.frameH, all, pal.ground)
	return all
}

// drawChrome repaints the frame and returns the canvas region it touched.
//
// full repaints the ground under everything, which is only correct right
// after a relayout, when every tile is going to be recomposited anyway.
// Otherwise only the rule bands and the dock are cleared and redrawn, so a
// focus change never disturbs a single guest pixel.
func (r *renderer) drawChrome(full bool) rect {
	comp := r.comp
	dmg := rect{}
	if full {
		fillRect(comp.frame, comp.frameW, comp.frameH, rect{0, 0, comp.frameW, comp.frameH}, pal.ground)
		dmg = rect{0, 0, comp.frameW, comp.frameH}
	} else {
		for _, w := range comp.windows {
			if w.area.empty() {
				continue
			}
			for _, b := range comp.ruleBand(w) {
				fillRect(comp.frame, comp.frameW, comp.frameH, b.clip(comp.frameW, comp.frameH), pal.ground)
				dmg = dmg.union(b.clip(comp.frameW, comp.frameH))
			}
		}
		dockTop := (comp.frameH/comp.cellH - dockRows) * comp.cellH
		dock := rect{0, dockTop, comp.frameW, comp.frameH}
		fillRect(comp.frame, comp.frameW, comp.frameH, dock, pal.ground)
		dmg = dmg.union(dock)
	}
	// A tile's content area is only cleared when its client has not drawn
	// yet. Live guest pixels are never touched by a chrome repaint, which
	// is what lets focus changes cost a frame of rules instead of a full
	// recomposite of every window.
	for _, w := range comp.windows {
		if w.area.empty() {
			continue
		}
		if w.top == nil || w.top.surf == nil || w.top.surf.content == nil {
			fillRect(comp.frame, comp.frameW, comp.frameH, w.area, pal.empty)
		}
	}
	// Unfocused rules first, focused last, so the owned rule wins wherever
	// two tiles meet.
	for _, w := range comp.windows {
		if w.area.empty() || w == comp.focus {
			continue
		}
		r.drawWindowRule(w, false)
	}
	if comp.focus != nil && !comp.focus.area.empty() {
		r.drawWindowRule(comp.focus, true)
	}
	for _, w := range comp.windows {
		if !w.area.empty() {
			r.drawBadge(w, w == comp.focus)
		}
	}
	r.drawDock()
	if len(comp.windows) == 0 {
		r.drawSplash()
	}
	return dmg
}

func (r *renderer) drawSplash() {
	comp := r.comp
	a := pickAtlas(comp.cellW, comp.cellH)
	if a == nil {
		return
	}
	lines := []string{
		"wlterm",
		"",
		fmt.Sprintf("%s then d opens the app launcher", comp.prefixName),
		fmt.Sprintf("%s then n opens a window", comp.prefixName),
		fmt.Sprintf("%s then q quits", comp.prefixName),
	}
	y := comp.frameH/2 - len(lines)*a.h/2
	for i, l := range lines {
		c := pal.dim
		if i == 0 {
			c = pal.text
		}
		drawText(comp.frame, comp.frameW, comp.frameH,
			(comp.frameW-len(l)*a.w)/2, y+i*(a.h+2), l, c, a)
	}
}
