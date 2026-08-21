package main

// Terminal setup: raw mode, size probing, mouse + kitty keyboard enable.

import (
	"os"
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
	// Probe: cell size, text area px, DECRQM 1016, kitty kb, DA1 terminator.
	out.WriteString("\x1b[16t\x1b[14t\x1b[?1016$p\x1b[?u\x1b[c")

	// Wait briefly for responses (input parser fills the channels).
	deadline := time.After(600 * time.Millisecond)
	gotKB := false
	gotPixel := false
loop:
	for {
		select {
		case ok := <-p.kittyKBProbe:
			gotKB = ok
			break loop // DA1 response ends the probe window
		case v := <-p.decrqm1016:
			gotPixel = v
		case <-p.gotCell:
		case <-deadline:
			break loop
		}
	}
	// Drain any stragglers.
	for {
		select {
		case v := <-p.decrqm1016:
			gotPixel = v
			continue
		case <-p.gotCell:
			continue
		default:
		}
		break
	}
	p.kittyKB = gotKB
	p.pixelMouse = gotPixel
	if gotKB {
		out.WriteString("\x1b[>11u") // push: disambiguate+event types+all keys
	}

	computeSize(comp, p)
	logf("terminal: %dx%d px, cell %dx%d, kittyKB=%v pixelMouse=%v",
		comp.widthPx, comp.heightPx, comp.cellW, comp.cellH, gotKB, gotPixel)
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
