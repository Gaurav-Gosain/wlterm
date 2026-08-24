package main

import (
	"encoding/binary"
	"testing"
)

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

// Extra toplevels float rather than tile: the application keeps the pane and
// the dialog it opened sits on top of it, centred at its own size. A browser
// that disappears the moment it asks for a keyring password is not usable.
func TestSingleAppFloatsADialogOverTheApp(t *testing.T) {
	comp := newSingleComp(800, 600, 10, 20)
	first := &window{top: &xdgToplevel{}}
	dialog := &window{top: &xdgToplevel{surf: &wlSurface{
		content: make([]byte, 200*100*4), w: 200, h: 100,
	}}}
	comp.windows = append(comp.windows, first, dialog)
	comp.relayout()

	if got, want := first.area, (rect{0, 0, 800, 600}); got != want {
		t.Errorf("the application lost the pane: %+v, want %+v", got, want)
	}
	if !dialog.float {
		t.Errorf("the second toplevel was not marked as floating")
	}
	// 200x100 at a 10x20 cell is 20x5 cells, centred in 80x30 cells: cell
	// (30,12), so pixel (300,240).
	if got, want := dialog.area, (rect{300, 240, 500, 340}); got != want {
		t.Errorf("dialog = %+v, want %+v", got, want)
	}
	if comp.focus != dialog {
		t.Errorf("focus did not follow the dialog")
	}
	// A click inside the dialog belongs to the dialog, not to the pane it
	// covers.
	if got := comp.windowAt(400, 300); got != dialog {
		t.Errorf("click inside the dialog hit the wrong window")
	}
	if got := comp.windowAt(20, 20); got != first {
		t.Errorf("click outside the dialog did not reach the application")
	}
}

// A dialog with nothing drawn yet has no size to centre, so it is not placed
// and the application underneath stays visible.
func TestSingleAppDoesNotPlaceAnUndrawnDialog(t *testing.T) {
	comp := newSingleComp(800, 600, 10, 20)
	first := &window{top: &xdgToplevel{}}
	dialog := &window{top: &xdgToplevel{surf: &wlSurface{}}}
	comp.windows = append(comp.windows, first, dialog)
	comp.relayout()

	if got, want := first.area, (rect{0, 0, 800, 600}); got != want {
		t.Errorf("the application lost the pane: %+v, want %+v", got, want)
	}
	if !dialog.area.empty() {
		t.Errorf("an undrawn dialog was placed at %+v", dialog.area)
	}
}

// The fullscreen state is what makes a browser hide its own tab strip and
// address bar, so it is off unless it is asked for.
func TestSingleAppDoesNotClaimFullscreen(t *testing.T) {
	comp := newSingleComp(800, 600, 10, 20)
	w := &window{top: &xdgToplevel{}}
	w.top.win = w
	comp.windows = append(comp.windows, w)
	comp.relayout()

	has := func(states []byte, want uint32) bool {
		for i := 0; i+4 <= len(states); i += 4 {
			if binary.LittleEndian.Uint32(states[i:]) == want {
				return true
			}
		}
		return false
	}
	st := toplevelStates(comp, w.top)
	if has(st, 2) {
		t.Errorf("fullscreen was sent without -fullscreen")
	}
	if !has(st, 1) {
		t.Errorf("maximized was not sent")
	}
	comp.fullscreen = true
	if !has(toplevelStates(comp, w.top), 2) {
		t.Errorf("-fullscreen did not send the fullscreen state")
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
