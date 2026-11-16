//go:build linux && !race

// Runs Xray's VLESS server, which races (see vlesstest). Linux only: the
// test makes Go trust the camouflage site through SSL_CERT_FILE, which Go
// reads on Linux, not on macOS.

package proxy

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/proxy/vlesstest"
)

const spiderEnv = "LOTSMAN_TEST_SPIDER"

// A REALITY client that receives a real, trusted certificate, as under a
// man-in-the-middle that has one, starts Xray's crawler, which indexes the
// config's spiderY unchecked. A key that ParseVLESS read survives that; the
// same key without spiderY, as a config built without Xray's JSON loader
// would be, crashes the process, which shows the test reaches the crawler.
// Each case runs in a child process: Go loads its trusted roots once per
// process, and one case is meant to crash.
func TestVLESSRealCertificate(t *testing.T) {
	if mode := os.Getenv(spiderEnv); mode != "" {
		spiderChild(t, mode)
		return
	}
	for _, tc := range []struct {
		mode  string
		crash bool
	}{{"parsed", false}, {"without-spiderY", true}} {
		t.Run(tc.mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestVLESSRealCertificate$", "-test.v", "-test.count=1")
			cmd.Env = append(os.Environ(), spiderEnv+"="+tc.mode, "SSL_CERT_FILE="+filepath.Join(t.TempDir(), "roots.pem"))
			out, err := cmd.CombinedOutput()
			crashed := strings.Contains(string(out), "index out of range")
			switch {
			case !strings.Contains(string(out), "received real certificate"):
				t.Fatalf("the client never received the real certificate: %v\n%s", err, out)
			case tc.crash && (err == nil || !crashed):
				t.Fatalf("without spiderY the crawler did not crash: %v\n%s", err, out)
			case !tc.crash && (err != nil || crashed):
				t.Fatalf("the crawler crashed the process: %v\n%s", err, out)
			}
		})
	}
}

func spiderChild(t *testing.T, mode string) {
	roots := os.Getenv("SSL_CERT_FILE")
	s := vlesstest.NewServer(t, nil, func(cert *x509.Certificate) {
		if err := os.WriteFile(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	vlesstest.WaitReady(t, s)
	LogXray(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParseVLESS(s.LinkWith(url.Values{"pbk": {base64.RawURLEncoding.EncodeToString(other.PublicKey().Bytes())}}))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "without-spiderY" {
		key.(*vlessKey).spiderY = nil
	}
	d, err := key.Dialers(s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	conn, err := d.Streams.DialStream(context.Background(), "127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "x")
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("data through a server that presented a real certificate")
	}
	time.Sleep(2 * time.Second) // the crawler runs in the background
	t.Log("the process survived the crawler")
}
