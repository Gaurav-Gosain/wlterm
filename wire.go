package main

// Wayland wire protocol: 32-bit LE words over a unix stream socket.
// Message header: word0 = sender object id, word1 = (size<<16)|opcode.
// File descriptors travel out-of-band via SCM_RIGHTS.

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"syscall"
)

type wlFixed int32 // 24.8 signed fixed point

func fixed(f float64) wlFixed { return wlFixed(f * 256) }

type argReader struct {
	data []byte
	fds  *[]int
}

func (r *argReader) uint() uint32 {
	v := binary.LittleEndian.Uint32(r.data)
	r.data = r.data[4:]
	return v
}
func (r *argReader) int() int32     { return int32(r.uint()) }
func (r *argReader) fixed() wlFixed { return wlFixed(r.uint()) }
func (r *argReader) string() string {
	n := r.uint() // length including NUL
	if n == 0 {
		return ""
	}
	pad := (n + 3) &^ 3
	s := string(r.data[:n-1])
	r.data = r.data[pad:]
	return s
}
func (r *argReader) fd() int {
	fd := (*r.fds)[0]
	*r.fds = (*r.fds)[1:]
	return fd
}

// client is one connected Wayland client.
type client struct {
	comp    *compositor
	conn    *net.UnixConn
	objects map[uint32]object
	wmu     sync.Mutex
	wbuf    []byte
	pendFds []int
	dead    bool

	// pid and sid come from SO_PEERCRED at accept time. They are what lets
	// a launch be verified against the framebuffer rather than against an
	// exit code: a window whose client shares the session of the process we
	// started is ours, and a window that appeared from anywhere else is
	// evidence the launch escaped.
	pid int
	sid int

	nextServerID uint32
}

// newServerID allocates an object id in the server range (0xff000000 and up),
// which the protocol reserves for objects the compositor creates. Needed by
// zwp_linux_buffer_params_v1.create, whose wl_buffer has no client-supplied id.
func (c *client) newServerID() uint32 {
	if c.nextServerID < 0xff000000 {
		c.nextServerID = 0xff000000
	}
	for {
		c.nextServerID++
		if _, taken := c.objects[c.nextServerID]; !taken {
			return c.nextServerID
		}
	}
}

type object interface {
	iface() string
	handle(c *client, id uint32, opcode uint16, r *argReader)
}

func (c *client) get(id uint32) object { return c.objects[id] }

// event queues an event; flushed at flush().
func (c *client) event(obj uint32, opcode uint16, args ...any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	start := len(c.wbuf)
	var hdr [8]byte
	c.wbuf = append(c.wbuf, hdr[:]...)
	for _, a := range args {
		switch v := a.(type) {
		case uint32:
			c.wbuf = binary.LittleEndian.AppendUint32(c.wbuf, v)
		case int32:
			c.wbuf = binary.LittleEndian.AppendUint32(c.wbuf, uint32(v))
		case wlFixed:
			c.wbuf = binary.LittleEndian.AppendUint32(c.wbuf, uint32(v))
		case string:
			n := uint32(len(v) + 1)
			c.wbuf = binary.LittleEndian.AppendUint32(c.wbuf, n)
			c.wbuf = append(c.wbuf, v...)
			c.wbuf = append(c.wbuf, 0)
			for len(c.wbuf)%4 != 0 {
				c.wbuf = append(c.wbuf, 0)
			}
		case []byte: // wl array
			c.wbuf = binary.LittleEndian.AppendUint32(c.wbuf, uint32(len(v)))
			c.wbuf = append(c.wbuf, v...)
			for len(c.wbuf)%4 != 0 {
				c.wbuf = append(c.wbuf, 0)
			}
		case fdArg:
			c.pendFds = append(c.pendFds, int(v))
		default:
			panic(fmt.Sprintf("bad event arg %T", a))
		}
	}
	size := len(c.wbuf) - start
	binary.LittleEndian.PutUint32(c.wbuf[start:], obj)
	binary.LittleEndian.PutUint32(c.wbuf[start+4:], uint32(size)<<16|uint32(opcode))
}

type fdArg int

func (c *client) flush() {
	c.wmu.Lock()
	buf := c.wbuf
	fds := c.pendFds
	c.wbuf = nil
	c.pendFds = nil
	c.wmu.Unlock()
	if len(buf) == 0 {
		return
	}
	if len(fds) > 0 {
		oob := syscall.UnixRights(fds...)
		_, _, err := c.conn.WriteMsgUnix(buf, oob, nil)
		if err != nil {
			c.dead = true
		}
		return
	}
	if _, err := c.conn.Write(buf); err != nil {
		c.dead = true
	}
}

// deleteID tells the client an object id is free for reuse. Required by
// libwayland whenever the server destroys a client-created object.
func (c *client) deleteID(id uint32) {
	delete(c.objects, id)
	c.event(1, 1, id) // wl_display.delete_id
}

func (c *client) protoError(obj uint32, code uint32, msg string) {
	c.event(1, 0, obj, code, msg) // wl_display.error
	c.flush()
	c.dead = true
}

// readLoop reads and dispatches messages until the connection dies.
func (c *client) readLoop() {
	defer func() {
		c.conn.Close()
		c.comp.mu.Lock()
		c.comp.clientGone(c)
		c.comp.mu.Unlock()
	}()
	var buf []byte
	var fds []int
	rbuf := make([]byte, 65536)
	oob := make([]byte, 4096)
	for {
		n, oobn, _, _, err := c.conn.ReadMsgUnix(rbuf, oob)
		if err != nil {
			return
		}
		if oobn > 0 {
			msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
			if err == nil {
				for _, m := range msgs {
					if got, err := syscall.ParseUnixRights(&m); err == nil {
						fds = append(fds, got...)
					}
				}
			}
		}
		buf = append(buf, rbuf[:n]...)
		for len(buf) >= 8 {
			objID := binary.LittleEndian.Uint32(buf)
			word := binary.LittleEndian.Uint32(buf[4:])
			size := int(word >> 16)
			opcode := uint16(word & 0xffff)
			if size < 8 || size > len(rbuf) {
				logf("client sent bad message size %d", size)
				return
			}
			if len(buf) < size {
				break
			}
			r := &argReader{data: buf[8:size], fds: &fds}
			c.comp.mu.Lock()
			if !c.dead {
				if obj := c.get(objID); obj != nil {
					obj.handle(c, objID, opcode, r)
				} else {
					logf("request for unknown object %d op %d", objID, opcode)
				}
			}
			c.comp.mu.Unlock()
			buf = buf[size:]
		}
		c.flush()
		if c.dead {
			return
		}
	}
}
