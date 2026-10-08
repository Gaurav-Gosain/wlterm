package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

// memfd_create has a different syscall number on every architecture. It was
// hard-coded as 319, the amd64 number, so an arm64 build compiled, started,
// and then had no keymap and no dmabuf format table: 319 is not a syscall
// there. CI runs this on arm64 as well as amd64.
func TestMemfdCreatesAMemfdOnThisArchitecture(t *testing.T) {
	fd, err := memfd("wlterm-test")
	if err != nil {
		t.Fatalf("memfd_create (syscall %d) failed: %v", sysMemfdCreate, err)
	}
	defer syscall.Close(fd)
	target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		t.Fatal(err)
	}
	if target != "/memfd:wlterm-test (deleted)" {
		t.Fatalf("syscall %d made %q, not a memfd", sysMemfdCreate, target)
	}
}
