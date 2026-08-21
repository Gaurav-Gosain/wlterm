package main

// Snapshot the composited canvas straight to PNG. This is the capture
// instrument for the multi-surface work: the layered transport puts several
// images on screen at different cell positions, so decoding the escape
// stream back into one picture is no longer a faithful check on its own.
// These are the exact bytes that get transmitted, read out of the canvas.

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"time"
)

func snapshotLoop(comp *compositor, dir string, every time.Duration) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logf("snapshot dir: %v", err)
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	n := 0
	for range t.C {
		comp.mu.Lock()
		w, h := comp.frameW, comp.frameH
		if w == 0 || comp.frame == nil {
			comp.mu.Unlock()
			continue
		}
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		copy(img.Pix, comp.frame)
		comp.mu.Unlock()

		f, err := os.Create(fmt.Sprintf("%s/%06d.png", dir, n))
		if err != nil {
			logf("snapshot: %v", err)
			return
		}
		png.Encode(f, img)
		f.Close()
		n++
	}
}
