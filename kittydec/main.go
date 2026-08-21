package main

// kittydec: replay a kitty-graphics escape stream and reconstruct what the
// terminal would show. This is wlterm's verification instrument.
//
// The single-surface version only had to track one image. The layered
// transport puts one image per window on screen, each transmitted with its
// own id and placed at its own cell, so the decoder now models what a
// terminal actually models: an image store, a placement list with z-order,
// and a cursor that CSI H moves around.
//
//	kittydec -in stream.bin -out frame.png [-frames DIR] [-cell 9x18] [-size 1440x800]

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"sort"
	"strconv"
	"strings"
)

type stored struct {
	w, h int
	pix  []byte // RGBA
}

type placement struct {
	imgID, plID int
	col, row    int
	z           int
	seq         int
}

func main() {
	in := flag.String("in", "", "captured stream")
	out := flag.String("out", "out.png", "output png (final state)")
	framesDir := flag.String("frames", "", "dump every frame state into DIR as %06d.png")
	cell := flag.String("cell", "9x18", "cell size WxH")
	size := flag.String("size", "", "screen size WxH (default: bounding box of placements)")
	flag.Parse()

	cw, ch := 9, 18
	fmt.Sscanf(*cell, "%dx%d", &cw, &ch)
	scrW, scrH := 0, 0
	fmt.Sscanf(*size, "%dx%d", &scrW, &scrH)

	data, err := os.ReadFile(*in)
	if err != nil {
		panic(err)
	}

	images := map[int]*stored{}
	places := map[[2]int]*placement{}
	curCol, curRow := 1, 1
	seq := 0
	cmds := 0
	frames := 0

	for len(data) > 0 {
		// Advance to the next thing we care about: a graphics escape or a
		// cursor position report.
		g := bytes.Index(data, []byte("\x1b_G"))
		c := indexCUP(data)
		if g < 0 && c < 0 {
			break
		}
		if c >= 0 && (g < 0 || c < g) {
			col, row, n := parseCUP(data[c:])
			curCol, curRow = col, row
			data = data[c+n:]
			continue
		}

		data = data[g+3:]
		end := bytes.Index(data, []byte("\x1b\\"))
		if end < 0 {
			break
		}
		seqBytes := data[:end]
		data = data[end+2:]

		ctrl := string(seqBytes)
		var payload []byte
		if semi := bytes.IndexByte(seqBytes, ';'); semi >= 0 {
			ctrl = string(seqBytes[:semi])
			payload = append([]byte{}, seqBytes[semi+1:]...)
		}
		kv := map[string]string{}
		for _, part := range strings.Split(ctrl, ",") {
			if eq := strings.IndexByte(part, '='); eq > 0 {
				kv[part[:eq]] = part[eq+1:]
			}
		}
		atoi := func(k string, def int) int {
			if v, ok := kv[k]; ok {
				n, err := strconv.Atoi(v)
				if err == nil {
					return n
				}
			}
			return def
		}

		// chunked t=d: keep appending until m=0
		for kv["m"] == "1" {
			j := bytes.Index(data, []byte("\x1b_G"))
			if j < 0 {
				break
			}
			data = data[j+3:]
			e2 := bytes.Index(data, []byte("\x1b\\"))
			if e2 < 0 {
				break
			}
			chunk := data[:e2]
			data = data[e2+2:]
			s2 := bytes.IndexByte(chunk, ';')
			c2 := string(chunk[:s2])
			payload = append(payload, chunk[s2+1:]...)
			if strings.Contains(c2, "m=0") || !strings.Contains(c2, "m=1") {
				kv["m"] = "0"
			}
		}

		id := atoi("i", 1)
		cmds++

		switch kv["a"] {
		case "d":
			switch kv["d"] {
			case "I", "i":
				delete(images, id)
				for k := range places {
					if k[0] == id {
						delete(places, k)
					}
				}
			case "A", "a":
				images = map[int]*stored{}
				places = map[[2]int]*placement{}
			}
		case "T", "t", "f":
			pixels, err := decodePayload(kv["t"], payload)
			if err != nil {
				fmt.Fprintf(os.Stderr, "payload: %v\n", err)
				continue
			}
			w, h := atoi("s", 0), atoi("v", 0)
			if kv["a"] == "f" {
				img := images[id]
				if img == nil {
					continue
				}
				patch(img, pixels, atoi("x", 0), atoi("y", 0), w, h)
			} else {
				img := &stored{w: w, h: h, pix: make([]byte, w*h*4)}
				patch(img, pixels, 0, 0, w, h)
				images[id] = img
				pl := atoi("p", 0)
				seq++
				places[[2]int{id, pl}] = &placement{
					imgID: id, plID: pl, col: curCol, row: curRow, z: atoi("z", 0), seq: seq,
				}
			}
		default:
			continue
		}

		if *framesDir != "" {
			frames++
			if img := compose(images, places, cw, ch, scrW, scrH); img != nil {
				dump(img, fmt.Sprintf("%s/%06d.png", *framesDir, frames))
			}
		}
	}

	img := compose(images, places, cw, ch, scrW, scrH)
	if img == nil {
		fmt.Println("no image state")
		os.Exit(1)
	}
	dump(img, *out)
	fmt.Printf("decoded %d graphics commands, %d images, %d placements, final %dx%d\n",
		cmds, len(images), len(places), img.Rect.Dx(), img.Rect.Dy())
}

// compose draws every placement in z then arrival order, which is what a
// terminal does when several images overlap.
func compose(images map[int]*stored, places map[[2]int]*placement, cw, ch, scrW, scrH int) *image.RGBA {
	if len(places) == 0 {
		return nil
	}
	list := make([]*placement, 0, len(places))
	for _, p := range places {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].z != list[j].z {
			return list[i].z < list[j].z
		}
		return list[i].seq < list[j].seq
	})
	w, h := scrW, scrH
	if w == 0 {
		for _, p := range list {
			img := images[p.imgID]
			if img == nil {
				continue
			}
			if x := (p.col-1)*cw + img.w; x > w {
				w = x
			}
			if y := (p.row-1)*ch + img.h; y > h {
				h = y
			}
		}
	}
	if w == 0 || h == 0 {
		return nil
	}
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	for _, p := range list {
		src := images[p.imgID]
		if src == nil {
			continue
		}
		si := &image.RGBA{Pix: src.pix, Stride: src.w * 4, Rect: image.Rect(0, 0, src.w, src.h)}
		at := image.Pt((p.col-1)*cw, (p.row-1)*ch)
		draw.Draw(canvas, image.Rectangle{at, at.Add(image.Pt(src.w, src.h))}, si, image.Point{}, draw.Src)
	}
	return canvas
}

func decodePayload(transport string, payload []byte) ([]byte, error) {
	switch transport {
	case "s":
		name, _ := base64.StdEncoding.DecodeString(string(payload))
		px, err := os.ReadFile("/dev/shm" + string(name))
		if err != nil {
			return nil, err
		}
		os.Remove("/dev/shm" + string(name)) // act like a terminal
		return px, nil
	default:
		return base64.StdEncoding.AppendDecode(nil, payload)
	}
}

func patch(img *stored, px []byte, x, y, w, h int) {
	for row := 0; row < h; row++ {
		if y+row >= img.h {
			break
		}
		src := row * w * 4
		if src+w*4 > len(px) {
			break
		}
		dst := ((y+row)*img.w + x) * 4
		n := w * 4
		if dst+n > len(img.pix) {
			n = len(img.pix) - dst
		}
		if n <= 0 {
			break
		}
		copy(img.pix[dst:dst+n], px[src:src+n])
	}
}

// indexCUP finds the next CSI row;col H (or CSI H).
func indexCUP(data []byte) int {
	off := 0
	for {
		i := bytes.Index(data[off:], []byte("\x1b["))
		if i < 0 {
			return -1
		}
		i += off
		for j := i + 2; j < len(data) && j < i+24; j++ {
			b := data[j]
			if b >= '0' && b <= '9' || b == ';' {
				continue
			}
			if b == 'H' || b == 'f' {
				return i
			}
			break
		}
		off = i + 2
	}
}

func parseCUP(data []byte) (col, row, n int) {
	end := 2
	for end < len(data) && data[end] != 'H' && data[end] != 'f' {
		end++
	}
	body := string(data[2:end])
	row, col = 1, 1
	if body != "" {
		parts := strings.Split(body, ";")
		if len(parts) > 0 && parts[0] != "" {
			row, _ = strconv.Atoi(parts[0])
		}
		if len(parts) > 1 && parts[1] != "" {
			col, _ = strconv.Atoi(parts[1])
		}
	}
	if row < 1 {
		row = 1
	}
	if col < 1 {
		col = 1
	}
	return col, row, end + 1
}

func dump(img *image.RGBA, path string) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	png.Encode(f, img)
}
