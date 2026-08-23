package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// A frame cap is a rate, not a gap between frames. The bug this guards was
// sleeping the cap interval measured from the end of the previous frame, so
// every period came out as the interval plus a whole frame's cost: at -fps 100
// with a 5ms frame that is 15ms, i.e. 67fps for a cap of 100.
//
// It stayed hidden for so long because every benchmark ran at -fps 1000, where
// the interval is 1ms and the frame cost swamps the error anyway.
func TestLoopPacesAtTheTargetRateNotTheTargetGap(t *testing.T) {
	const (
		fps       = 100
		frameCost = 5 * time.Millisecond
		window    = 600 * time.Millisecond
	)
	comp := &compositor{renderCh: make(chan struct{}, 1)}
	r := newRenderer(comp, "shm", false, nil, fps)

	var frames atomic.Int64
	r.frameFn = func() {
		time.Sleep(frameCost)
		frames.Add(1)
	}
	go r.loop()

	// Keep a frame permanently owed, so the only thing deciding the rate is
	// the pacing.
	done := make(chan struct{})
	go func() {
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				comp.markDirty()
			}
		}
	}()

	start := time.Now()
	time.Sleep(window)
	close(done)
	got := float64(frames.Load()) / time.Since(start).Seconds()

	// The old behaviour lands at 1/(10ms+5ms) = 67fps. The correct one lands
	// at 100. A floor of 85 separates them with room for a loaded machine,
	// and the ceiling catches a cap that stopped capping.
	if got < 85 || got > fps*1.1 {
		t.Fatalf("cap of %d fps with a %v frame delivered %.1f fps; want ~%d "+
			"(the pre-fix behaviour is ~%.0f)", fps, frameCost, got, fps,
			1/((time.Second/fps + frameCost).Seconds()))
	}
}

// The opaque blit was rewritten to move a pixel as one 32-bit word instead of
// four bytes, because it is the whole cost of a full-pane frame. It has to
// produce exactly what the byte-wise version produced.
func TestOpaqueBlitMatchesTheBytewiseSwizzle(t *testing.T) {
	const w, h = 37, 11 // deliberately not a multiple of anything
	src := make([]byte, w*h*4)
	for i := range src {
		src[i] = byte(i*7 + i/3) // every channel takes many values, alpha included
	}
	want := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			b, g, r := src[i], src[i+1], src[i+2]
			want[i], want[i+1], want[i+2], want[i+3] = r, g, b, 255
		}
	}
	got := make([]byte, w*h*4)
	blitBGRAtoRGBA(got, w, h, src, w, h, 0, 0, false, rect{0, 0, w, h})
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d (pixel %d, channel %d): got %d want %d",
				i, i/4, i%4, got[i], want[i])
		}
	}
}
