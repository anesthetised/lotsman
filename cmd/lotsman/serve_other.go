//go:build !linux

package main

import (
	"context"
	"errors"
)

func serve(context.Context, string) error {
	return errors.New("serve needs Linux: it creates TUN interfaces and programs policy routing and nftables")
}
