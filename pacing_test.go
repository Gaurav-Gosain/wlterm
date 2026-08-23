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
