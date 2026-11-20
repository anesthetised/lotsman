package health

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
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

// UploadOnInterface is TransferOnInterface the other way: it POSTs size
// bytes of random data, which no compression on the path can shrink, to
// rawURL and passes when the server answers 2xx within timeout. A path that
// carries what comes in but loses what goes out passes a download; this
// catches it. It returns the time until the answer arrived.
func UploadOnInterface(ctx context.Context, iface string, source netip.Addr, rawURL string, size int64, timeout time.Duration) (time.Duration, error) {
	if iface == "" {
		return 0, fmt.Errorf("probe interface is required")
	}
	return upload(ctx, iface, source, rawURL, size, timeout)
}

// client makes requests through iface from source, or unbound when iface
// is "", as in tests.
func client(iface string, source netip.Addr) (*http.Client, error) {
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: source.AsSlice()}}
	if iface != "" {
		if err := bindProbe(&d, iface); err != nil {
			return nil, err
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil, // never an HTTP(S)_PROXY from the environment: it would bypass the upstream
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return d.DialContext(ctx, "tcp4", addr) // the source is IPv4
			},
			DisableKeepAlives:  true,
			DisableCompression: true, // count bytes on the wire, not inflated ones
		},
		// A redirect is followed, through the same interface.
	}, nil
}

func transfer(ctx context.Context, iface string, source netip.Addr, rawURL string, minBytes int64, timeout time.Duration) (time.Duration, error) {
	client, err := client(iface, source)
	if err != nil {
		return 0, err
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

func upload(ctx context.Context, iface string, source netip.Addr, rawURL string, size int64, timeout time.Duration) (time.Duration, error) {
	client, err := client(iface, source)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body := &countingReader{r: io.LimitReader(rand.Reader, size)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, body)
	if err != nil {
		return 0, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("upload to %s: %d of %d bytes sent in %s: %w", req.URL.Host, body.n.Load(), size, time.Since(start).Round(time.Millisecond), err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("upload to %s: %s", req.URL.Host, resp.Status)
	}
	return time.Since(start), nil
}

// countingReader counts what the transport has read of a request body, for
// the error of an upload that stalls. The transport reads it on its own
// goroutine.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Speed is the result of a speed test through an upstream: bytes per
// second each way.
type Speed struct {
	Down, Up uint64
	Time     time.Time // when the test finished; zero for none
}
