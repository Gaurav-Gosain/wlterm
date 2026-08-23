package main

// Terminal setup: raw mode, size probing, mouse + kitty keyboard enable.

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func pointerOf(b *byte) uintptr { return uintptr(unsafe.Pointer(b)) }

type termios struct {
	Iflag, Oflag, Cflag, Lflag uint32
	Line                       uint8
	Cc                         [19]uint8
	Ispeed, Ospeed             uint32
}

var savedTermios *termios

func rawMode(fd int) error {
	var t termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}
	saved := t
	savedTermios = &saved
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}
	return nil
}

func restoreTermios(fd int) {
	if savedTermios != nil {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(savedTermios)))
	}
}

type winsize struct {
	Rows, Cols, Xpx, Ypx uint16
}

func getWinsize(fd int) winsize {
	var ws winsize
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	return ws
}

// setupTerminal puts the terminal in raw mode, probes cell/pixel sizes and
// capabilities, and enables mouse + kitty keyboard reporting.
func setupTerminal(comp *compositor, p *inputParser) {
	rawMode(0)
	out := os.Stdout
	// Enter alt screen, hide cursor.
	out.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J\x1b[H")
	// Mouse: button-motion + any-motion + SGR + SGR-pixels.
	out.WriteString("\x1b[?1002h\x1b[?1003h\x1b[?1006h\x1b[?1016h")
	// Probe: cell size, text area px, DECRQM 1016, kitty kb, frame edits,
	// DA1 terminator. DA1 goes last so its reply closes the whole batch.
	out.WriteString("\x1b[16t\x1b[14t\x1b[?1016$p\x1b[?u")
	out.WriteString(frameEditProbe())
	out.WriteString("\x1b[c")

	// Wait briefly for responses (input parser fills the channels).
	deadline := time.After(600 * time.Millisecond)
	gotKB := false
	gotPixel := false
	accepted, refused := false, false
	answer := func(a gfxAnswer) {
		switch a.id {
		case frameProbeAccept:
			accepted = a.ok
		case frameProbeReject:
			refused = !a.ok
		}
	}
loop:
	for {
		select {
		case <-p.da1:
			break loop // asked for last, so its reply closes the window
		case ok := <-p.kittyKBProbe:
			gotKB = ok
		case v := <-p.decrqm1016:
			gotPixel = v
		case a := <-p.gfxAnswers:
			answer(a)
		case <-p.gotCell:
		case <-deadline:
			break loop
		}
	}
	// Drain any stragglers.
	for {
		select {
		case ok := <-p.kittyKBProbe:
			gotKB = ok
			continue
		case v := <-p.decrqm1016:
			gotPixel = v
			continue
		case a := <-p.gfxAnswers:
			answer(a)
			continue
		case <-p.gotCell:
			continue
		default:
		}
		break
	}
	p.kittyKB = gotKB
	p.pixelMouse = gotPixel
	p.frameEdits = accepted && refused
	if gotKB {
		out.WriteString("\x1b[>11u") // push: disambiguate+event types+all keys
	}

	computeSize(comp, p)
	logf("terminal: %dx%d px, cell %dx%d, kittyKB=%v pixelMouse=%v frameEdits=%v",
		comp.widthPx, comp.heightPx, comp.cellW, comp.cellH, gotKB, gotPixel, p.frameEdits)
}

// frameProbeAccept and frameProbeReject are the image ids of the two halves of
// the frame-edit probe. They sit below nextImgID so they can never collide
// with an image wlterm goes on to draw, and both are deleted straight away.
const (
	frameProbeAccept = 91
	frameProbeReject = 92
)

// frameEditProbe returns a test of a=f that a terminal which gets frame edits
// wrong has to fail.
//
// The obvious test -- a one-pixel image with a one-pixel patch -- cannot fail.
// The patch covers the whole image, so a terminal that reads s= and v= as the
// patch rectangle (which is right) and one that reads them as the image's new
// size (which is wrong) behave identically and both answer OK.
//
// So the patch here is smaller than the image, and what is checked afterwards
// is the image's size:
//
//   - id 91 is four pixels wide, is patched one pixel wide, and is then asked
//     to take a four-pixel-wide frame. A terminal that kept the image four
//     wide accepts; one that shrank it to the patch has to refuse.
//   - id 92 is four pixels wide, is never patched, and is asked to take a
//     nine-pixel-wide frame. That is out of bounds, so the answer must be an
//     error. This half catches a relay that acknowledges frame edits and drops
//     them, because such a relay answers OK to everything, including this.
//
// Measured: kitty answers OK then EINVAL; ghostty and WezTerm answer neither,
// which is also the right answer, because neither implements frame edits.
//
// Every payload fits one escape. A payload split across m= continuations is
// not a frame edit at all: a continuation carries no a= key, so the terminal
// routes it to the transmit handler and finishes the load as a new image.
// Neither image is ever placed, so nothing reaches the screen either way.
func frameEditProbe() string {
	px := func(n int) string {
		return base64.StdEncoding.EncodeToString(make([]byte, n*4))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\x1b_Gi=%d,a=t,f=32,s=4,v=1,q=2;%s\x1b\\", frameProbeAccept, px(4))
	fmt.Fprintf(&b, "\x1b_Gi=%d,a=f,r=1,X=1,x=0,y=0,s=1,v=1,f=32,q=2;%s\x1b\\", frameProbeAccept, px(1))
	fmt.Fprintf(&b, "\x1b_Gi=%d,a=f,r=1,X=1,x=0,y=0,s=4,v=1,f=32;%s\x1b\\", frameProbeAccept, px(4))
	fmt.Fprintf(&b, "\x1b_Gi=%d,a=t,f=32,s=4,v=1,q=2;%s\x1b\\", frameProbeReject, px(4))
	fmt.Fprintf(&b, "\x1b_Gi=%d,a=f,r=1,X=1,x=0,y=0,s=9,v=1,f=32;%s\x1b\\", frameProbeReject, px(9))
	fmt.Fprintf(&b, "\x1b_Gi=%d,a=d,d=I,q=2\x1b\\", frameProbeAccept)
	fmt.Fprintf(&b, "\x1b_Gi=%d,a=d,d=I,q=2\x1b\\", frameProbeReject)
	return b.String()
}

func computeSize(comp *compositor, p *inputParser) {
	ws := getWinsize(0)
	cw, ch := 0, 0
	if p.cellW > 0 {
		cw, ch = p.cellW, p.cellH
	} else if ws.Cols > 0 && ws.Xpx > 0 {
		cw = int(ws.Xpx) / int(ws.Cols)
		ch = int(ws.Ypx) / int(ws.Rows)
	}
	comp.cellW, comp.cellH = cw, ch

	w, h := 0, 0
	if p.textW > 0 {
		w, h = p.textW, p.textH
	} else if ws.Xpx > 0 {
		w, h = int(ws.Xpx), int(ws.Ypx)
	} else if cw > 0 {
		w, h = int(ws.Cols)*cw, int(ws.Rows)*ch
	}
	if w == 0 {
		w, h = 1280, 720
	}
	// Leave the bottom cell row free so the terminal has somewhere for its
	// cursor without scrolling the image.
	if ch > 0 && h > ch {
		h -= ch
	}
	comp.widthPx, comp.heightPx = w, h
}

func teardownTerminal() {
	out := os.Stdout
	out.WriteString("\x1b_Ga=d,d=A,q=2\x1b\\") // delete all images
	out.WriteString("\x1b[<u")                 // pop kitty keyboard
	out.WriteString("\x1b[?1016l\x1b[?1006l\x1b[?1003l\x1b[?1002l")
	out.WriteString("\x1b[?25h\x1b[?1049l")
	restoreTermios(0)
}
