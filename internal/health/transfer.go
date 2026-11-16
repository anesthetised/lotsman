package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// TransferOnInterface downloads rawURL through the named interface with the
// given source address and passes when at least minBytes of the body arrive
// within timeout, which bounds everything: the lookup, connecting, TLS, the
// request and the body. It reads minBytes and stops. The URL's host is
// resolved by the host's resolver; only the connection goes through the
// interface, and like ProbeOnInterface it never falls back to an unbound
// one. A reachability probe misses a path that carries a handshake but
// freezes or crawls after a few kilobytes; this does not. It returns the
// time until the last of minBytes arrived.
func TransferOnInterface(ctx context.Context, iface string, source netip.Addr, rawURL string, minBytes int64, timeout time.Duration) (time.Duration, error) {
	if iface == "" {
		return 0, fmt.Errorf("probe interface is required")
	}
	return transfer(ctx, iface, source, rawURL, minBytes, timeout)
}

func transfer(ctx context.Context, iface string, source netip.Addr, rawURL string, minBytes int64, timeout time.Duration) (time.Duration, error) {
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: source.AsSlice()}}
	if iface != "" {
		if err := bindProbe(&d, iface); err != nil {
			return 0, err
		}
	}
	client := http.Client{
		Transport: &http.Transport{
			Proxy: nil, // never an HTTP(S)_PROXY from the environment: it would bypass the upstream
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return d.DialContext(ctx, "tcp4", addr) // the source is IPv4
			},
			DisableKeepAlives:  true,
			DisableCompression: true, // count bytes on the wire, not inflated ones
		},
		// A redirect is followed, through the same interface.
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("transfer from %s: %s", req.URL.Host, resp.Status)
	}
	n, err := io.CopyN(io.Discard, resp.Body, minBytes)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = fmt.Errorf("the body ended")
		}
		return 0, fmt.Errorf("transfer from %s: %d of %d bytes in %s: %w", req.URL.Host, n, minBytes, time.Since(start).Round(time.Millisecond), err)
	}
	return time.Since(start), nil
}
