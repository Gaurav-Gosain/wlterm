//go:build arm64 || riscv64 || loong64 || s390x || mips64 || mips64le

package main

import "syscall"

// These architectures have the number in the syscall package. amd64 and the
// others with a file of their own do not.
const sysMemfdCreate = syscall.SYS_MEMFD_CREATE
