//go:build !linux

package tunnel

import "syscall"

func sizeSocketBuffers(_, _ string, _ syscall.RawConn) error { return nil }
