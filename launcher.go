package main

// The application launcher: rofi's shape, drawn by wlterm itself.
//
// It is deliberately NOT a Wayland client. A client would need something to
// render it, which is circular in a compositor whose whole job is to render
// clients, and it would occupy a tile when its entire purpose is to sit
// above them. So it is an overlay on the chrome layer, with the same font
// atlas and the same contrast vocabulary as the frame.
//
// The damage discipline is the point.
//
// In per-window mode the panel is its own kitty image at its own cell
// position with z above the tiles, so:
//
//	open     one image transmission of the panel rectangle. No tile moves.
//	type     one animation-frame patch of the rows that changed. No tile.
//	arrow    two row rectangles and the counter. No tile.
//	close    a single `a=d,d=I` delete escape. ZERO pixels, no tile.
//
// Closing costs nothing because the canvas underneath was never touched:
// the panel lives in its own buffer and is never composited into comp.frame,
// so deleting the placement uncovers pixels the terminal already holds.
// That is the same property the per-window transport was built for, and it
// is why the launcher is not drawn as a translucent full-screen scrim: a
// scrim would make every open and every close a full-canvas repaint and
// hand back everything the damage work bought.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// launchRow is one visible result.
type launchRow struct {
	e     *appEntry
	score int
	pos   []int  // match positions into e.label; nil when a secondary field matched
	via   string // the secondary field that matched, "" when the label did
}

// lcChange records what logically changed, which decides what has to be
// retransmitted. The panel is always redrawn in full (it is a few hundred
// kilobytes of memset and glyph blitting, tens of microseconds); only the
// damage rectangles are computed precisely, because those are what cost.
type lcChange uint8

const (
	lcNone lcChange = iota
	lcSel           // selection moved inside the visible page
	lcList          // query or scroll changed: the prompt and every row
	lcAll           // geometry changed: the whole panel
)

type launcher struct {
	comp *compositor

	open      bool
	query     []byte
	entries   []appEntry
	scan      scanResult
	rows      []launchRow
	sel       int
	top       int
	frec      *frecencyStore
	prevSel   int
	prevTop   int
	scannedAt time.Time
	scanning  bool

	// Overlay layer. pix is canvas-shaped only over `area`; it is never
	// blended into comp.frame in per-window mode.
	imgID   uint32
	cell    cellRect
	area    rect
	pix     []byte
	pixW    int
	pixH    int
	dmg     damageSet
	change  lcChange
	placed  bool // the terminal holds a placement for the panel
	closing bool // the placement must be deleted this frame
	imgW    int  // size of the image the terminal currently holds
	imgH    int

	// Per-row content signatures from the last paint. A keystroke changes
	// the rows it changes; rows that come out identical (very often the
	// blank tail of a narrowed list) are not retransmitted at all.
	sig      []uint64
	prevSig  []uint64
	scrollAt rect

	// Geometry, recomputed whenever the pane resizes.
	atlas   *fontAtlas
	pad     int
	border  int
	rowH    int
	promptH int
	listTop int
	visible int

	// Instrumentation, logged on close.
	filterNs  time.Duration
	drawNs    time.Duration
	keystroke int
}

func newLauncher(comp *compositor, imgID uint32) *launcher {
	return &launcher{comp: comp, imgID: imgID}
}

// warm loads the frecency store and scans the applications directories in
// the background, so the first summon is instant.
//
// Neither of those may ever run under comp.mu. The scan walks a few hundred
// files and the frecency store touches the disk; holding the compositor lock
// across either would stall every connected client and the render loop for
// milliseconds, which is precisely the kind of cost the damage work exists
// to avoid paying.
func (lc *launcher) warm() {
	go func() {
		frec := loadFrecency()
		t0 := time.Now()
		res := scanApplications()
		took := time.Since(t0)
		lc.comp.mu.Lock()
		lc.frec = frec
		lc.scan = res
		lc.entries = res.entries
		if lc.open {
			lc.filter()
			lc.mark(lcList)
		}
		lc.comp.mu.Unlock()
		logf("launcher scan in %v: %s", took.Round(time.Microsecond), res.summary())
	}()
}

// rescanStale kicks off a background rescan when the cached list has had
// time to go out of date. The panel opens on what it already has; if the
// scan turns up something different, filter() runs again and the list
// updates in place.
const rescanAfter = 10 * time.Second

func (lc *launcher) rescanStale() {
	if lc.scanning || time.Since(lc.scannedAt) < rescanAfter {
		return
	}
	lc.scanning = true
	go func() {
		res := scanApplications()
		lc.comp.mu.Lock()
		lc.scanning = false
		lc.scannedAt = time.Now()
		changed := len(res.entries) != len(lc.entries)
		lc.scan = res
		lc.entries = res.entries
		if lc.open {
			lc.filter()
			lc.mark(lcList)
		}
		lc.comp.mu.Unlock()
		if changed {
			logf("launcher rescan: %s", res.summary())
		}
	}()
}

// ---- ranking ----

// filter rebuilds the visible list. Ranking matters more than filtering:
// people press Enter without looking, so the right answer has to land first.
//
// The blend is deliberately asymmetric. Frecency is worth at most
// frecencyShare of the best match score in this pass, which is enough to
// reorder near-ties -- the browser you actually use over the one that
// shipped with the distribution -- and never enough to lift a clearly worse
// match past a clearly better one. Capping against the best score rather
// than against each row's own score is what makes that true: a row scoring
// half as much cannot buy its way to the top with history.
//
// With an empty query every match score is zero, so there is nothing to be
// worse than and the list is pure frecency, then alphabetical.
const frecencyShare = 0.15

func (lc *launcher) filter() {
	t0 := time.Now()
	q := string(lc.query)
	now := time.Now()
	rows := lc.rows[:0]
	best := 0

	for i := range lc.entries {
		e := &lc.entries[i]
		var row launchRow
		found := false
		if m, ok := activeMatcher.Match(q, e.label); ok {
			row = launchRow{e: e, score: m.Score, pos: m.Positions}
			found = true
		}
		if q != "" {
			for _, h := range e.hay {
				m, ok := activeMatcher.Match(q, h)
				if !ok {
					continue
				}
				// A hit on something not on screen is a real hit, but a
				// weaker one than a hit on the name the user can see.
				s := m.Score - hiddenFieldPenalty
				if !found || s > row.score {
					row = launchRow{e: e, score: s, via: h}
					found = true
				}
			}
		}
		if !found {
			continue
		}
		if row.score > best {
			best = row.score
		}
		rows = append(rows, row)
	}

	boostCap := 1 << 30
	if q != "" {
		boostCap = int(float64(best) * frecencyShare)
		if boostCap < 0 {
			boostCap = 0
		}
	}
	for i := range rows {
		b := lc.frec.boost(rows[i].e.id, now)
		if b > boostCap {
			b = boostCap
		}
		rows[i].score += b
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].score != rows[j].score {
			return rows[i].score > rows[j].score
		}
		if len(rows[i].e.label) != len(rows[j].e.label) {
			return len(rows[i].e.label) < len(rows[j].e.label)
		}
		return strings.ToLower(rows[i].e.label) < strings.ToLower(rows[j].e.label)
	})
	lc.rows = rows
	if lc.sel >= len(rows) {
		lc.sel = len(rows) - 1
	}
	if lc.sel < 0 {
		lc.sel = 0
	}
	lc.filterNs = time.Since(t0)
}

// hiddenFieldPenalty is what a match costs when it landed on a keyword, a
// generic name or the executable rather than on the label. It has to be
// large enough that a visible match always wins, and small enough that
// typing "vscode" still finds "Code - OSS".
const hiddenFieldPenalty = 12

// ---- geometry ----

// layout sizes the panel in whole cells, which is what lets it be
// transmitted as its own kitty image placed at a cell position rather than
// baked into the canvas.
func (lc *launcher) layout() bool {
	comp := lc.comp
	if comp.cellW <= 0 || comp.cellH <= 0 || comp.frameW <= 0 {
		return false
	}
	cols, rows := comp.frameW/comp.cellW, comp.frameH/comp.cellH
	w := clampInt(cols*3/5, 34, 84)
	if w > cols-2 {
		w = cols - 2
	}
	h := clampInt(rows*2/3, 7, 26)
	if h > rows-2-dockRows {
		h = rows - 2 - dockRows
	}
	if w < 20 || h < 5 {
		return false
	}
	// A third of the way down reads as "in front of" rather than "centred
	// in", which is where every launcher of this shape puts itself.
	y := (rows - dockRows - h) / 3
	if y < 1 {
		y = 1
	}
	cell := cellRect{(cols - w) / 2, y, w, h}
	area := cell.px(comp.cellW, comp.cellH)

	atlas := pickAtlas(comp.cellW, comp.cellH)
	if atlas == nil {
		return false
	}

	changed := cell != lc.cell || lc.atlas != atlas
	lc.cell, lc.area, lc.atlas = cell, area, atlas
	lc.pixW, lc.pixH = area.x1-area.x0, area.y1-area.y0
	if len(lc.pix) < lc.pixW*lc.pixH*4 {
		lc.pix = make([]byte, lc.pixW*lc.pixH*4)
	}

	lc.border = 2
	if comp.cellH >= 24 {
		lc.border = 3
	}
	lc.pad = comp.cellW / 2
	if lc.pad < 4 {
		lc.pad = 4
	}
	lc.rowH = comp.cellH
	if lc.rowH < atlas.h+4 {
		lc.rowH = atlas.h + 4
	}
	lc.promptH = lc.rowH + lc.pad
	lc.listTop = lc.border + lc.pad + lc.promptH + lc.pad
	lc.visible = (lc.pixH - lc.listTop - lc.border - lc.pad) / lc.rowH
	if lc.visible < 1 {
		lc.visible = 1
	}
	return changed
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---- open / close ----

// openLauncher summons the panel. Caller holds comp.mu.
func (comp *compositor) openLauncher() {
	if comp.lc == nil {
		comp.lc = newLauncher(comp, comp.nextImgID)
		comp.nextImgID++
	}
	lc := comp.lc
	if lc.open {
		return
	}
	lc.rescanStale()
	lc.query = lc.query[:0]
	lc.sel, lc.top, lc.prevSel, lc.prevTop = 0, 0, 0, 0
	lc.keystroke = 0
	lc.open = true
	lc.closing = false
	lc.filter()
	lc.mark(lcAll)
	logf("launcher open: %d entries, %d rows", len(lc.entries), len(lc.rows))
	comp.markDirty()
}

// closeLauncher dismisses the panel. Caller holds comp.mu.
func (comp *compositor) closeLauncher() {
	lc := comp.lc
	if lc == nil || !lc.open {
		return
	}
	lc.open = false
	lc.closing = lc.placed
	lc.change = lcNone
	lc.dmg.clear()
	logf("launcher closed after %d keystrokes: filter=%v draw=%v",
		lc.keystroke, lc.filterNs.Round(time.Microsecond), lc.drawNs.Round(time.Microsecond))
	comp.markDirty()
}

// mark records what changed and lets the renderer work out the rectangles.
func (lc *launcher) mark(c lcChange) {
	if c > lc.change {
		lc.change = c
	}
	if lc.comp != nil {
		lc.comp.markDirty()
	}
}

// ---- input ----

// key handles one key press while the panel owns the keyboard. Caller holds
// comp.mu.
func (lc *launcher) key(code, mods uint32) {
	ctrl := mods&4 != 0
	shift := mods&1 != 0
	alt := mods&2 != 0

	switch {
	case code == kcEsc:
		lc.comp.closeLauncher()
		return
	case code == kcEnter:
		lc.activate()
		return
	case code == kcBackspace:
		if ctrl {
			lc.deleteWord()
		} else if n := len(lc.query); n > 0 {
			lc.query = lc.query[:n-1]
			lc.reFilter()
		}
		return
	case code == kcUp, ctrl && code == kcP, code == kcTab && shift:
		lc.move(-1)
		return
	case code == kcDown, ctrl && code == kcN, code == kcTab && !shift:
		lc.move(1)
		return
	case code == kcPgUp:
		lc.move(-lc.visible)
		return
	case code == kcPgDn:
		lc.move(lc.visible)
		return
	case code == kcHome:
		lc.moveTo(0)
		return
	case code == kcEnd:
		lc.moveTo(len(lc.rows) - 1)
		return
	case ctrl && code == kcU:
		lc.query = lc.query[:0]
		lc.reFilter()
		return
	case ctrl && code == kcW:
		lc.deleteWord()
		return
	case ctrl && code == kcC:
		lc.comp.closeLauncher()
		return
	}
	if ctrl || alt {
		return // an unbound chord: swallowed, not typed
	}
	if ch, ok := charForKey(code, shift); ok {
		lc.query = append(lc.query, byte(ch))
		lc.reFilter()
	}
}

func (lc *launcher) reFilter() {
	lc.keystroke++
	lc.sel, lc.top = 0, 0
	lc.filter()
	lc.mark(lcList)
}

func (lc *launcher) deleteWord() {
	q := lc.query
	for len(q) > 0 && q[len(q)-1] == ' ' {
		q = q[:len(q)-1]
	}
	for len(q) > 0 && q[len(q)-1] != ' ' {
		q = q[:len(q)-1]
	}
	lc.query = q
	lc.reFilter()
}

func (lc *launcher) move(delta int) { lc.moveTo(lc.sel + delta) }

func (lc *launcher) moveTo(i int) {
	if len(lc.rows) == 0 {
		return
	}
	// Wrapping is what every tool of this shape does, and it makes "last
	// item" one keystroke from the top.
	n := len(lc.rows)
	if i < 0 {
		i = n - 1
	}
	if i >= n {
		i = 0
	}
	if i == lc.sel {
		return
	}
	lc.prevSel, lc.prevTop = lc.sel, lc.top
	lc.sel = i
	if lc.sel < lc.top {
		lc.top = lc.sel
	}
	if lc.sel >= lc.top+lc.visible {
		lc.top = lc.sel - lc.visible + 1
	}
	if lc.top != lc.prevTop {
		lc.mark(lcList) // the page scrolled: every row is different
	} else {
		lc.mark(lcSel) // two rows and the counter
	}
}

// rowAt maps a canvas pixel to a result index, or -1.
func (lc *launcher) rowAt(x, y int) int {
	if !lc.open || x < lc.area.x0 || x >= lc.area.x1 || y < lc.area.y0 || y >= lc.area.y1 {
		return -1
	}
	ly := y - lc.area.y0 - lc.listTop
	if ly < 0 {
		return -1
	}
	i := lc.top + ly/lc.rowH
	if i >= len(lc.rows) || i >= lc.top+lc.visible {
		return -1
	}
	return i
}

// hover moves the selection to the row under the pointer. Motion that lands
// on no row leaves the selection alone: a launcher that deselects when the
// mouse drifts off the list is a launcher that loses your place.
func (lc *launcher) hover(x, y int) {
	if i := lc.rowAt(x, y); i >= 0 && i != lc.sel {
		lc.moveTo(i)
	}
}

// click launches the row under the pointer, or dismisses the panel when the
// press lands outside it.
func (lc *launcher) click(btn uint32, pressed bool, x, y int) {
	if !pressed || btn != btnLeft {
		return
	}
	if x < lc.area.x0 || x >= lc.area.x1 || y < lc.area.y0 || y >= lc.area.y1 {
		lc.comp.closeLauncher()
		return
	}
	if i := lc.rowAt(x, y); i >= 0 {
		if i != lc.sel {
			lc.moveTo(i)
		}
		lc.activate()
	}
}

// ---- launching ----

// activate runs the selected entry. Caller holds comp.mu.
func (lc *launcher) activate() {
	if lc.sel < 0 || lc.sel >= len(lc.rows) {
		return
	}
	e := lc.rows[lc.sel].e
	comp := lc.comp
	comp.closeLauncher()

	argv, warn, err := parseExec(e.exec, e.path, e.name, e.icon)
	if err != nil {
		logf("launcher: %s has no usable Exec: %v", e.id, err)
		return
	}
	if warn != nil {
		// Spec-correct behaviour that looks like a bug from the outside:
		// say so once, at the moment it matters.
		logf("launcher: %s uses unquoted reserved characters %q in Exec; "+
			"there is no shell, so they are passed through literally: %q",
			e.id, warn.chars, argv)
	}

	// Terminal=true means the program has no window of its own and needs a
	// terminal to be run inside. wlterm has no terminal to give it, so it
	// spawns one: the -term command, which defaults to foot.
	if e.term {
		term := comp.termCmd
		if len(term) == 0 {
			logf("launcher: %s needs a terminal but none is configured", e.id)
			return
		}
		argv = append(append([]string{}, term...), argv...)
	}

	lc.frec.record(e.id)
	req := spawnReq{argv: argv, dir: e.workDir, label: e.label, entryID: e.id}
	logf("launcher: launching %s -> %q (term=%v dir=%q)", e.id, argv, e.term, e.workDir)
	if comp.spawn != nil {
		comp.spawn(req)
	}
}

// spawnReq is everything a child launch needs. It carries an argv, never a
// command string: nothing between a .desktop file and execve is allowed to
// re-interpret it.
type spawnReq struct {
	argv    []string
	dir     string
	label   string
	entryID string
}

func (s spawnReq) name() string {
	if s.label != "" {
		return s.label
	}
	if len(s.argv) > 0 {
		return filepath.Base(s.argv[0])
	}
	return "child"
}

// defaultTermCmd picks the terminal used for Terminal=true entries. foot is
// the obvious choice inside wlterm: it is a wl_shm client, so it renders
// through the same path everything else does.
func defaultTermCmd(pref string) []string {
	cands := []string{pref, "foot", "alacritty", "kitty", "wezterm", "xterm"}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if !resolvesInPath(c) {
			continue
		}
		// -e is the portable "run this argv" flag. foot documents it as
		// accepted-and-ignored, which works because the remaining words are
		// then taken as the command anyway.
		return []string{c, "-e"}
	}
	return nil
}

func (lc *launcher) statusLine() string {
	if len(lc.rows) == 0 {
		return "0/0"
	}
	return fmt.Sprintf("%d/%d", lc.sel+1, len(lc.rows))
}
