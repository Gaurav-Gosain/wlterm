package main

import (
	"syscall"
	"unsafe"
)

// memfd creates an anonymous memory file. It is close-on-exec, so a program
// wlterm launches never inherits it, and it allows sealing, so a file shared
// with every client can be made read-only for good.
//
// Go's syscall package has no memfd_create wrapper, and the syscall number
// differs per architecture: sysMemfdCreate comes from the memfd_*.go files.
func memfd(name string) (int, error) {
	const (
		mfdCloexec      = 0x0001
		mfdAllowSealing = 0x0002
	)
	b := append([]byte(name), 0)
	fd, _, errno := syscall.Syscall(sysMemfdCreate,
		uintptr(unsafe.Pointer(&b[0])), mfdCloexec|mfdAllowSealing, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

// sealShared makes a memfd immutable: it can no longer shrink, grow or be
// written, and the seals cannot be removed. A client can still map it
// read-only, which is all a keymap or a format table needs.
func sealShared(fd int) {
	const (
		fAddSeals  = 1033 // F_ADD_SEALS
		sealSeal   = 0x0001
		sealShrink = 0x0002
		sealGrow   = 0x0004
		sealWrite  = 0x0008
	)
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), fAddSeals,
		sealSeal|sealShrink|sealGrow|sealWrite); errno != 0 {
		logf("memfd: seal: %v", errno)
	}
}

// dupCloexec is dup with FD_CLOEXEC set atomically, so the copy never leaks
// into a program started from another goroutine in between.
func dupCloexec(fd int) (int, error) {
	r, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD_CLOEXEC, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r), nil
}
