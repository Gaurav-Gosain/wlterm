package main

import (
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
//
// The loop runs against a fake clock. Measured on the wall clock this test
// failed 1 run in 3 on a loaded machine (69.2 fps against a floor of 85),
// where it could not tell the fix from the bug.
func TestLoopPacesAtTheTargetRateNotTheTargetGap(t *testing.T) {
	cases := []struct {
		name      string
		fps       int
		frameCost time.Duration
		overshoot time.Duration // how late every sleep wakes up
		want      float64
	}{
		{"exact sleeps", 100, 5 * time.Millisecond, 0, 100},
		// The deadline advances on its own grid, so a sleep that wakes late
		// is absorbed by the next slot instead of lowering the rate.
		{"late wake-ups", 100, 5 * time.Millisecond, time.Millisecond, 100},
		{"120 fps, 2ms frames", 120, 2 * time.Millisecond, 0, 120},
		// A frame that costs more than the interval cannot be paced at all,
		// and must not be repaid as a burst later.
		{"frames slower than the cap", 100, 15 * time.Millisecond, 0, 1000.0 / 15},
		{"uncapped", 0, 5 * time.Millisecond, 0, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const window = time.Second
			clock := time.Unix(1e9, 0)
			comp := &compositor{renderCh: make(chan struct{}, 1)}
			r := newRenderer(comp, "shm", false, nil, tc.fps)
			r.now = func() time.Time { return clock }
			r.sleep = func(d time.Duration) { clock = clock.Add(d + tc.overshoot) }

			start := clock
			frames := 0
			r.frameFn = func() {
				clock = clock.Add(tc.frameCost)
				frames++
				if clock.Sub(start) >= window {
					close(comp.renderCh) // ends loop()
					return
				}
				// Keep a frame permanently owed, so the only thing deciding
				// the rate is the pacing.
				comp.markDirty()
			}
			comp.markDirty()
			r.loop()

			got := float64(frames) / clock.Sub(start).Seconds()
			if got < tc.want*0.99 || got > tc.want*1.01 {
				t.Fatalf("cap of %d fps with a %v frame delivered %.1f fps; want %.1f "+
					"(sleeping the interval after each frame gives %.1f)",
					tc.fps, tc.frameCost, got, tc.want,
					1/((time.Second/time.Duration(max(tc.fps, 1)) + tc.frameCost).Seconds()))
			}
		})
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
