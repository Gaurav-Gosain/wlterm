package main

// wlbench: minimal Wayland shm client that animates as fast as frame
// callbacks allow and reports achieved fps. Workloads:
//   -work full: whole surface repainted each frame
//   -work rect: one 128x128 box moves (small damage)
//   -work evil: declares a pool bigger than its file, then truncates a valid
//               pool mid-stream (compositor hardening test)

import (
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var (
	conn *net.UnixConn
	wbuf []byte
)

func put(obj uint32, opcode uint16, args ...any) {
	start := len(wbuf)
	wbuf = append(wbuf, make([]byte, 8)...)
	for _, a := range args {
		switch v := a.(type) {
		case uint32:
			wbuf = binary.LittleEndian.AppendUint32(wbuf, v)
		case int32:
			wbuf = binary.LittleEndian.AppendUint32(wbuf, uint32(v))
		case string:
			n := uint32(len(v) + 1)
			wbuf = binary.LittleEndian.AppendUint32(wbuf, n)
			wbuf = append(wbuf, v...)
			wbuf = append(wbuf, 0)
			for len(wbuf)%4 != 0 {
				wbuf = append(wbuf, 0)
			}
		}
	}
	size := len(wbuf) - start
	binary.LittleEndian.PutUint32(wbuf[start:], obj)
	binary.LittleEndian.PutUint32(wbuf[start+4:], uint32(size)<<16|uint32(opcode))
}

func flush() {
	if len(wbuf) > 0 {
		conn.Write(wbuf)
		wbuf = wbuf[:0]
	}
}

func flushFd(fd int) {
	oob := syscall.UnixRights(fd)
	conn.WriteMsgUnix(wbuf, oob, nil)
	wbuf = wbuf[:0]
}

const (
	regID   = 2
	compID  = 3
	shmID   = 4
	wmID    = 5
	surfID  = 6
	xsurfID = 7
	topID   = 8
	poolID  = 9
	buf0    = 10
	buf1    = 11
	cbBase  = 100
)

func main() {
	width := flag.Int("w", 1280, "width")
	height := flag.Int("h", 720, "height")
	work := flag.String("work", "full", "full|rect|evil")
	dur := flag.Int("dur", 10, "seconds to run")
	flag.Parse()
	W, H := *width, *height

	sock := os.Getenv("XDG_RUNTIME_DIR") + "/" + os.Getenv("WAYLAND_DISPLAY")
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		panic(err)
	}
	conn = c

	put(1, 1, uint32(regID)) // get_registry
	flush()

	names := map[string]uint32{}
	readMsgs(func(obj uint32, op uint16, data []byte) bool {
		if obj == regID && op == 0 {
			name := binary.LittleEndian.Uint32(data)
			ln := binary.LittleEndian.Uint32(data[4:])
			iface := string(data[8 : 8+ln-1])
			names[iface] = name
		}
		return len(names) < 5
	})

	bind := func(iface string, version, id uint32) {
		put(regID, 0, names[iface], iface, version, id)
	}
	bind("wl_compositor", 4, compID)
	bind("wl_shm", 1, shmID)
	bind("xdg_wm_base", 2, wmID)

	if *work == "evil" {
		evil()
		return
	}

	put(compID, 0, uint32(surfID))                // create_surface
	put(wmID, 2, uint32(xsurfID), uint32(surfID)) // get_xdg_surface
	put(xsurfID, 1, uint32(topID))                // get_toplevel
	put(surfID, 6)                                // commit -> configure
	flush()

	poolSize := W * H * 4 * 2
	fd, _ := memfd()
	syscall.Ftruncate(fd, int64(poolSize))
	mem, _ := syscall.Mmap(fd, 0, poolSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	put(shmID, 0, uint32(poolID), int32(poolSize)) // create_pool + fd
	flushFd(fd)
	put(poolID, 0, uint32(buf0), int32(0), int32(W), int32(H), int32(W*4), uint32(1))
	put(poolID, 0, uint32(buf1), int32(W*H*4), int32(W), int32(H), int32(W*4), uint32(1))
	flush()

	readMsgs(func(obj uint32, op uint16, data []byte) bool {
		if obj == xsurfID && op == 0 {
			serial := binary.LittleEndian.Uint32(data)
			put(xsurfID, 4, serial) // ack_configure
			flush()
			return false
		}
		return true
	})

	frames := 0
	cur := 0
	cb := uint32(cbBase)
	start := time.Now()
	deadline := start.Add(time.Duration(*dur) * time.Second)

	draw := func(n int) {
		base := cur * W * H * 4
		px := mem[base : base+W*H*4]
		if *work == "full" {
			v := byte(n)
			for i := 0; i < len(px); i += 4 {
				px[i] = v
				px[i+1] = byte(n >> 2)
				px[i+2] = byte(255 - n)
				px[i+3] = 0xff
			}
			return
		}
		for i := range px {
			px[i] = 0x30
		}
		bx := (n * 7) % (W - 128)
		by := (n * 3) % (H - 128)
		for y := 0; y < 128; y++ {
			row := ((by+y)*W + bx) * 4
			for x := 0; x < 128; x++ {
				px[row+x*4] = 0xff
				px[row+x*4+1] = byte(n)
				px[row+x*4+2] = 0x40
				px[row+x*4+3] = 0xff
			}
		}
	}

	commit := func(n int) {
		draw(n)
		b := uint32(buf0)
		if cur == 1 {
			b = buf1
		}
		put(surfID, 1, b, int32(0), int32(0)) // attach
		if *work == "rect" {
			bx := int32((n * 7) % (W - 128))
			by := int32((n * 3) % (H - 128))
			pbx := int32(((n - 1) * 7) % (W - 128))
			pby := int32(((n - 1) * 3) % (H - 128))
			put(surfID, 2, pbx, pby, int32(128), int32(128))
			put(surfID, 2, bx, by, int32(128), int32(128))
		} else {
			put(surfID, 2, int32(0), int32(0), int32(W), int32(H))
		}
		put(surfID, 3, cb) // frame callback
		put(surfID, 6)     // commit
		flush()
		cur ^= 1
	}

	commit(0)
	n := 1
	for time.Now().Before(deadline) {
		done := false
		readMsgs(func(obj uint32, op uint16, data []byte) bool {
			if obj == cb && op == 0 {
				done = true
				return false
			}
			return true
		})
		if done {
			frames++
			cb++
			commit(n)
			n++
		}
	}
	el := time.Since(start).Seconds()
	fmt.Fprintf(os.Stderr, "BENCH %s %dx%d: %d frames in %.1fs = %.1f fps\n", *work, W, H, frames, el, float64(frames)/el)
}

// evil exercises the compositor's shm hardening. Expected: pool 1 is refused
// with a protocol error; a well-behaved compositor stays alive throughout.
func evil() {
	// Pool declaring 1MB backed by a 4KB file.
	fd, _ := memfd()
	syscall.Ftruncate(fd, 4096)
	put(shmID, 0, uint32(poolID), int32(1<<20))
	flushFd(fd)
	flush()
	// Read until the server error or a short timeout.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := false
	readMsgs(func(obj uint32, op uint16, data []byte) bool {
		if obj == 1 && op == 0 { // wl_display.error
			got = true
			return false
		}
		return true
	})
	if got {
		fmt.Fprintln(os.Stderr, "EVIL: undersized pool correctly refused with protocol error")
	} else {
		fmt.Fprintln(os.Stderr, "EVIL: no protocol error received (BAD)")
	}
}

func readMsgs(f func(obj uint32, op uint16, data []byte) bool) {
	var buf []byte
	rb := make([]byte, 65536)
	for {
		n, err := conn.Read(rb)
		if err != nil {
			os.Exit(0)
		}
		buf = append(buf, rb[:n]...)
		for len(buf) >= 8 {
			obj := binary.LittleEndian.Uint32(buf)
			word := binary.LittleEndian.Uint32(buf[4:])
			size := int(word >> 16)
			if len(buf) < size {
				break
			}
			op := uint16(word & 0xffff)
			cont := f(obj, op, buf[8:size])
			buf = buf[size:]
			if !cont {
				return
			}
		}
	}
}

func memfd() (int, error) {
	name, _ := syscall.BytePtrFromString("wlbench")
	r0, _, errno := syscall.Syscall(319, uintptr(unsafe.Pointer(name)), 1 /*CLOEXEC*/, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r0), nil
}
