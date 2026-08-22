package main

// zwp_linux_dmabuf_v1, the protocol Hyprland's backend refuses to start without.
//
// The whole implementation rests on one measured fact: a dmabuf allocated with
// DRM_FORMAT_MOD_LINEAR on an integrated GPU can be mmap()'d and read by the
// CPU at ~31 GB/s, which is 0.11 ms for a 1280x720 frame. So wlterm advertises
// exactly one modifier -- LINEAR -- and every client that wants to talk to it
// has to allocate something wlterm can simply map, the same way it maps shm.
// No EGL, no GL context, no readback, no cgo.
//
// The cost of that choice is pushed onto the client: a linear render target is
// slower for the GPU than a tiled one. See bench/ for what that trade is worth.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// DRM fourcc codes. Byte order in memory matches wl_shm's ARGB8888/XRGB8888,
// which is what the rest of wlterm already expects.
const (
	drmFormatARGB8888 = 0x34325241 // 'AR24'
	drmFormatXRGB8888 = 0x34325258 // 'XR24'

	drmModLinear = uint64(0)

	// A dmabuf that is not LINEAR is tiled in a vendor-specific layout that
	// only the GPU can decode. wlterm never advertises one, but a client may
	// still try to hand one over.
	drmModInvalid = uint64(0x00ffffffffffffff)
)

// dmabufFormats is the entire format/modifier table wlterm supports.
var dmabufFormats = []struct {
	format   uint32
	modifier uint64
}{
	{drmFormatARGB8888, drmModLinear},
	{drmFormatXRGB8888, drmModLinear},
}

// ---- the DRM node advertised as main_device -------------------------------
//
// Clients allocate on whatever node we name here. That makes the choice
// load-bearing: on this machine an i915 linear dmabuf reads back at 31 GB/s
// while an NVIDIA one reads back at 0.015 GB/s (242 ms per 720p frame, i.e.
// uncached PCIe reads). Prefer a node whose buffers the CPU can actually
// touch, and let -drm override.

var mappableDrivers = []string{"i915", "xe", "amdgpu", "radeon", "msm", "panfrost", "panthor", "v3d", "vc4", "lima"}

// pickRenderNode returns the render node to advertise, and its dev_t.
func pickRenderNode(override string) (string, uint64, bool) {
	try := func(path string) (uint64, bool) {
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			return 0, false
		}
		return uint64(st.Rdev), true
	}
	if override != "" {
		if rdev, ok := try(override); ok {
			return override, rdev, true
		}
		logf("dmabuf: -drm %s unusable", override)
		return "", 0, false
	}
	nodes, _ := filepath.Glob("/dev/dri/renderD*")
	var fallback string
	var fallbackDev uint64
	for _, n := range nodes {
		rdev, ok := try(n)
		if !ok {
			continue
		}
		drv := driverOf(filepath.Base(n))
		for _, want := range mappableDrivers {
			if drv == want {
				logf("dmabuf: main_device %s (driver %s, CPU-mappable)", n, drv)
				return n, rdev, true
			}
		}
		if fallback == "" {
			fallback, fallbackDev = n, rdev
			logf("dmabuf: %s has driver %q, not known CPU-mappable; keeping as fallback", n, drv)
		}
	}
	if fallback != "" {
		return fallback, fallbackDev, true
	}
	return "", 0, false
}

func driverOf(node string) string {
	b, err := os.ReadFile("/sys/class/drm/" + node + "/device/uevent")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "DRIVER=") {
			return strings.TrimPrefix(line, "DRIVER=")
		}
	}
	return ""
}

// ---- format table ---------------------------------------------------------
//
// zwp_linux_dmabuf_feedback_v1 hands clients the format list as a shared
// read-only memfd of 16-byte entries: u32 format, u32 pad, u64 modifier.

// dmabufSync controls the DMA_BUF_IOCTL_SYNC cache maintenance around every
// CPU read. Turning it off is incorrect in general but useful for measuring
// what the cache flush actually costs.
var dmabufSync = true

var (
	formatTableFd   = -1
	formatTableSize int
	dmabufDevice    string
	dmabufDevID     uint64
	dmabufReady     bool
)

func initDmabuf(override string) bool {
	node, rdev, ok := pickRenderNode(override)
	if !ok {
		logf("dmabuf: no DRM render node, protocol not advertised")
		return false
	}
	dmabufDevice, dmabufDevID = node, rdev

	buf := make([]byte, 0, len(dmabufFormats)*16)
	for _, f := range dmabufFormats {
		buf = binary.LittleEndian.AppendUint32(buf, f.format)
		buf = binary.LittleEndian.AppendUint32(buf, 0)
		buf = binary.LittleEndian.AppendUint64(buf, f.modifier)
	}
	fd, err := memfd("wlterm-dmabuf-formats")
	if err != nil {
		logf("dmabuf: memfd_create: %v", err)
		return false
	}
	if _, err := syscall.Write(fd, buf); err != nil {
		logf("dmabuf: write format table: %v", err)
		syscall.Close(fd)
		return false
	}
	// Seal it: the table is shared with every client and must never change,
	// and a sealed memfd cannot be shrunk under us into a SIGBUS.
	const fSeal = 1033 // F_ADD_SEALS
	const sealAll = 0x0001 | 0x0002 | 0x0004 | 0x0008
	syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), fSeal, sealAll)
	formatTableFd, formatTableSize = fd, len(buf)
	dmabufReady = true
	logf("dmabuf: ready, device=%s dev=0x%x formats=%d", node, rdev, len(dmabufFormats))
	return true
}

// Go's syscall package does not export memfd_create; the number is stable ABI.
const sysMemfdCreate = 319 // linux/amd64

func memfd(name string) (int, error) {
	b := append([]byte(name), 0)
	fd, _, errno := syscall.Syscall(sysMemfdCreate,
		uintptr(unsafe.Pointer(&b[0])), uintptr(0x0001|0x0002), 0) // CLOEXEC|ALLOW_SEALING
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

// ---- zwp_linux_dmabuf_v1 --------------------------------------------------

type zwpLinuxDmabuf struct{ version uint32 }

func (zwpLinuxDmabuf) iface() string { return "zwp_linux_dmabuf_v1" }

// sendLegacyFormats serves clients that bound v1-v3, which learn the format
// list from events on the global itself rather than from a feedback object.
func sendLegacyFormats(c *client, id uint32, version uint32) {
	for _, f := range dmabufFormats {
		c.event(id, 0, f.format) // format
		if version >= 3 {
			c.event(id, 1, f.format,
				uint32(f.modifier>>32), uint32(f.modifier&0xffffffff)) // modifier
		}
	}
}

func (d zwpLinuxDmabuf) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		c.deleteID(id)
	case 1: // create_params(new_id params_id)
		pid := r.uint()
		c.objects[pid] = &dmabufParams{}
	case 2: // get_default_feedback(new_id id)
		fid := r.uint()
		c.objects[fid] = dmabufFeedback{}
		sendFeedback(c, fid)
	case 3: // get_surface_feedback(new_id id, object surface)
		fid := r.uint()
		r.uint() // surface: one output, one device, so per-surface == default
		c.objects[fid] = dmabufFeedback{}
		sendFeedback(c, fid)
	}
}

type dmabufFeedback struct{}

func (dmabufFeedback) iface() string { return "zwp_linux_dmabuf_feedback_v1" }
func (dmabufFeedback) handle(c *client, id uint32, opcode uint16, r *argReader) {
	if opcode == 0 { // destroy
		c.deleteID(id)
	}
}

// devArray encodes a dev_t the way the protocol wants it: an array holding the
// raw bytes of a dev_t in native byte order.
func devArray(dev uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, dev)
	return b
}

func sendFeedback(c *client, id uint32) {
	c.event(id, 1, fdArg(formatTableFd), uint32(formatTableSize)) // format_table
	c.event(id, 2, devArray(dmabufDevID))                         // main_device
	// One tranche: everything we support, on the main device, no flags.
	c.event(id, 4, devArray(dmabufDevID)) // tranche_target_device
	idx := make([]byte, 0, len(dmabufFormats)*2)
	for i := range dmabufFormats {
		idx = binary.LittleEndian.AppendUint16(idx, uint16(i))
	}
	c.event(id, 5, idx)       // tranche_formats
	c.event(id, 6, uint32(0)) // tranche_flags
	c.event(id, 3)            // tranche_done
	c.event(id, 0)            // done
}

// ---- zwp_linux_buffer_params_v1 -------------------------------------------

type dmabufPlane struct {
	fd       int
	offset   uint32
	stride   uint32
	modifier uint64
}

type dmabufParams struct {
	planes [4]dmabufPlane
	set    [4]bool
	used   bool
}

func (p *dmabufParams) releaseResources() { p.closeAll() }

func (p *dmabufParams) iface() string { return "zwp_linux_buffer_params_v1" }

const (
	paramsErrAlreadyUsed  = 0
	paramsErrPlaneIdx     = 1
	paramsErrPlaneSet     = 2
	paramsErrIncomplete   = 3
	paramsErrInvalidFmt   = 4
	paramsErrInvalidDims  = 5
	paramsErrOutOfBounds  = 6
	paramsErrInvalidWLBuf = 7
)

func (p *dmabufParams) closeAll() {
	for i := range p.planes {
		if p.set[i] && p.planes[i].fd >= 0 {
			syscall.Close(p.planes[i].fd)
			p.planes[i].fd = -1
		}
		p.set[i] = false
	}
}

func (p *dmabufParams) handle(c *client, id uint32, opcode uint16, r *argReader) {
	switch opcode {
	case 0: // destroy
		p.closeAll()
		c.deleteID(id)
	case 1: // add(fd, plane_idx, offset, stride, modifier_hi, modifier_lo)
		fd := r.fd()
		idx := r.uint()
		off := r.uint()
		stride := r.uint()
		hi := r.uint()
		lo := r.uint()
		if p.used {
			syscall.Close(fd)
			c.protoError(id, paramsErrAlreadyUsed, "params already used")
			return
		}
		if idx >= 4 {
			syscall.Close(fd)
			c.protoError(id, paramsErrPlaneIdx, "plane index out of range")
			return
		}
		if p.set[idx] {
			syscall.Close(fd)
			c.protoError(id, paramsErrPlaneSet, "plane already set")
			return
		}
		p.planes[idx] = dmabufPlane{fd: fd, offset: off, stride: stride, modifier: uint64(hi)<<32 | uint64(lo)}
		p.set[idx] = true
	case 2: // create(width, height, format, flags)
		w, h, format, flags := r.int(), r.int(), r.uint(), r.uint()
		buf, code, msg := p.build(c, w, h, format, flags)
		if buf == nil {
			// create() reports failure as an event, not a fatal error: the
			// client is allowed to try a different format.
			logf("dmabuf create failed: %s (code %d)", msg, code)
			p.closeAll()
			c.event(id, 1) // failed
			return
		}
		// The buffer id is server-allocated for create(); pick from the
		// server id space (0xff000000+) as the protocol requires.
		bid := c.newServerID()
		buf.id = bid
		c.objects[bid] = buf
		p.used = true
		c.event(id, 0, bid) // created(new_id buffer)
	case 3: // create_immed(new_id buffer_id, width, height, format, flags)
		bid := r.uint()
		w, h, format, flags := r.int(), r.int(), r.uint(), r.uint()
		buf, code, msg := p.build(c, w, h, format, flags)
		if buf == nil {
			// create_immed has no failure event: the spec says raise an error.
			logf("dmabuf create_immed failed: %s (code %d)", msg, code)
			p.closeAll()
			c.protoError(id, code, msg)
			return
		}
		buf.id = bid
		c.objects[bid] = buf
		p.used = true
	}
}

// build validates the parameters and maps the dmabuf into our address space.
// Everything here is a refusal path: a compositor must treat a client's
// buffer description as a lie until fstat says otherwise.
func (p *dmabufParams) build(c *client, w, h int32, format, flags uint32) (*wlBuffer, uint32, string) {
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return nil, paramsErrInvalidDims, "invalid dimensions"
	}
	if !p.set[0] {
		return nil, paramsErrIncomplete, "plane 0 missing"
	}
	if p.set[1] || p.set[2] || p.set[3] {
		return nil, paramsErrInvalidFmt, "multi-plane formats not supported"
	}
	opaque := false
	switch format {
	case drmFormatARGB8888:
	case drmFormatXRGB8888:
		opaque = true
	default:
		return nil, paramsErrInvalidFmt, "unsupported format"
	}
	pl := p.planes[0]
	if pl.modifier != drmModLinear {
		// This is the load-bearing refusal. A tiled buffer would need a GPU
		// to decode, which is the entire thing wlterm does not have.
		return nil, paramsErrInvalidFmt, "only DRM_FORMAT_MOD_LINEAR is supported"
	}
	if pl.stride < uint32(w)*4 {
		return nil, paramsErrInvalidDims, "stride too small"
	}
	need := int64(pl.offset) + int64(pl.stride)*int64(h)
	if need > maxPoolSize {
		return nil, paramsErrOutOfBounds, "buffer too large"
	}
	// Same fstat guard as wl_shm: never trust a declared size.
	var st syscall.Stat_t
	if err := syscall.Fstat(pl.fd, &st); err != nil {
		return nil, paramsErrOutOfBounds, "fstat failed"
	}
	if st.Size > 0 && need > st.Size {
		logf("dmabuf: refusing, needs %d bytes, fd holds %d", need, st.Size)
		return nil, paramsErrOutOfBounds, "buffer exceeds dmabuf"
	}
	mapLen := int(need)
	data, err := syscall.Mmap(pl.fd, 0, mapLen, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		// A dmabuf that refuses mmap is one only a GPU can read. Nothing in
		// this design can do anything with it.
		logf("dmabuf: mmap refused: %v", err)
		return nil, paramsErrInvalidFmt, "dmabuf is not CPU-mappable"
	}
	dup, err := syscall.Dup(pl.fd)
	if err != nil {
		dup = -1
	}
	return &wlBuffer{
		dma:    &dmaBuffer{fd: dup, data: data},
		off:    int(pl.offset),
		w:      int(w),
		h:      int(h),
		stride: int(pl.stride),
		opaque: opaque,
		yflip:  flags&1 != 0,
	}, 0, ""
}

// ---- the mapped buffer ----------------------------------------------------

type dmaBuffer struct {
	fd   int
	data []byte
}

func (d *dmaBuffer) release() {
	if d.data != nil {
		syscall.Munmap(d.data)
		d.data = nil
	}
	if d.fd >= 0 {
		syscall.Close(d.fd)
		d.fd = -1
	}
}

// dma-buf cache maintenance. Without this the CPU may read stale cache lines
// for a buffer the GPU just wrote. _IOW('b', 0, __u64) == 0x40086200.
const (
	dmaBufIoctlSync = 0x40086200
	dmaBufSyncRead  = 1 << 0
	dmaBufSyncStart = 0 << 2
	dmaBufSyncEnd   = 1 << 2
)

func (d *dmaBuffer) sync(start bool) {
	if d == nil || d.fd < 0 {
		return
	}
	flags := uint64(dmaBufSyncRead)
	if start {
		flags |= dmaBufSyncStart
	} else {
		flags |= dmaBufSyncEnd
	}
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(d.fd), uintptr(dmaBufIoctlSync),
		uintptr(unsafe.Pointer(&flags)))
}

// checkDmabufBandwidth watches the first few CPU reads of a client's dmabuf and
// complains loudly if they run at device-memory speed instead of RAM speed.
//
// This is not hypothetical. On this machine an NVIDIA dmabuf mmap()s
// successfully and then reads at 0.015 GB/s -- 242 ms for a single 1280x720
// frame -- because the mapping is uncached PCIe BAR space. A compositor that
// silently accepts such a buffer looks hung rather than broken, so say so.
var dmabufSlowChecks int

func checkDmabufBandwidth(b *wlBuffer, ns uint64) {
	bytes := float64(b.w) * float64(b.h) * 4
	// Small buffers (cursors) are all fence wait and no bandwidth; timing them
	// says nothing about the mapping.
	if dmabufSlowChecks >= 3 || ns == 0 || bytes < 1<<18 {
		return
	}
	dmabufSlowChecks++
	gbps := bytes / 1073741824 / (float64(ns) / 1e9)
	if gbps < 1.0 {
		logf("dmabuf: WARNING read of %dx%d ran at %.3f GB/s (%.1f ms/frame). "+
			"This buffer is not in CPU-cacheable memory. Try -drm on another render "+
			"node, or -no-dmabuf to force clients back onto wl_shm.",
			b.w, b.h, gbps, float64(ns)/1e6)
	} else {
		logf("dmabuf: first read of %dx%d at %.1f GB/s (%.2f ms)", b.w, b.h, gbps, float64(ns)/1e6)
	}
}
