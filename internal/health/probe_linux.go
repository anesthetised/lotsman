//go:build linux

package health

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func bindProbe(d *net.Dialer, iface string) error {
	d.Control = func(_, _ string, raw syscall.RawConn) error {
		var bindErr error
		if err := raw.Control(func(fd uintptr) {
			bindErr = unix.BindToDevice(int(fd), iface)
		}); err != nil {
			return fmt.Errorf("probe interface %s: %w", iface, err)
		}
		if bindErr != nil {
			return fmt.Errorf("probe interface %s: %w", iface, bindErr)
		}
		return nil
	}
	return nil
}
