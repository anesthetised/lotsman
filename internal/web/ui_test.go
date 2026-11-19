package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func uiPanel(t *testing.T, files fs.FS) http.Handler {
	t.Helper()
	return New(Deps{Config: testConfig(t, ""), Auth: NoAuth{}, Daemon: &fakeDaemon{}, UI: files, Log: discardLog()})
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	return serve(h, httptest.NewRequest(http.MethodGet, path, nil))
}

func TestPlaceholderWithoutBuild(t *testing.T) {
	h := uiPanel(t, fstest.MapFS{".gitkeep": {}})
	for _, path := range []string{"/", "/u/abc", "/operator/users"} {
		w := get(h, path)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "just web-build") ||
			!strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: status %d, type %q, body %q", path, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
	}
	// The placeholder obeys the panel's own CSP: nothing inline.
	if body := get(h, "/").Body.String(); strings.Contains(body, "<script") || strings.Contains(body, "<style") || strings.Contains(body, "style=") {
		t.Errorf("placeholder has inline script or style: %s", body)
	}
}

// The repository's dist holds only .gitkeep, so a binary built without the
// frontend serves the placeholder.
func TestEmbeddedDist(t *testing.T) {
	if _, err := fs.Stat(Dist(), ".gitkeep"); err != nil {
		t.Fatalf("dist not embedded: %v", err)
	}
	_, err := fs.Stat(Dist(), "index.html")
	body := get(uiPanel(t, Dist()), "/").Body.String()
	if built := err == nil; built == strings.Contains(body, "just web-build") {
		t.Errorf("index.html built %v, but / serves %q", built, body)
	}
}

func TestSinglePageApp(t *testing.T) {
	files := fstest.MapFS{
		"index.html":          {Data: []byte("app")},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
		"favicon.svg":         {Data: []byte("<svg/>")},
	}
	h := uiPanel(t, files)
	for _, tc := range []struct {
		path, body, cache string
		code              int
	}{
		{"/", "app", "no-cache", http.StatusOK},
		{"/u/0123abcd", "app", "no-cache", http.StatusOK},
		{"/operator/profiles/eu", "app", "no-cache", http.StatusOK},
		{"/assets", "app", "no-cache", http.StatusOK}, // a directory is no file
		{"/index.html", "app", "no-cache", http.StatusOK},
		{"/assets/index-abc.js", "console.log(1)", "public, max-age=31536000, immutable", http.StatusOK},
		{"/favicon.svg", "<svg/>", "", http.StatusOK},
		{"/assets/gone.js", "", "", http.StatusNotFound},
	} {
		w := get(h, tc.path)
		if w.Code != tc.code || (tc.body != "" && w.Body.String() != tc.body) || w.Header().Get("Cache-Control") != tc.cache {
			t.Errorf("%s: status %d, Cache-Control %q, body %q; want %d, %q, %q",
				tc.path, w.Code, w.Header().Get("Cache-Control"), w.Body, tc.code, tc.cache, tc.body)
		}
	}
	if w := get(h, "/assets/index-abc.js"); !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("script Content-Type = %q", w.Header().Get("Content-Type"))
	}
	r := httptest.NewRequest(http.MethodPut, "/x", nil)
	r.Header.Set("Origin", testOrigin)
	if w := serve(h, r); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: status %d", w.Code)
	}
}
