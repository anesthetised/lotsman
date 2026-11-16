package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// body serves size bytes in chunks, sleeping pause before each chunk; after
// freezeAt bytes (when positive) it sends nothing more until the client
// gives up, like a path that stalls a flow after its first kilobytes.
func body(size, chunk, freezeAt int, pause time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, chunk)
		for sent := 0; sent < size; sent += chunk {
			if freezeAt > 0 && sent >= freezeAt {
				<-r.Context().Done()
				return
			}
			time.Sleep(pause)
			if _, err := w.Write(buf); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}
}

func TestTransfer(t *testing.T) {
	const minBytes = 64 << 10
	source := netip.MustParseAddr("127.0.0.1")
	for _, tc := range []struct {
		name    string
		handler http.Handler
		want    string // "" passes; otherwise part of the error
	}{
		{"fast", body(1<<20, 16<<10, 0, 0), ""},
		{"exactly min_bytes", body(minBytes, 16<<10, 0, 0), ""},
		{"frozen after 16 KiB", body(1<<20, 4<<10, 16<<10, 0), "16384 of 65536 bytes"},
		{"slow", body(1<<20, 4<<10, 0, 100*time.Millisecond), "of 65536 bytes"},
		{"short body", body(32<<10, 16<<10, 0, 0), "32768 of 65536 bytes"},
		{"error status", http.NotFoundHandler(), "404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			start := time.Now()
			took, err := transfer(context.Background(), "", source, srv.URL+"/down", minBytes, 500*time.Millisecond)
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("took %v, the timeout is 500ms", elapsed)
			}
			switch {
			case tc.want == "" && (err != nil || took <= 0):
				t.Fatalf("transfer: %v, %v; want a pass", took, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("transfer: %v; want an error with %q", err, tc.want)
			}
		})
	}
}

func TestTransferHonoursContext(t *testing.T) {
	srv := httptest.NewServer(body(1<<20, 4<<10, 4<<10, 0))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := transfer(ctx, "", netip.MustParseAddr("127.0.0.1"), srv.URL, 64<<10, time.Minute); err == nil {
		t.Fatal("frozen transfer passed")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancel took %v to stop the probe", elapsed)
	}
}

func TestTransferOnInterfaceRequiresInterface(t *testing.T) {
	_, err := TransferOnInterface(context.Background(), "", netip.MustParseAddr("127.0.0.1"), "http://127.0.0.1:1/", 1, time.Second)
	if err == nil || !strings.Contains(err.Error(), "interface is required") {
		t.Fatalf("empty interface: %v", err)
	}
}
