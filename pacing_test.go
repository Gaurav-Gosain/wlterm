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

// The opaque blit moves two pixels as one 64-bit word, with a 32-bit tail for
// an odd pixel. It has to produce exactly what the byte-wise swizzle produces,
// at odd widths, odd clip rectangles and odd destination offsets, where the
// tail and the unaligned rows are.
func TestOpaqueBlitMatchesTheBytewiseSwizzle(t *testing.T) {
	const w, h = 37, 11 // deliberately not a multiple of anything
	src := make([]byte, w*h*4)
	for i := range src {
		src[i] = byte(i*7 + i/3) // every channel takes many values, alpha included
	}
	cases := []struct {
		name   string
		dw, dh int
		ox, oy int
		clip   rect
	}{
		{"whole image", w, h, 0, 0, rect{0, 0, w, h}},
		{"odd clip, odd width", w, h, 0, 0, rect{3, 1, 30, 9}},
		{"one pixel wide", w, h, 0, 0, rect{5, 0, 6, h}},
		{"two pixels wide", w, h, 0, 0, rect{5, 0, 7, h}},
		{"odd offset into a larger canvas", 64, 20, 7, 3, rect{0, 0, 64, 20}},
		{"clipped by the canvas edge", 40, 12, 9, 5, rect{0, 0, 40, 12}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := make([]byte, tc.dw*tc.dh*4)
			got := make([]byte, tc.dw*tc.dh*4)
			for i := range want {
				want[i] = byte(i * 13) // pixels outside the clip must stay as they are
				got[i] = want[i]
			}
			for dy := 0; dy < tc.dh; dy++ {
				for dx := 0; dx < tc.dw; dx++ {
					sx, sy := dx-tc.ox, dy-tc.oy
					if sx < 0 || sy < 0 || sx >= w || sy >= h ||
						dx < tc.clip.x0 || dx >= tc.clip.x1 || dy < tc.clip.y0 || dy >= tc.clip.y1 {
						continue
					}
					s, d := (sy*w+sx)*4, (dy*tc.dw+dx)*4
					want[d], want[d+1], want[d+2], want[d+3] = src[s+2], src[s+1], src[s], 255
				}
			}
			blitBGRAtoRGBA(got, tc.dw, tc.dh, src, w, h, tc.ox, tc.oy, false, tc.clip)
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("byte %d (pixel %d,%d, channel %d): got %d want %d",
						i, (i/4)%tc.dw, (i/4)/tc.dw, i%4, got[i], want[i])
				}
			}
		})
	}
}

// BenchmarkOpaqueBlit1080p is one full-pane frame through the opaque path.
func BenchmarkOpaqueBlit1080p(b *testing.B) {
	const w, h = 1920, 1080
	src := make([]byte, w*h*4)
	for i := range src {
		src[i] = byte(i*7 + i/3)
	}
	dst := make([]byte, w*h*4)
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		blitBGRAtoRGBA(dst, w, h, src, w, h, 0, 0, false, rect{0, 0, w, h})
	}
}
