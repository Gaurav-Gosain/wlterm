package main

// dmaevil: a hostile zwp_linux_dmabuf_v1 client.
//
// Every case here is something a real client could do, deliberately or through
// a bug, and every one of them has to end with the client refused rather than
// the compositor faulting. The buffers are plain memfds: the protocol never
// checks that an fd is a real GPU allocation, which is exactly why the
// compositor has to.
//
//	-case ok         a well-formed LINEAR buffer; must render
//	-case small      declares 1280x720 over a 4 KB fd
//	-case tiled      claims I915_FORMAT_MOD_Y_TILED
//	-case multiplane sends two planes
//	-case huge       claims 100000x100000
//	-case badformat  claims an unsupported fourcc
//	-case truncate   valid at create time, then shrinks the fd under the map
//	-case fdstorm    creates params and never uses them, 2000 times

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
func flushFds(fds ...int) {
	conn.WriteMsgUnix(wbuf, syscall.UnixRights(fds...), nil)
	wbuf = wbuf[:0]
}

func memfd(size int64) int {
	name := append([]byte("dmaevil"), 0)
	fd, _, _ := syscall.Syscall(319, uintptr(unsafe.Pointer(&name[0])), 1, 0)
	syscall.Ftruncate(int(fd), size)
	return int(fd)
}

const (
	regID   = 2
	compID  = 3
	dmaID   = 4
	wmID    = 5
	surfID  = 6
	xsurfID = 7
	topID   = 8
	parmID  = 9
	bufID   = 10
)

const (
	fmtXR24   = 0x34325258
	fmtAR24   = 0x34325241
	modLinear = uint64(0)
	// fourcc_mod_code(INTEL, 2)
	modYTiled = uint64(0x01)<<56 | 2
)

var deadline time.Time

// readMsgs pumps events until fn returns false or the socket dies. It reports
// wl_display.error verbatim, which is the whole point of this program.
func readMsgs(fn func(obj uint32, op uint16, data []byte) bool) {
	rb := make([]byte, 65536)
	oob := make([]byte, 4096)
	var buf []byte
	for {
		conn.SetReadDeadline(deadline)
		n, oobn, _, _, err := conn.ReadMsgUnix(rb, oob)
		if err != nil {
			fmt.Println("  socket closed by compositor:", err)
			return
		}
		_ = oobn
		buf = append(buf, rb[:n]...)
		for len(buf) >= 8 {
			obj := binary.LittleEndian.Uint32(buf)
			word := binary.LittleEndian.Uint32(buf[4:])
			size := int(word >> 16)
			op := uint16(word & 0xffff)
			if size < 8 || len(buf) < size {
				break
			}
			data := buf[8:size]
			if obj == 1 && op == 0 { // wl_display.error
				badObj := binary.LittleEndian.Uint32(data)
				code := binary.LittleEndian.Uint32(data[4:])
				ln := binary.LittleEndian.Uint32(data[8:])
				fmt.Printf("  REFUSED: wl_display.error object=%d code=%d %q\n",
					badObj, code, string(data[12:12+ln-1]))
				return
			}
			if obj == parmID && op == 1 { // zwp_linux_buffer_params_v1.failed
				fmt.Println("  REFUSED: zwp_linux_buffer_params_v1.failed")
				return
			}
			if obj == parmID && op == 0 { // created
				fmt.Printf("  accepted: params.created -> buffer %d\n",
					binary.LittleEndian.Uint32(data))
			}
			if !fn(obj, op, data) {
				return
			}
			buf = buf[size:]
		}
	}
}

func main() {
	kase := flag.String("case", "ok", "ok|small|tiled|multiplane|huge|badformat|truncate|fdstorm")
	flag.Parse()
	W, H := int32(1280), int32(720)

	sock := os.Getenv("XDG_RUNTIME_DIR") + "/" + os.Getenv("WAYLAND_DISPLAY")
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	conn = c
	deadline = time.Now().Add(6 * time.Second)
	fmt.Printf("case %s:\n", *kase)

	put(1, 1, uint32(regID))
	flush()
	names := map[string]uint32{}
	readMsgs(func(obj uint32, op uint16, data []byte) bool {
		if obj == regID && op == 0 {
			name := binary.LittleEndian.Uint32(data)
			ln := binary.LittleEndian.Uint32(data[4:])
			names[string(data[8:8+ln-1])] = name
		}
		_, have := names["zwp_linux_dmabuf_v1"]
		return !(have && len(names) >= 8)
	})
	if _, ok := names["zwp_linux_dmabuf_v1"]; !ok {
		fmt.Println("  compositor does not advertise zwp_linux_dmabuf_v1")
		os.Exit(1)
	}
	put(regID, 0, names["wl_compositor"], "wl_compositor", uint32(4), uint32(compID))
	put(regID, 0, names["zwp_linux_dmabuf_v1"], "zwp_linux_dmabuf_v1", uint32(4), uint32(dmaID))
	put(regID, 0, names["xdg_wm_base"], "xdg_wm_base", uint32(2), uint32(wmID))
	put(compID, 0, uint32(surfID))
	put(wmID, 2, uint32(xsurfID), uint32(surfID))
	put(xsurfID, 1, uint32(topID))
	put(surfID, 6)
	flush()

	if *kase == "fdstorm" {
		// A client that allocates params objects and abandons them. Each one
		// holds an fd; the compositor must not leak them.
		for i := 0; i < 2000; i++ {
			id := uint32(1000 + i)
			put(dmaID, 1, id)
			fd := memfd(4096)
			put(id, 1, uint32(0), uint32(0), uint32(4096), uint32(0), uint32(0))
			flushFds(fd)
			syscall.Close(fd)
		}
		fmt.Println("  sent 2000 abandoned params with fds")
		time.Sleep(time.Second)
		fmt.Printf("  compositor still alive: %v\n", stillAlive())
		return
	}

	stride := uint32(W) * 4
	size := int64(stride) * int64(H)
	format := uint32(fmtXR24)
	mod := modLinear
	planes := 1

	switch *kase {
	case "small":
		size = 4096
	case "tiled":
		mod = modYTiled
	case "multiplane":
		planes = 2
	case "huge":
		W, H = 100000, 100000
		stride = 400000
	case "badformat":
		format = 0x11223344
	}

	fd := memfd(size)
	// paint something recognisable so "ok" is visibly correct
	if m, err := syscall.Mmap(fd, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED); err == nil {
		for i := 0; i+3 < len(m); i += 4 {
			px := i / 4
			m[i], m[i+1], m[i+2], m[i+3] = byte(px%256), byte((px/1280)%256), 0x90, 0xff
		}
		syscall.Munmap(m)
	}

	put(dmaID, 1, uint32(parmID)) // create_params
	flush()
	for p := 0; p < planes; p++ {
		put(parmID, 1, uint32(p), uint32(0), stride,
			uint32(mod>>32), uint32(mod&0xffffffff)) // add
		f2, _ := syscall.Dup(fd)
		flushFds(f2)
		syscall.Close(f2)
	}
	put(parmID, 3, uint32(bufID), W, H, format, uint32(0)) // create_immed
	put(surfID, 1, uint32(bufID), int32(0), int32(0))      // attach
	put(surfID, 9, int32(0), int32(0), W, H)               // damage_buffer
	put(surfID, 6)                                         // commit
	flush()

	if *kase == "truncate" {
		time.Sleep(200 * time.Millisecond)
		fmt.Println("  shrinking the fd under the compositor's mapping")
		syscall.Ftruncate(fd, 0)
		for i := 0; i < 20; i++ {
			put(surfID, 9, int32(0), int32(0), W, H)
			put(surfID, 6)
			flush()
			time.Sleep(50 * time.Millisecond)
		}
	}

	got := false
	readMsgs(func(obj uint32, op uint16, data []byte) bool {
		if obj == xsurfID && op == 0 {
			put(xsurfID, 4, binary.LittleEndian.Uint32(data))
			flush()
		}
		if obj == bufID && op == 0 { // wl_buffer.release
			got = true
			return false
		}
		return true
	})
	if got {
		fmt.Println("  accepted: buffer committed and released")
	}
	fmt.Printf("  compositor still alive: %v\n", stillAlive())
}

func stillAlive() bool {
	sock := os.Getenv("XDG_RUNTIME_DIR") + "/" + os.Getenv("WAYLAND_DISPLAY")
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return false
	}
	c.Close()
	return true
}
