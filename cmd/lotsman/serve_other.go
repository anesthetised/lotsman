//go:build !linux

package main

import (
	"context"
	"errors"
	"log/slog"
)

func serve(context.Context, string, slog.Level) error {
	return errors.New("serve needs Linux: it creates TUN interfaces and programs policy routing and nftables")
}
