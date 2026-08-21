package main

// Window model and tiling. Layout is computed in terminal cells, not pixels,
// for two reasons: it matches tuios (whose panes are cell-shaped), and it
// keeps every window's content rectangle aligned to the cell grid, which is
// what lets each window be transmitted as its own kitty image placed at a
// cell position instead of being baked into one big canvas.

import "math"

// cellRect is a rectangle in cells: origin plus size.
type cellRect struct{ x, y, w, h int }

func (c cellRect) px(cw, ch int) rect {
	return rect{c.x * cw, c.y * ch, (c.x + c.w) * cw, (c.y + c.h) * ch}
}

// window is one xdg_toplevel with a place on screen.
type window struct {
	top *xdgToplevel

	cell cellRect // content area in cells (the divider lives outside it)
	area rect     // content area in pixels

	imgID        uint32 // kitty image id for per-window transmission
	sentW, sentH int    // last size we configured the client with
	sentFocus    bool   // last activated state we configured
	imgW, imgH   int    // size of the image the terminal currently holds
	dmg          rect   // pending damage, canvas pixel coords
	needsFull    bool   // retransmit the whole window image
	placed       bool   // the terminal holds a placement for this window
}

func (w *window) title() string {
	if w.top == nil {
		return ""
	}
	if w.top.title != "" {
		return w.top.title
	}
	if w.top.appID != "" {
		return w.top.appID
	}
	return "window"
}

// ---- layout tree ----

// node is a BSP node: either a leaf holding one window, or a split with two
// children and a ratio.
type node struct {
	win      *window
	vertical bool    // true: a|b side by side; false: a above b
	auto     bool    // direction was not chosen by the user: split the long axis
	ratio    float64 // fraction of the parent given to a
	a, b     *node
}

func (n *node) leaf() bool { return n.win != nil }

// find returns the leaf holding w and the split node above it.
func (n *node) find(w *window) (leaf, parent *node) {
	var walk func(cur, par *node) (*node, *node)
	walk = func(cur, par *node) (*node, *node) {
		if cur == nil {
			return nil, nil
		}
		if cur.leaf() {
			if cur.win == w {
				return cur, par
			}
			return nil, nil
		}
		if l, p := walk(cur.a, cur); l != nil {
			return l, p
		}
		return walk(cur.b, cur)
	}
	return walk(n, nil)
}

func (n *node) leaves(out *[]*window) {
	if n == nil {
		return
	}
	if n.leaf() {
		*out = append(*out, n.win)
		return
	}
	n.a.leaves(out)
	n.b.leaves(out)
}

func (n *node) count() int {
	if n == nil {
		return 0
	}
	if n.leaf() {
		return 1
	}
	return n.a.count() + n.b.count()
}

// insert places w next to the focused window by splitting that leaf in two.
func (n *node) insert(focus *window, w *window, dir splitDir) *node {
	if n == nil {
		return &node{win: w}
	}
	target, _ := n.find(focus)
	if target == nil {
		// Focused window is not in the tree: split the deepest right leaf.
		target = n
		for !target.leaf() {
			target = target.b
		}
	}
	old := target.win
	target.win = nil
	target.ratio = 0.5
	target.auto = dir == splitAuto
	target.vertical = dir == splitVertical
	target.a = &node{win: old}
	target.b = &node{win: w}
	return n
}

// remove deletes w's leaf and collapses its parent.
func (n *node) remove(w *window) *node {
	if n == nil {
		return nil
	}
	if n.win == w {
		return nil
	}
	if n.leaf() {
		return n
	}
	if n.a.win == w {
		return n.b
	}
	if n.b.win == w {
		return n.a
	}
	n.a = n.a.remove(w)
	n.b = n.b.remove(w)
	if n.a == nil {
		return n.b
	}
	if n.b == nil {
		return n.a
	}
	return n
}

type splitDir int

const (
	splitAuto splitDir = iota
	splitVertical
	splitHorizontal
)

type layoutMode int

const (
	layoutBSP layoutMode = iota
	layoutMaster
	layoutModeCount
)

func (m layoutMode) String() string {
	switch m {
	case layoutMaster:
		return "master"
	default:
		return "bsp"
	}
}

// arrange assigns content rectangles to every leaf under n inside r,
// reserving `gap` cells between siblings. That reserved cell is where the
// divider is drawn, which is how tuios does it when panes share borders:
// neighbours meet on one rule rather than each drawing their own.
func (n *node) arrange(r cellRect, gap int) {
	if n == nil {
		return
	}
	if n.leaf() {
		n.win.cell = r
		return
	}
	vert := n.vertical
	if n.auto {
		// Cells are about twice as tall as they are wide, so a "square"
		// region is w == h*2. tuios scales height by that same factor
		// before comparing (cellAspect = 2).
		vert = r.w >= r.h*2
	}
	if vert {
		split := r.x + int(float64(r.w)*n.ratio)
		if split-r.x < minContentW {
			split = r.x + minContentW
		}
		if r.x+r.w-split-gap < minContentW {
			split = r.x + r.w - gap - minContentW
		}
		if split <= r.x || split >= r.x+r.w {
			split = r.x + r.w/2
		}
		n.a.arrange(cellRect{r.x, r.y, split - r.x, r.h}, gap)
		n.b.arrange(cellRect{split + gap, r.y, r.x + r.w - split - gap, r.h}, gap)
	} else {
		split := r.y + int(float64(r.h)*n.ratio)
		if split-r.y < minContentH {
			split = r.y + minContentH
		}
		if r.y+r.h-split-gap < minContentH {
			split = r.y + r.h - gap - minContentH
		}
		if split <= r.y || split >= r.y+r.h {
			split = r.y + r.h/2
		}
		n.a.arrange(cellRect{r.x, r.y, r.w, split - r.y}, gap)
		n.b.arrange(cellRect{r.x, split + gap, r.w, r.y + r.h - split - gap}, gap)
	}
}

// Smallest content a tile may shrink to, in cells.
const (
	minContentW = 8
	minContentH = 3
)

// masterRatio is tuios's default; it clamps the same way.
const (
	masterRatio    = 0.5
	masterRatioMin = 0.3
	masterRatioMax = 0.7
)

// arrangeMaster reproduces tuios's master-stack case analysis: one, two
// (side by side or stacked depending on the region's shape), three (master
// plus two stacked), four (2x2), and a grid beyond that.
func arrangeMaster(wins []*window, r cellRect, ratio float64, gap int) {
	n := len(wins)
	switch {
	case n == 0:
		return
	case n == 1:
		wins[0].cell = r
	case n == 2:
		if r.w >= r.h*2 {
			mw := int(float64(r.w) * ratio)
			wins[0].cell = cellRect{r.x, r.y, mw, r.h}
			wins[1].cell = cellRect{r.x + mw + gap, r.y, r.w - mw - gap, r.h}
		} else {
			mh := int(float64(r.h) * ratio)
			wins[0].cell = cellRect{r.x, r.y, r.w, mh}
			wins[1].cell = cellRect{r.x, r.y + mh + gap, r.w, r.h - mh - gap}
		}
	case n == 3:
		mw := int(float64(r.w) * ratio)
		wins[0].cell = cellRect{r.x, r.y, mw, r.h}
		half := r.h / 2
		sx := r.x + mw + gap
		sw := r.w - mw - gap
		wins[1].cell = cellRect{sx, r.y, sw, half}
		wins[2].cell = cellRect{sx, r.y + half + gap, sw, r.h - half - gap}
	case n == 4:
		hw, hh := r.w/2, r.h/2
		wins[0].cell = cellRect{r.x, r.y, hw, hh}
		wins[1].cell = cellRect{r.x + hw + gap, r.y, r.w - hw - gap, hh}
		wins[2].cell = cellRect{r.x, r.y + hh + gap, hw, r.h - hh - gap}
		wins[3].cell = cellRect{r.x + hw + gap, r.y + hh + gap, r.w - hw - gap, r.h - hh - gap}
	default:
		cols := 2
		if n > 6 {
			cols = 3
		}
		rows := (n + cols - 1) / cols
		cw := (r.w - gap*(cols-1)) / cols
		chh := (r.h - gap*(rows-1)) / rows
		for i, w := range wins {
			cx, cy := i%cols, i/cols
			ww, wh := cw, chh
			if cx == cols-1 {
				ww = r.w - (cw+gap)*cx
			}
			if cy == rows-1 {
				wh = r.h - (chh+gap)*cy
			}
			w.cell = cellRect{r.x + (cw+gap)*cx, r.y + (chh+gap)*cy, ww, wh}
		}
	}
}

// relayout recomputes every content rectangle and marks what must be
// redrawn. Caller holds comp.mu.
func (comp *compositor) relayout() {
	cw, ch := comp.cellW, comp.cellH
	if cw <= 0 || ch <= 0 || comp.widthPx <= 0 {
		return
	}
	cols, rows := comp.widthPx/cw, comp.heightPx/ch
	// One cell of outer margin so the outermost rule has a cell to live in,
	// and two rows at the bottom for the dock: a hairline and a bar.
	region := cellRect{1, 1, cols - 2, rows - 2 - dockRows}
	comp.region = region
	if region.w < minContentW || region.h < minContentH {
		return
	}

	wins := comp.windows
	for _, w := range wins {
		w.cell = cellRect{}
	}
	switch {
	case len(wins) == 0:
	case comp.zoomed != nil && comp.hasWindow(comp.zoomed):
		comp.zoomed.cell = region
	case comp.mode == layoutMaster:
		arrangeMaster(wins, region, comp.masterRatio, tileGap)
	default:
		comp.root.arrange(region, tileGap)
	}

	for _, w := range wins {
		if w.cell.w < 1 || w.cell.h < 1 {
			w.area = rect{}
			continue
		}
		w.area = w.cell.px(cw, ch)
	}
	comp.chromeLayout = true
	comp.configureAll()
	for _, w := range wins {
		if w.area.empty() {
			continue
		}
		w.needsFull = true
		w.dmg = w.area
	}
	comp.markDirty()
}

// tileGap is the one reserved cell between neighbouring tiles that the
// divider is painted into. dockRows is the height of the bottom bar.
const (
	tileGap  = 1
	dockRows = 2
)

func (comp *compositor) hasWindow(w *window) bool {
	for _, x := range comp.windows {
		if x == w {
			return true
		}
	}
	return false
}

// addWindow registers a new toplevel and gives it a tile.
func (comp *compositor) addWindow(t *xdgToplevel) *window {
	w := &window{top: t, imgID: comp.nextImgID}
	comp.nextImgID++
	t.win = w
	comp.windows = append(comp.windows, w)
	comp.root = comp.root.insert(comp.focus, w, comp.nextSplit)
	comp.nextSplit = splitAuto
	comp.zoomed = nil
	comp.setFocus(w)
	comp.relayout()
	logf("window added: %d total", len(comp.windows))
	return w
}

func (comp *compositor) removeWindow(w *window) {
	if w == nil {
		return
	}
	idx := -1
	for i, x := range comp.windows {
		if x == w {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	comp.windows = append(comp.windows[:idx], comp.windows[idx+1:]...)
	comp.root = comp.root.remove(w)
	if comp.zoomed == w {
		comp.zoomed = nil
	}
	comp.freeImage(w)
	if comp.focus == w {
		comp.focus = nil
		if len(comp.windows) > 0 {
			next := idx
			if next >= len(comp.windows) {
				next = len(comp.windows) - 1
			}
			comp.setFocus(comp.windows[next])
		} else {
			comp.setKeyboardFocus(nil)
		}
	}
	comp.relayout()
	logf("window removed: %d left", len(comp.windows))
}

// windowAt returns the window whose content rect contains the pixel, or the
// window whose frame contains it (so clicking a border focuses it).
func (comp *compositor) windowAt(x, y int) *window {
	cw, ch := comp.cellW, comp.cellH
	if cw <= 0 || ch <= 0 {
		return nil
	}
	cx, cy := x/cw, y/ch
	for _, w := range comp.windows {
		o := w.cell
		if o.w == 0 {
			continue
		}
		if cx >= o.x && cx < o.x+o.w && cy >= o.y && cy < o.y+o.h {
			return w
		}
	}
	// In a divider cell: give it to the nearest tile so a click on a rule
	// still selects something sensible.
	var best *window
	bestD := 1 << 30
	for _, w := range comp.windows {
		o := w.cell
		if o.w == 0 {
			continue
		}
		dx := clampDist(cx, o.x, o.x+o.w-1)
		dy := clampDist(cy, o.y, o.y+o.h-1)
		if d := dx*dx + dy*dy; d < bestD {
			bestD, best = d, w
		}
	}
	if bestD <= 4 {
		return best
	}
	return nil
}

func clampDist(v, lo, hi int) int {
	if v < lo {
		return lo - v
	}
	if v > hi {
		return v - hi
	}
	return 0
}

// contentAt reports whether the pixel is inside a window's content area.
func (w *window) contains(x, y int) bool {
	return x >= w.area.x0 && x < w.area.x1 && y >= w.area.y0 && y < w.area.y1
}

func (comp *compositor) cycleFocus(delta int) {
	if len(comp.windows) < 2 {
		return
	}
	idx := 0
	for i, w := range comp.windows {
		if w == comp.focus {
			idx = i
		}
	}
	idx = (idx + delta + len(comp.windows)) % len(comp.windows)
	comp.setFocus(comp.windows[idx])
	if comp.zoomed != nil {
		comp.zoomed = comp.focus
		comp.relayout()
	} else {
		comp.chromeDirty = true
		comp.markDirty()
	}
}

// focusDir moves focus to the nearest window in a compass direction.
func (comp *compositor) focusDir(dx, dy int) {
	cur := comp.focus
	if cur == nil || comp.zoomed != nil {
		comp.cycleFocus(1)
		return
	}
	cx := float64(cur.cell.x) + float64(cur.cell.w)/2
	cy := float64(cur.cell.y) + float64(cur.cell.h)/2
	var best *window
	bestD := math.MaxFloat64
	for _, w := range comp.windows {
		if w == cur || w.cell.w == 0 {
			continue
		}
		wx := float64(w.cell.x) + float64(w.cell.w)/2
		wy := float64(w.cell.y) + float64(w.cell.h)/2
		ddx, ddy := wx-cx, wy-cy
		if dx != 0 && (ddx*float64(dx) <= 0 || math.Abs(ddy) > math.Abs(ddx)*2) {
			continue
		}
		if dy != 0 && (ddy*float64(dy) <= 0 || math.Abs(ddx) > math.Abs(ddy)*2) {
			continue
		}
		d := ddx*ddx + ddy*ddy
		if d < bestD {
			bestD, best = d, w
		}
	}
	if best != nil {
		comp.setFocus(best)
		comp.chromeDirty = true
		comp.markDirty()
	}
}

func (comp *compositor) toggleZoom() {
	if comp.zoomed != nil {
		comp.zoomed = nil
	} else {
		comp.zoomed = comp.focus
	}
	comp.relayout()
}

func (comp *compositor) toggleLayout() {
	comp.mode = (comp.mode + 1) % layoutModeCount
	comp.zoomed = nil
	comp.relayout()
	logf("layout mode: %s", comp.mode)
}

// swapMaster promotes the focused window to the head of the order, which is
// what master-stack calls the master slot.
func (comp *compositor) swapMaster() {
	if comp.focus == nil || len(comp.windows) < 2 {
		return
	}
	for i, w := range comp.windows {
		if w == comp.focus && i > 0 {
			comp.windows[0], comp.windows[i] = comp.windows[i], comp.windows[0]
			break
		}
	}
	comp.relayout()
}

// resizeSplit nudges the ratio of the split that owns the focused window.
func (comp *compositor) resizeSplit(delta float64) {
	if comp.focus == nil {
		return
	}
	if comp.mode == layoutMaster {
		comp.masterRatio += delta
		if comp.masterRatio < masterRatioMin {
			comp.masterRatio = masterRatioMin
		}
		if comp.masterRatio > masterRatioMax {
			comp.masterRatio = masterRatioMax
		}
		comp.relayout()
		return
	}
	_, parent := comp.root.find(comp.focus)
	if parent == nil {
		return
	}
	// Moving "bigger" means growing the side the focused window is on.
	side := 1.0
	if parent.b != nil && parent.b.win == comp.focus {
		side = -1.0
	}
	parent.ratio += delta * side
	if parent.ratio < 0.15 {
		parent.ratio = 0.15
	}
	if parent.ratio > 0.85 {
		parent.ratio = 0.85
	}
	comp.relayout()
}

// freeImage schedules the terminal-side deletion of a closed window's image.
func (comp *compositor) freeImage(w *window) {
	if w.placed {
		comp.freeImages = append(comp.freeImages, w.imgID)
	}
}

// equalize resets every split to half, as tuios's EqualizeRatios does.
func (comp *compositor) equalize() {
	var walk func(n *node)
	walk = func(n *node) {
		if n == nil || n.leaf() {
			return
		}
		n.ratio = 0.5
		walk(n.a)
		walk(n.b)
	}
	walk(comp.root)
	comp.masterRatio = masterRatio
	comp.relayout()
}

// rotateSplit flips the axis of the split that owns the focused window.
func (comp *compositor) rotateSplit() {
	if comp.focus == nil {
		return
	}
	if _, parent := comp.root.find(comp.focus); parent != nil {
		parent.auto = false
		parent.vertical = !parent.vertical
		comp.relayout()
	}
}

// closeFocused asks the focused client to close its window. If it never
// does, the window stays; a tiler does not get to kill a guest for taking
// its time, and a client that ignores close is a client bug.
func (comp *compositor) closeFocused() {
	if comp.focus != nil && comp.focus.top != nil {
		comp.focus.top.close()
	}
}
