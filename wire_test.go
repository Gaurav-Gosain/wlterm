package main

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests talk to the compositor the way a Wayland client does: real
// messages over a real unix socket, read back as real events. A client that
// misreads an event, or a compositor that dies on a request, fails here in
// the same way it fails in front of foot or vkcube.

// wireEvent is one event as a client reads it off the socket.
type wireEvent struct {
	obj    uint32
	opcode uint16
	args   []byte
}

func (e wireEvent) u32(i int) uint32 { return binary.LittleEndian.Uint32(e.args[i*4:]) }

// connect hands comp a new client on one end of a socketpair and returns the
// other end, which is the client's side. The client's read loop runs until
// the peer goes away.
func connect(t testing.TB, comp *compositor) (*client, *net.UnixConn, chan struct{}) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	conn := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "wire")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatalf("FileConn: %v", err)
		}
		return c.(*net.UnixConn)
	}
	server, peer := conn(fds[0]), conn(fds[1])
	c := &client{comp: comp, conn: server, objects: map[uint32]object{1: wlDisplay{}}}
	comp.mu.Lock()
	comp.clients[c] = true
	comp.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.readLoop()
	}()
	t.Cleanup(func() { peer.Close() })
	return c, peer, done
}

// request encodes one client request. A string argument gets its length,
// its NUL and its padding.
func request(obj uint32, opcode uint16, args ...any) []byte {
	var body []byte
	for _, a := range args {
		switch v := a.(type) {
		case uint32:
			body = binary.LittleEndian.AppendUint32(body, v)
		case string:
			body = binary.LittleEndian.AppendUint32(body, uint32(len(v)+1))
			body = append(body, v...)
			body = append(body, 0)
			for len(body)%4 != 0 {
				body = append(body, 0)
			}
		default:
			panic("request: unsupported argument type")
		}
	}
	msg := binary.LittleEndian.AppendUint32(nil, obj)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(8+len(body))<<16|uint32(opcode))
	return append(msg, body...)
}

func send(t testing.TB, peer *net.UnixConn, msgs ...[]byte) {
	t.Helper()
	for _, m := range msgs {
		if _, err := peer.Write(m); err != nil {
			t.Fatalf("write request: %v", err)
		}
	}
}

// readUntil reads events until one satisfies stop, the connection closes, or
// the deadline passes. It returns everything read, and whether stop matched.
func readUntil(t testing.TB, peer *net.UnixConn, d time.Duration, stop func(wireEvent) bool) ([]wireEvent, bool) {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(d))
	defer peer.SetReadDeadline(time.Time{})
	var evs []wireEvent
	var buf []byte
	rb := make([]byte, 65536)
	oob := make([]byte, 1024)
	for {
		for len(buf) >= 8 {
			size := int(binary.LittleEndian.Uint32(buf[4:]) >> 16)
			if size < 8 || len(buf) < size {
				break
			}
			ev := wireEvent{
				obj:    binary.LittleEndian.Uint32(buf),
				opcode: uint16(binary.LittleEndian.Uint32(buf[4:])),
				args:   append([]byte(nil), buf[8:size]...),
			}
			buf = buf[size:]
			evs = append(evs, ev)
			if stop != nil && stop(ev) {
				return evs, true
			}
		}
		n, oobn, _, _, err := peer.ReadMsgUnix(rb, oob)
		if oobn > 0 {
			// The keymap and the dmabuf format table arrive as fds. Close
			// them so a long fuzz run does not run out.
			if msgs, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil {
				for _, m := range msgs {
					if got, err := syscall.ParseUnixRights(&m); err == nil {
						for _, fd := range got {
							syscall.Close(fd)
						}
					}
				}
			}
		}
		buf = append(buf, rb[:n]...)
		if err != nil {
			return evs, false
		}
	}
}

// roundTrip sends wl_display.sync and waits for its done, so every event the
// requests before it caused has been read.
func roundTrip(t testing.TB, peer *net.UnixConn, cb uint32) []wireEvent {
	t.Helper()
	send(t, peer, request(1, 0, cb))
	evs, ok := readUntil(t, peer, 5*time.Second, func(e wireEvent) bool {
		return e.obj == cb && e.opcode == 0
	})
	if !ok {
		t.Fatalf("no wl_callback.done for sync %d within 5s; the compositor is stuck", cb)
	}
	return evs
}

// mapSurfaceFor gives the client a toplevel that covers the pane, without
// going through xdg-shell, so the pointer has something of the client's to
// land on.
func mapSurfaceFor(comp *compositor, c *client, id uint32) {
	comp.mu.Lock()
	defer comp.mu.Unlock()
	surf := &wlSurface{id: id, client: c, w: comp.widthPx, h: comp.heightPx, mapped: true,
		content: make([]byte, comp.widthPx*comp.heightPx*4)}
	comp.windows = append(comp.windows, &window{top: &xdgToplevel{surf: surf, client: c}})
	comp.relayout()
}

// The vkcube crash. wl_pointer.frame arrived in wl_seat version 5, and a
// client that bound an older seat has no listener slot for it. libwayland
// aborts on such an event rather than skipping it: vkcube binds wl_seat v1
// and died with "listener function for opcode 5 of wl_pointer is NULL" on the
// first pointer motion. The same rule already holds for repeat_info.
func TestPointerFrameFollowsTheBoundSeatVersion(t *testing.T) {
	const (
		registry = 2
		seat     = 3
		pointer  = 4
		surface  = 20
	)
	for _, version := range []uint32{1, 4, 5} {
		t.Run("wl_seat v"+string(rune('0'+version)), func(t *testing.T) {
			comp := newSingleComp(400, 200, 10, 20)
			c, peer, _ := connect(t, comp)
			send(t, peer,
				request(1, 1, uint32(registry)),
				request(registry, 0, uint32(3), "wl_seat", version, uint32(seat)),
				request(seat, 0, uint32(pointer)))
			roundTrip(t, peer, 10)
			mapSurfaceFor(comp, c, surface)

			comp.mu.Lock()
			comp.pointerMotion(50, 50) // enter, then motion
			comp.pointerButton(btnLeft, true)
			comp.pointerButton(btnLeft, false)
			comp.pointerAxis(true, 15)
			comp.pointerMotion(500, 500) // off the window: leave
			comp.mu.Unlock()
			evs := roundTrip(t, peer, 11)

			seen := map[uint16]int{}
			for _, e := range evs {
				if e.obj == pointer {
					seen[e.opcode]++
				}
			}
			for _, op := range []uint16{0, 1, 2, 3, 4} { // enter leave motion button axis
				if seen[op] == 0 {
					t.Errorf("no wl_pointer event with opcode %d; got %v", op, seen)
				}
			}
			if version < 5 && seen[5] > 0 {
				t.Errorf("sent %d wl_pointer.frame events to a client that bound wl_seat v%d; "+
					"frame needs v5 and libwayland aborts the client", seen[5], version)
			}
			if version >= 5 && seen[5] == 0 {
				t.Errorf("a wl_seat v%d client got no wl_pointer.frame", version)
			}
		})
	}
}

// One malformed request hung the whole compositor. wl_display.get_registry
// with its argument missing panicked in the argument reader while the global
// lock was held, and the reader's deferred cleanup then took the same lock on
// the same goroutine. Nothing was printed, the process stayed up, and every
// client and the renderer stopped for good. The offender has to get a
// protocol error and lose its connection, and nobody else may notice.
func TestMalformedRequestDisconnectsOnlyThatClient(t *testing.T) {
	cases := []struct {
		name string
		msgs [][]byte
	}{
		{"get_registry with no new_id", [][]byte{request(1, 1)}},
		{"bind with a string longer than the message", [][]byte{
			request(1, 1, uint32(2)),
			func() []byte {
				m := request(2, 0, uint32(3), "wl_seat", uint32(1), uint32(3))
				binary.LittleEndian.PutUint32(m[12:], 4096) // the string's length
				return m
			}(),
		}},
		{"bind with a string length that wraps the padding", [][]byte{
			request(1, 1, uint32(2)),
			func() []byte {
				m := request(2, 0, uint32(3), "wl_seat", uint32(1), uint32(3))
				binary.LittleEndian.PutUint32(m[12:], 0xffffffff)
				return m
			}(),
		}},
		{"create_pool with no fd", [][]byte{
			request(1, 1, uint32(2)),
			request(2, 0, uint32(2), "wl_shm", uint32(1), uint32(3)),
			request(3, 0, uint32(4), uint32(4096)), // new_id, (fd missing), size
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			comp := newSingleComp(400, 200, 10, 20)
			_, good, _ := connect(t, comp)
			_, bad, badDone := connect(t, comp)

			send(t, bad, tc.msgs...)
			evs, ok := readUntil(t, bad, 5*time.Second, func(e wireEvent) bool {
				return e.obj == 1 && e.opcode == 0 // wl_display.error
			})
			if !ok {
				t.Fatalf("the malformed client got no wl_display.error within 5s (events: %d)", len(evs))
			}
			select {
			case <-badDone:
			case <-time.After(5 * time.Second):
				t.Fatalf("the malformed client's connection was not closed")
			}

			// The other client is untouched and still served.
			roundTrip(t, good, 50)
			if !comp.mu.TryLock() {
				t.Fatalf("the compositor lock is still held after the bad client left")
			}
			comp.mu.Unlock()
		})
	}
}

// FuzzDispatch feeds arbitrary request streams to one client. Whatever it
// sends, the read loop must return when the stream ends, the compositor lock
// must be free afterwards, and nothing may panic out of the loop. The seeds
// reach the registry, the seat, wl_shm and xdg_wm_base so the mutator starts
// from requests that land on real objects.
func FuzzDispatch(f *testing.F) {
	join := func(msgs ...[]byte) []byte {
		var b []byte
		for _, m := range msgs {
			b = append(b, m...)
		}
		return b
	}
	bindAll := join(
		request(1, 1, uint32(2)),
		request(2, 0, uint32(1), "wl_compositor", uint32(4), uint32(3)),
		request(2, 0, uint32(2), "wl_shm", uint32(1), uint32(4)),
		request(2, 0, uint32(3), "wl_seat", uint32(5), uint32(5)),
		request(2, 0, uint32(4), "wl_output", uint32(3), uint32(6)),
		request(2, 0, uint32(5), "xdg_wm_base", uint32(2), uint32(7)),
		request(2, 0, uint32(6), "wl_subcompositor", uint32(1), uint32(8)),
	)
	f.Add(request(1, 1))                          // get_registry, no argument
	f.Add(request(1, 0))                          // sync, no argument
	f.Add(append(request(1, 1, uint32(2)), 0, 0)) // a trailing partial header
	f.Add(join(bindAll, request(4, 0, uint32(9), uint32(4096))))
	f.Add(join(bindAll, request(5, 0, uint32(9)), request(5, 1, uint32(10))))
	f.Add(join(bindAll,
		request(3, 0, uint32(9)),             // create_surface
		request(7, 2, uint32(10), uint32(9)), // get_xdg_surface
		request(10, 1, uint32(11)),           // get_toplevel
		request(9, 6),                        // commit
		request(11, 2, "title"),
	))
	f.Add(join(bindAll,
		request(3, 0, uint32(9)),
		request(3, 0, uint32(10)),
		request(8, 1, uint32(11), uint32(9), uint32(10)), // get_subsurface
		request(11, 1, uint32(1), uint32(2)),             // set_position
	))

	f.Fuzz(func(t *testing.T, stream []byte) {
		comp := newSingleComp(400, 200, 10, 20)
		_, peer, done := connect(t, comp)
		go io.Copy(io.Discard, peer) // never let the compositor block on a full socket
		if _, err := peer.Write(stream); err != nil && !errors.Is(err, syscall.EPIPE) &&
			!strings.Contains(err.Error(), "connection reset") {
			t.Fatalf("write: %v", err)
		}
		peer.CloseWrite()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("the read loop did not return after the stream ended; the compositor is stuck")
		}
		if !comp.mu.TryLock() {
			t.Fatalf("the compositor lock is still held after the client left")
		}
		comp.mu.Unlock()
	})
}
