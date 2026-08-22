package main

import "testing"

func newSingleComp(w, h, cw, ch int) *compositor {
	return &compositor{
		single:   true,
		clients:  map[*client]bool{},
		renderCh: make(chan struct{}, 1),
		swallow:  map[uint32]bool{},
		widthPx:  w, heightPx: h,
		cellW: cw, cellH: ch,
	}
}

// The whole point of single-app mode: the client is handed the pane, not the
// pane minus a frame, a gap and a dock. At a 10x20 cell the tiler's region
// would be 1260x640.
func TestSingleAppFillsThePane(t *testing.T) {
	comp := newSingleComp(1280, 720, 10, 20)
	w := &window{top: &xdgToplevel{}}
	comp.windows = append(comp.windows, w)
	comp.relayout()

	want := rect{0, 0, 1280, 720}
	if w.area != want {
		t.Fatalf("area = %+v, want %+v", w.area, want)
	}
	// The kitty placement is cell (cell.x+1, cell.y+1), so the origin cell
	// has to be 0,0 for the image to land at pixel 0,0.
	if w.cell.x != 0 || w.cell.y != 0 {
		t.Errorf("cell origin = %d,%d, want 0,0", w.cell.x, w.cell.y)
	}
}

// A resize is the only thing that moves in single-app mode, and it has to
// reach the client's rectangle.
func TestSingleAppFollowsAResize(t *testing.T) {
	comp := newSingleComp(1280, 720, 10, 20)
	w := &window{top: &xdgToplevel{}}
	comp.windows = append(comp.windows, w)
	comp.relayout()

	comp.widthPx, comp.heightPx = 580, 220
	comp.relayout()
	if got, want := w.area, (rect{0, 0, 580, 220}); got != want {
		t.Fatalf("after resize area = %+v, want %+v", got, want)
	}
}

// Extra toplevels stack rather than tile: the newest owns the pane and the
// ones underneath get nothing, which is what a dialog wants.
func TestSingleAppStacksExtraToplevels(t *testing.T) {
	comp := newSingleComp(800, 600, 10, 20)
	first := &window{top: &xdgToplevel{}}
	second := &window{top: &xdgToplevel{}}
	comp.windows = append(comp.windows, first, second)
	comp.relayout()

	if !first.area.empty() {
		t.Errorf("older toplevel still has %+v, want nothing", first.area)
	}
	if got, want := second.area, (rect{0, 0, 800, 600}); got != want {
		t.Errorf("newest toplevel = %+v, want %+v", got, want)
	}
	if comp.focus != second {
		t.Errorf("focus did not follow the newest toplevel")
	}
}

// prefixCode zero is what makes handleKey hand everything to the guest, so
// assert on the behaviour rather than on the field.
func TestSingleAppInterceptsNoKeys(t *testing.T) {
	comp := newSingleComp(800, 600, 10, 20)
	for _, code := range []uint32{kcC, kcN, kcQ, kcSpace, kcBackslash} {
		for _, mods := range []uint32{0, 4} { // plain and ctrl
			if comp.handleKey(code, mods, 1) {
				t.Errorf("swallowed press code=%d mods=%d", code, mods)
			}
			if comp.handleKey(code, mods, 3) {
				t.Errorf("swallowed release code=%d mods=%d", code, mods)
			}
		}
	}
}

// The tiler must still tile, since it is only behind a flag.
func TestMultiModeStillReservesTheFrame(t *testing.T) {
	comp := &compositor{
		clients: map[*client]bool{}, renderCh: make(chan struct{}, 1),
		swallow: map[uint32]bool{}, masterRatio: masterRatio,
		widthPx: 1280, heightPx: 720, cellW: 10, cellH: 20,
	}
	w := &window{top: &xdgToplevel{}}
	comp.windows = append(comp.windows, w)
	comp.root = comp.root.insert(nil, w, splitAuto)
	comp.relayout()

	if w.area.x0 == 0 && w.area.y0 == 0 {
		t.Fatalf("multi mode gave the client the whole pane (%+v); the frame "+
			"and dock are supposed to be reserved around it", w.area)
	}
}
