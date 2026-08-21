package main

// Per-surface damage as a small set of rectangles rather than one bounding
// box.
//
// A bounding box is fine when a client scribbles in one place, and terrible
// when it damages two corners of its window: the union of two 128x128 boxes
// at opposite ends of a tile is most of the tile. The first scaling run
// measured exactly that -- the animation-frame transport was sending 987 KB
// per frame to move 32 KB of pixels, and lost to simply retransmitting the
// whole tile. A handful of rectangles fixes it without needing a real
// region algebra.

const maxDamageRects = 8

type damageSet struct {
	r [maxDamageRects]rect
	n int
}

func area(r rect) int {
	if r.empty() {
		return 0
	}
	return (r.x1 - r.x0) * (r.y1 - r.y0)
}

func (d *damageSet) empty() bool { return d.n == 0 }
func (d *damageSet) clear()      { d.n = 0 }

func (d *damageSet) set(r rect) {
	d.n = 0
	d.add(r)
}

// bbox is the union of everything, used by transports that can only send one
// rectangle.
func (d *damageSet) bbox() rect {
	out := rect{}
	for i := 0; i < d.n; i++ {
		out = out.union(d.r[i])
	}
	return out
}

func (d *damageSet) pixels() int {
	t := 0
	for i := 0; i < d.n; i++ {
		t += area(d.r[i])
	}
	return t
}

// add folds a rectangle in, merging where merging is free and picking the
// cheapest merge when the set is full.
func (d *damageSet) add(x rect) {
	if x.empty() {
		return
	}
	for i := 0; i < d.n; i++ {
		if waste(d.r[i], x) <= 0 {
			d.r[i] = d.r[i].union(x)
			d.compact()
			return
		}
	}
	if d.n < maxDamageRects {
		d.r[d.n] = x
		d.n++
		return
	}
	best, bi := waste(d.r[0], x), 0
	for i := 1; i < d.n; i++ {
		if w := waste(d.r[i], x); w < best {
			best, bi = w, i
		}
	}
	d.r[bi] = d.r[bi].union(x)
	d.compact()
}

// waste is how many pixels a merge would add beyond the two rectangles
// themselves; zero or less means they touch or overlap.
func waste(a, b rect) int {
	return area(a.union(b)) - area(a) - area(b)
}

// compact merges any pair that has become free to merge after a union.
func (d *damageSet) compact() {
	for i := 0; i < d.n; i++ {
		for j := i + 1; j < d.n; j++ {
			if waste(d.r[i], d.r[j]) <= 0 {
				d.r[i] = d.r[i].union(d.r[j])
				d.r[j] = d.r[d.n-1]
				d.n--
				j--
			}
		}
	}
}

// clipTo drops everything outside r and trims what straddles it.
func (d *damageSet) clipTo(r rect) {
	out := 0
	for i := 0; i < d.n; i++ {
		c := d.r[i]
		if c.x0 < r.x0 {
			c.x0 = r.x0
		}
		if c.y0 < r.y0 {
			c.y0 = r.y0
		}
		if c.x1 > r.x1 {
			c.x1 = r.x1
		}
		if c.y1 > r.y1 {
			c.y1 = r.y1
		}
		if !c.empty() {
			d.r[out] = c
			out++
		}
	}
	d.n = out
}
