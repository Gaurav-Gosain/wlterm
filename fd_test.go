package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// childFds lists the fds a program started from here inherits, as
// "fd -> target" lines. wlterm launches applications (-multi, the launcher,
// the command it was given), and each of them sees exactly this.
func childFds(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", `for f in /proc/$$/fd/*; do echo "${f##*/} -> $(readlink "$f")"; done`).Output()
	if err != nil {
		t.Fatalf("list the child's fds: %v", err)
	}
	return string(out)
}

const (
	fGetSeals = 1034 // F_GET_SEALS
	sealWrite = 0x0008
)

// The keymap memfd is shared by every client. It used to be created without
// MFD_CLOEXEC and without seals, so every application wlterm launched held a
// writable fd to it, and any client could rewrite the keymap every other
// client maps.
func TestKeymapFdIsSealedAndNotInherited(t *testing.T) {
	if _, err := exec.LookPath("xkbcli"); err != nil {
		t.Skip("xkbcli is not installed; the keymap cannot be compiled")
	}
	comp := newSingleComp(400, 200, 10, 20)
	km := comp.keymap()
	if km == nil {
		t.Fatalf("no keymap was made")
	}
	if fds := childFds(t); strings.Contains(fds, "wlterm-keymap") {
		t.Errorf("a launched program inherits the keymap memfd:\n%s", fds)
	}
	seals, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(km.fd), fGetSeals, 0)
	if errno != 0 || seals&sealWrite == 0 {
		t.Errorf("the keymap memfd is not write-sealed (seals %#x, errno %v)", seals, errno)
	}
	if _, err := syscall.Pwrite(km.fd, []byte("x"), 0); err == nil {
		t.Errorf("a write to the shared keymap succeeded; a client could rewrite it for everyone")
	}
}

// A dmabuf wlterm imports is kept open as a dup of the client's fd. A plain
// dup has no FD_CLOEXEC, so one client's GPU buffers leaked into every
// program launched after it and stayed alive as long as that program did.
func TestImportedDmabufFdIsNotInherited(t *testing.T) {
	const w, h = 16, 8
	f, err := os.CreateTemp(t.TempDir(), "dmabuf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(w * h * 4); err != nil {
		t.Fatal(err)
	}
	// The params own their plane fd and close it, so hand them a dup.
	planeFd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	p := &dmabufParams{}
	p.planes[0] = dmabufPlane{fd: planeFd, stride: w * 4, modifier: drmModLinear}
	p.set[0] = true
	comp := newSingleComp(400, 200, 10, 20)
	c := &client{comp: comp, objects: map[uint32]object{}}
	buf, _, msg := p.build(c, w, h, drmFormatXRGB8888, 0)
	if buf == nil {
		t.Fatalf("import refused: %s", msg)
	}
	defer buf.releaseResources()
	p.closeAll()

	kept := buf.dma.fd
	if kept < 0 {
		t.Fatalf("the import kept no fd")
	}
	for _, line := range strings.Split(childFds(t), "\n") {
		if strings.HasPrefix(line, strconv.Itoa(kept)+" -> ") {
			t.Errorf("a launched program inherits the imported dmabuf fd %d: %s", kept, line)
		}
	}
}
