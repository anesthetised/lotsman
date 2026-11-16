//go:build !linux

package helper

import (
	"errors"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// Helpers take over a kernel TUN, which the daemon creates on Linux only.
func openTUN() (tun.Device, error) {
	return nil, errors.New("proxy helpers run on Linux only")
}

func harden() error { return nil }
