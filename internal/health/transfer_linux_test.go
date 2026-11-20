//go:build linux && integration

package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Run only in the disposable Linux integration container.
func TestTransferOnInterfaceBinding(t *testing.T) {
	from := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		select {
		case from <- host:
		default:
		}
		body(1<<20, 16<<10, 0, 0)(w, r)
	}))
	t.Cleanup(srv.Close)
	source := netip.MustParseAddr("127.0.0.2")
	if took, err := TransferOnInterface(context.Background(), "lo", source, srv.URL, 64<<10, time.Second); err != nil || took <= 0 {
		t.Fatalf("transfer on lo: %v, %v", took, err)
	}
	if got := <-from; got != source.String() {
		t.Errorf("transfer source = %s, want %s", got, source)
	}
	// A missing device must fail, not fall back to an unbound connection.
	_, err := TransferOnInterface(context.Background(), "lm-no-such-if", source, srv.URL, 64<<10, time.Second)
	if !errors.Is(err, unix.ENODEV) {
		t.Fatalf("missing interface: %v, want ENODEV", err)
	}
}

// Run only in the disposable Linux integration container.
func TestUploadOnInterfaceBinding(t *testing.T) {
	from := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		select {
		case from <- host:
		default:
		}
		sink(32<<10, 0, 0, http.StatusOK, nil, nil)(w, r)
	}))
	t.Cleanup(srv.Close)
	source := netip.MustParseAddr("127.0.0.2")
	if took, err := UploadOnInterface(context.Background(), "lo", source, srv.URL, 64<<10, time.Second); err != nil || took <= 0 {
		t.Fatalf("upload on lo: %v, %v", took, err)
	}
	if got := <-from; got != source.String() {
		t.Errorf("upload source = %s, want %s", got, source)
	}
	_, err := UploadOnInterface(context.Background(), "lm-no-such-if", source, srv.URL, 64<<10, time.Second)
	if !errors.Is(err, unix.ENODEV) {
		t.Fatalf("missing interface: %v, want ENODEV", err)
	}
}
