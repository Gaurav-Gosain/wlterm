package main

import (
	"fmt"
	"testing"
	"time"
)

// testLauncher builds a launcher over a headless canvas with synthetic
// entries, so the damage claims can be asserted rather than eyeballed.
func testLauncher(t *testing.T, n int) *launcher {
	t.Helper()
	initPalette()
	comp := &compositor{
		cellW: 10, cellH: 20, frameW: 1280, frameH: 720,
		renderCh: make(chan struct{}, 1),
	}
	lc := newLauncher(comp, 100)
	comp.lc = lc
	for i := 0; i < n; i++ {
		lc.entries = append(lc.entries, finishEntry(appEntry{
			id:   fmt.Sprintf("app%03d.desktop", i),
			name: fmt.Sprintf("Application %03d", i),
			exec: fmt.Sprintf("app%03d", i),
		}))
	}
	lc.open = true
	lc.filter()
	lc.mark(lcAll)
	return lc
}

// The design claim: moving the selection costs two row rectangles and the
// counter, not the panel. This is the property that made the overlay worth
// putting on its own layer, so it gets a test rather than a comment.
func TestArrowDamageIsTwoRows(t *testing.T) {
	lc := testLauncher(t, 200)
	full := lc.repaint()
	panelPx := lc.pixW * lc.pixH
	if full.pixels() != panelPx {
		t.Fatalf("first paint should cover the panel: %d of %d", full.pixels(), panelPx)
	}

	lc.move(1)
	d := lc.repaint()
	if d.pixels() == 0 {
		t.Fatal("moving the selection must damage something")
	}
	// Two rows plus a counter out of ~24 rows: comfortably under a fifth.
	if d.pixels() > panelPx/5 {
		t.Fatalf("selection move damaged %d px of a %d px panel (%.0f%%); "+
			"it should be two rows and the counter",
			d.pixels(), panelPx, 100*float64(d.pixels())/float64(panelPx))
	}
}

// Rows whose drawn content did not change are not retransmitted, which is
// what keeps a keystroke that empties the list from costing a full panel.
func TestUnchangedRowsAreNotDamaged(t *testing.T) {
	lc := testLauncher(t, 200)
	lc.repaint()

	// Narrow to nothing: every row goes blank.
	lc.query = append(lc.query[:0], "zzzzz"...)
	lc.reFilter()
	first := lc.repaint()
	if len(lc.rows) != 0 {
		t.Fatalf("expected no matches, got %d", len(lc.rows))
	}

	// Another character that also matches nothing changes no row at all, so
	// only the prompt is worth sending.
	lc.query = append(lc.query, 'z')
	lc.reFilter()
	second := lc.repaint()
	if second.pixels() >= first.pixels() {
		t.Fatalf("a keystroke that changes no row should cost less than one "+
			"that blanks the list: %d then %d", first.pixels(), second.pixels())
	}
	if second.pixels() > lc.pixW*lc.pixH/4 {
		t.Fatalf("no row changed, yet %d px of %d were damaged",
			second.pixels(), lc.pixW*lc.pixH)
	}
}

// Dismissal must not damage the canvas at all in the layered transport: the
// panel was never composited into comp.frame, so there is nothing to repair.
func TestDismissLeavesTheCanvasUntouched(t *testing.T) {
	lc := testLauncher(t, 50)
	lc.repaint()
	before := append([]byte(nil), lc.comp.frame...)
	lc.comp.closeLauncher()
	if lc.open {
		t.Fatal("still open")
	}
	if !lc.closing {
		// closing is only set when a placement exists; repaint() alone does
		// not place it, so drive that flag the way the renderer does.
		lc.placed = true
		lc.comp.lc.open = true
		lc.comp.closeLauncher()
	}
	if len(before) != len(lc.comp.frame) {
		t.Fatal("canvas resized")
	}
	for i := range before {
		if before[i] != lc.comp.frame[i] {
			t.Fatalf("the launcher wrote into the canvas at byte %d", i)
		}
	}
}

func TestFrecencyOrdersAnEmptyQuery(t *testing.T) {
	lc := testLauncher(t, 20)
	lc.frec = &frecencyStore{recs: map[string]frecencyRecord{
		"app017.desktop": {count: 9, last: time.Now()},
		"app003.desktop": {count: 2, last: time.Now()},
	}}
	lc.query = lc.query[:0]
	lc.filter()
	if lc.rows[0].e.id != "app017.desktop" {
		t.Fatalf("empty query should be ordered by frecency, got %q", lc.rows[0].e.id)
	}
	if lc.rows[1].e.id != "app003.desktop" {
		t.Fatalf("second row = %q", lc.rows[1].e.id)
	}
}

// Frecency reorders near-ties and never beats a clearly better match.
func TestFrecencyCannotOverrideAClearlyBetterMatch(t *testing.T) {
	initPalette()
	comp := &compositor{cellW: 10, cellH: 20, frameW: 1280, frameH: 720,
		renderCh: make(chan struct{}, 1)}
	lc := newLauncher(comp, 100)
	comp.lc = lc
	lc.entries = []appEntry{
		finishEntry(appEntry{id: "good.desktop", name: "Firefox", exec: "firefox"}),
		finishEntry(appEntry{id: "far.desktop", name: "Thunar File Manager - Recent", exec: "thunar"}),
	}
	// Give the poor match an implausible amount of history.
	lc.frec = &frecencyStore{recs: map[string]frecencyRecord{
		"far.desktop": {count: 5000, last: time.Now()},
	}}
	lc.open = true
	lc.query = append(lc.query[:0], "fire"...)
	lc.filter()
	if lc.rows[0].e.id != "good.desktop" {
		t.Fatalf("frecency overrode a clearly better match: %q first", lc.rows[0].e.id)
	}
}

func TestSecondaryFieldsAreFindableButRankLower(t *testing.T) {
	initPalette()
	comp := &compositor{cellW: 10, cellH: 20, frameW: 1280, frameH: 720,
		renderCh: make(chan struct{}, 1)}
	lc := newLauncher(comp, 100)
	comp.lc = lc
	lc.entries = []appEntry{
		finishEntry(appEntry{id: "a.desktop", name: "Code - OSS", exec: "code-oss",
			keywords: []string{"vscode"}}),
		finishEntry(appEntry{id: "b.desktop", name: "Vscode Themes", exec: "vt"}),
	}
	lc.open = true
	lc.query = append(lc.query[:0], "vscode"...)
	lc.filter()
	if len(lc.rows) != 2 {
		t.Fatalf("both should match, got %d", len(lc.rows))
	}
	// The one whose visible name matches wins; the keyword hit still appears.
	if lc.rows[0].e.id != "b.desktop" {
		t.Fatalf("a hit on the visible name should outrank a keyword hit, got %q", lc.rows[0].e.id)
	}
	if lc.rows[1].via != "vscode" {
		t.Fatalf("the keyword hit should record which field matched, got %q", lc.rows[1].via)
	}
	if lc.rows[1].pos != nil {
		t.Fatal("a hit outside the label must not carry highlight positions")
	}
}
