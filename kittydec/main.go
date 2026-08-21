package main

// kittydec: replay a kitty-graphics escape stream (as emitted by wlterm)
// and write the image state as PNGs. Verification instrument; with -frames
// it dumps every graphics command's resulting state for video assembly.
//
//	kittydec -in stream.bin -out frame.png [-frames DIR]

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"strconv"
	"strings"
)

func main() {
	in := flag.String("in", "", "captured stream")
	out := flag.String("out", "out.png", "output png (final state)")
	framesDir := flag.String("frames", "", "dump every frame state into DIR as %06d.png")
	flag.Parse()

	data, err := os.ReadFile(*in)
	if err != nil {
		panic(err)
	}

	var img *image.RGBA
	frames := 0

	for {
		i := bytes.Index(data, []byte("\x1b_G"))
		if i < 0 {
			break
		}
		data = data[i+3:]
		end := bytes.Index(data, []byte("\x1b\\"))
		if end < 0 {
			break
		}
		seq := data[:end]
		data = data[end+2:]

		semi := bytes.IndexByte(seq, ';')
		ctrl := string(seq)
		payload := []byte{}
		if semi >= 0 {
			ctrl = string(seq[:semi])
			payload = seq[semi+1:]
		}
		kv := map[string]string{}
		for _, part := range strings.Split(ctrl, ",") {
			if eq := strings.IndexByte(part, '='); eq > 0 {
				kv[part[:eq]] = part[eq+1:]
			}
		}
		atoi := func(k string, def int) int {
			if v, ok := kv[k]; ok {
				n, _ := strconv.Atoi(v)
				return n
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
			chunk := data[:e2]
			data = data[e2+2:]
			s2 := bytes.IndexByte(chunk, ';')
			c2 := string(chunk[:s2])
			payload = append(payload, chunk[s2+1:]...)
			if strings.Contains(c2, "m=0") || !strings.Contains(c2, "m=1") {
				kv["m"] = "0"
			}
		}

		var pixels []byte
		switch kv["t"] {
		case "s":
			name, _ := base64.StdEncoding.DecodeString(string(payload))
			pixels, err = os.ReadFile("/dev/shm" + string(name))
			if err != nil {
				fmt.Fprintf(os.Stderr, "shm read: %v\n", err)
				continue
			}
			os.Remove("/dev/shm" + string(name)) // act like a terminal
		case "d", "":
			pixels, err = base64.StdEncoding.AppendDecode(nil, payload)
			if err != nil {
				fmt.Fprintf(os.Stderr, "b64: %v\n", err)
				continue
			}
		}

		w := atoi("s", 0)
		h := atoi("v", 0)
		switch kv["a"] {
		case "T", "t":
			img = image.NewRGBA(image.Rect(0, 0, w, h))
			copyPixels(img, pixels, 0, 0, w, h)
		case "f":
			if img == nil {
				continue
			}
			copyPixels(img, pixels, atoi("x", 0), atoi("y", 0), w, h)
		default:
			continue
		}
		frames++
		if *framesDir != "" && img != nil {
			dump(img, fmt.Sprintf("%s/%06d.png", *framesDir, frames))
		}
	}

	if img == nil {
		fmt.Println("no image state")
		os.Exit(1)
	}
	dump(img, *out)
	fmt.Printf("decoded %d graphics commands, final %dx%d\n", frames, img.Rect.Dx(), img.Rect.Dy())
}

func copyPixels(img *image.RGBA, px []byte, x, y, w, h int) {
	for row := 0; row < h; row++ {
		if y+row >= img.Rect.Dy() {
			break
		}
		src := row * w * 4
		if src+w*4 > len(px) {
			break
		}
		dst := img.PixOffset(x, y+row)
		copy(img.Pix[dst:dst+w*4], px[src:src+w*4])
	}
}

func dump(img *image.RGBA, path string) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	png.Encode(f, img)
}
