package tunnel

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// socketBufferSize matches amneziawg-go's StdNetBind.
const socketBufferSize = 7 << 20

// sizeSocketBuffers asks for socketBufferSize, then forces it past
// net.core.rmem_max/wmem_max when the process has CAP_NET_ADMIN. Like
// StdNetBind it ignores errors: a smaller buffer only costs throughput.
func sizeSocketBuffers(_, _ string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		for _, opt := range []int{unix.SO_RCVBUF, unix.SO_SNDBUF, unix.SO_RCVBUFFORCE, unix.SO_SNDBUFFORCE} {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, opt, socketBufferSize)
		}
	})
}
