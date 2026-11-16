package proxy

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	dynSecretPath = "/s3cr3t-path-token"
	dynPassword   = "hunter2password"
)

func ssKey(server string) string {
	return "ss://" + base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:"+dynPassword)) + "@" + server + "/?outline=1#name"
}

func TestParseLocation(t *testing.T) {
	for in, want := range map[string]string{
		"ssconf://keys.example.org/" + dynSecretPath[1:] + "#My%20key": "https://keys.example.org" + dynSecretPath,
		" SSCONF://keys.example.org:8443/a?b=c\n":                      "https://keys.example.org:8443/a?b=c",
		"https://keys.example.org/x":                                   "https://keys.example.org/x",
	} {
		loc, err := ParseLocation(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if loc.url.String() != want {
			t.Errorf("%q → %q, want %q", in, loc.url, want)
		}
		if s := fmt.Sprint(loc) + fmt.Sprintf("%+v", loc); strings.Contains(s, "example.org") {
			t.Errorf("formatting shows the URL: %s", s)
		}
	}
	a, _ := ParseLocation("ssconf://keys.example.org/a")
	b, _ := ParseLocation("https://keys.example.org/a#other-name")
	c, _ := ParseLocation("ssconf://keys.example.org/b")
	if a.Fingerprint() != b.Fingerprint() || a.Fingerprint() == c.Fingerprint() || strings.Contains(a.Fingerprint(), "keys") {
		t.Error("fingerprints should match for the same URL only, and hide it")
	}

	if IsDynamic("http://keys.example.org/x") || IsDynamic("ss://x") || !IsDynamic(" ssconf://h/x") {
		t.Error("IsDynamic wants ssconf:// or https:// only")
	}
	for _, in := range []string{"http://keys.example.org" + dynSecretPath, "ssconf://" + dynSecretPath, "ssconf://h:port" + dynSecretPath, "ss://x"} {
		_, err := ParseLocation(in)
		if err == nil {
			t.Errorf("%q accepted", in)
		} else if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("error quotes the URL: %v", err)
		}
	}
}

func TestParseConfig(t *testing.T) {
	key := ssKey("192.0.2.1:8388")
	for name, tc := range map[string]struct {
		text, server string
		prefix       []byte
	}{
		"ss key":        {text: key + "\n", server: "192.0.2.1:8388"},
		"quoted string": {text: `"` + key + `"`, server: "192.0.2.1:8388"},
		"legacy json": {text: `{"server": "ss.example.org", "server_port": 8388, "password": "` + dynPassword + `", "method": "chacha20-ietf-poly1305", "prefix": "\u0016\u0003\u0001\u0000¨\u0001\u0001"}`,
			server: "ss.example.org:8388", prefix: []byte{0x16, 0x03, 0x01, 0x00, 0xa8, 0x01, 0x01}},
		"legacy yaml, ipv6": {text: "server: 2001:db8::1\nserver_port: \"443\"\npassword: x\nmethod: AES-256-GCM\n", server: "[2001:db8::1]:443"},
	} {
		t.Run(name, func(t *testing.T) {
			k, err := ParseConfig(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			ss := k.(*shadowsocksKey)
			if ss.Server() != tc.server || !bytes.Equal(ss.prefix, tc.prefix) {
				t.Errorf("server %q prefix % x", ss.Server(), ss.prefix)
			}
		})
	}

	for name, tc := range map[string]struct{ text, want string }{
		"empty":           {" \n", "empty response"},
		"provider error":  {`{"error": {"message": "Subscription expired", "details": "` + dynPassword + `"}}`, `provider returned an error: "Subscription expired"`},
		"bare error":      {`{"error": "` + dynPassword + `"}`, "without a message"},
		"transport":       {`{"transport": {"$type": "tcpudp"}}`, "transport format is not supported"},
		"another url":     {"ssconf://keys.example.org" + dynSecretPath, "another dynamic key URL"},
		"html":            {"<html>" + dynPassword + "</html>", "want a URL"},
		"list":            {"[1, 2]", "neither an ss:// key"},
		"broken yaml":     {"{server: [" + dynPassword, "neither an ss:// key"},
		"no server":       {`{"method": "aes-256-gcm", "password": "` + dynPassword + `"}`, "server is missing"},
		"port as text":    {`{"server": "h", "server_port": "` + dynPassword + `", "method": "aes-256-gcm", "password": "x"}`, "server_port must be a number"},
		"port range":      {`{"server": "h", "server_port": 70000, "method": "aes-256-gcm", "password": "x"}`, "from 1 to 65535"},
		"password number": {`{"server": "h", "server_port": 1, "method": "aes-256-gcm", "password": 12345}`, "password must be a string"},
		"bad cipher":      {`{"server": "h", "server_port": 1, "method": "` + dynPassword + `", "password": "` + dynPassword + `"}`, "unsupported cipher"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig(tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), dynPassword) || strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("error quotes the response: %v", err)
			}
		})
	}

	long := strings.Repeat("é", 300)
	if _, err := ParseConfig(`{"error": {"message": "` + long + `"}}`); err == nil || strings.Contains(err.Error(), long) || !strings.Contains(err.Error(), "…") {
		t.Errorf("long provider message: %v", err)
	}
}

// keyServer is an HTTPS server for dynamic keys; the client trusts its
// certificate through the roots seam, with verification on.
func keyServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the untrusted-certificate case fails handshakes on purpose
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return srv, NewFetchClient(roots)
}

func location(t *testing.T, srv *httptest.Server, path string) *Location {
	t.Helper()
	loc, err := ParseLocation(strings.Replace(srv.URL, "https://", "ssconf://", 1) + path + "#name")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func fetch(t *testing.T, client *http.Client, loc *Location, timeout time.Duration) (Key, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	k, text, err := FetchKey(ctx, client, loc)
	if err != nil && (strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), dynPassword)) {
		t.Errorf("error quotes the URL or the response: %v", err)
	}
	return k, text, err
}

func TestFetchKey(t *testing.T) {
	body := map[string]string{
		dynSecretPath + "/ss":     ssKey("192.0.2.1:8388") + "\n",
		dynSecretPath + "/json":   `{"server": "192.0.2.2", "server_port": 443, "password": "` + dynPassword + `", "method": "chacha20-ietf-poly1305"}`,
		dynSecretPath + "/error":  `{"error": {"message": "Expired", "details": "` + dynPassword + `"}}`,
		dynSecretPath + "/huge":   strings.Repeat(" ", maxConfigSize) + ssKey("192.0.2.1:8388"),
		dynSecretPath + "/status": dynPassword,
	}
	var slow atomic.Bool
	srv, client := keyServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == dynSecretPath+"/status":
			http.Error(w, body[r.URL.Path], http.StatusForbidden)
		case r.URL.Path == dynSecretPath+"/slow":
			slow.Store(true)
			<-r.Context().Done()
		default:
			fmt.Fprint(w, body[r.URL.Path])
		}
	})

	k, text, err := fetch(t, client, location(t, srv, dynSecretPath+"/ss"), 5*time.Second)
	if err != nil || k.Server() != "192.0.2.1:8388" || text != ssKey("192.0.2.1:8388") {
		t.Errorf("ss key: %v, %v, %q", k, err, text)
	}
	if again, err := ParseConfig(text); err != nil || again.Server() != k.Server() {
		t.Errorf("the returned text does not parse back: %v", err)
	}
	if k, _, err := fetch(t, client, location(t, srv, dynSecretPath+"/json"), 5*time.Second); err != nil || k.Server() != "192.0.2.2:443" {
		t.Errorf("json key: %v, %v", k, err)
	}
	for path, want := range map[string]string{
		"/error":  `provider returned an error: "Expired"`,
		"/huge":   "exceeds 64 KiB",
		"/status": "HTTP status 403",
	} {
		if _, _, err := fetch(t, client, location(t, srv, dynSecretPath+path), 5*time.Second); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
	}

	start := time.Now()
	_, _, err = fetch(t, client, location(t, srv, dynSecretPath+"/slow"), 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second || !slow.Load() {
		t.Errorf("slow server: %v after %v", err, time.Since(start))
	}

	// Verification is on: the system roots do not know the test CA.
	if _, _, err := fetch(t, NewFetchClient(nil), location(t, srv, dynSecretPath+"/ss"), 5*time.Second); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("untrusted certificate: %v", err)
	}
}

func TestFetchKeyRedirects(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		fmt.Fprint(w, ssKey("192.0.2.9:8388"))
	}))
	t.Cleanup(plain.Close)
	var srv *httptest.Server
	srv, client := keyServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case dynSecretPath + "/to-https":
			http.Redirect(w, r, srv.URL+dynSecretPath+"/key", http.StatusFound)
		case dynSecretPath + "/to-http":
			http.Redirect(w, r, plain.URL+dynSecretPath, http.StatusMovedPermanently)
		case dynSecretPath + "/loop":
			http.Redirect(w, r, srv.URL+dynSecretPath+"/loop", http.StatusTemporaryRedirect)
		default:
			fmt.Fprint(w, ssKey("192.0.2.1:8388"))
		}
	})

	if k, _, err := fetch(t, client, location(t, srv, dynSecretPath+"/to-https"), 5*time.Second); err != nil || k.Server() != "192.0.2.1:8388" {
		t.Errorf("redirect to https: %v, %v", k, err)
	}
	if _, _, err := fetch(t, client, location(t, srv, dynSecretPath+"/to-http"), 5*time.Second); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Errorf("redirect to http: %v", err)
	}
	if plainHits.Load() != 0 {
		t.Error("the plain-HTTP target was requested")
	}
	if _, _, err := fetch(t, client, location(t, srv, dynSecretPath+"/loop"), 5*time.Second); err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Errorf("redirect loop: %v", err)
	}
}
