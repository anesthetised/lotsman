//go:build !linux

package health

import (
	"fmt"
	"net"
	"runtime"
)

func bindProbe(_ *net.Dialer, iface string) error {
	return fmt.Errorf("probe interface %s: interface binding unsupported on %s", iface, runtime.GOOS)
}
