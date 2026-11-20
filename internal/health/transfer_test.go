package health

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
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
	_, err = UploadOnInterface(context.Background(), "", netip.MustParseAddr("127.0.0.1"), "http://127.0.0.1:1/", 1, time.Second)
	if err == nil || !strings.Contains(err.Error(), "interface is required") {
		t.Fatalf("empty interface, upload: %v", err)
	}
}

// sink reads a request body in chunks, sleeping pause before each; after
// freezeAt bytes (when positive) it reads nothing more until stop closes,
// like a path that loses outgoing packets. (The server notices a client
// that gives up only once it has read the body, so the request's context
// would never end.) It answers status once it has read everything.
func sink(chunk, freezeAt int, pause time.Duration, status int, stop <-chan struct{}, got *[]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, chunk)
		var body bytes.Buffer
		for {
			if freezeAt > 0 && body.Len() >= freezeAt {
				<-stop
				return
			}
			time.Sleep(pause)
			n, err := r.Body.Read(buf)
			body.Write(buf[:n])
			if err == io.EOF {
				break
			}
			if err != nil {
				return
			}
		}
		if got != nil {
			*got = body.Bytes()
		}
		w.WriteHeader(status)
	}
}

func TestUpload(t *testing.T) {
	const size = 256 << 10
	source := netip.MustParseAddr("127.0.0.1")
	for _, tc := range []struct {
		name            string
		chunk, freezeAt int
		pause           time.Duration
		status          int
		want            string // "" passes; otherwise part of the error
	}{
		{"fast", 32 << 10, 0, 0, http.StatusOK, ""},
		{"frozen after 64 KiB", 16 << 10, 64 << 10, 0, http.StatusOK, "of 262144 bytes sent"},
		{"slow", 1 << 10, 0, 20 * time.Millisecond, http.StatusOK, "of 262144 bytes sent"},
		{"error status", 32 << 10, 0, 0, http.StatusRequestEntityTooLarge, "413"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := make(chan struct{})
			srv := httptest.NewServer(sink(tc.chunk, tc.freezeAt, tc.pause, tc.status, stop, nil))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { close(stop) }) // before srv.Close, which waits for the handler
			start := time.Now()
			took, err := upload(context.Background(), "", source, srv.URL+"/up", size, 500*time.Millisecond)
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("took %v, the timeout is 500ms", elapsed)
			}
			switch {
			case tc.want == "" && (err != nil || took <= 0):
				t.Fatalf("upload: %v, %v; want a pass", took, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("upload: %v; want an error with %q", err, tc.want)
			}
		})
	}
}

// The body is size bytes of random data, which no compression shrinks.
func TestUploadSendsRandomBytes(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(sink(32<<10, 0, 0, http.StatusOK, nil, &got))
	t.Cleanup(srv.Close)
	if _, err := upload(context.Background(), "", netip.MustParseAddr("127.0.0.1"), srv.URL, 64<<10, time.Second); err != nil {
		t.Fatal(err)
	}
	var zip bytes.Buffer
	w := gzip.NewWriter(&zip)
	w.Write(got)
	w.Close()
	if len(got) != 64<<10 || zip.Len() < len(got) {
		t.Errorf("body: %d bytes, %d gzipped; want 65536 that do not compress", len(got), zip.Len())
	}
}
