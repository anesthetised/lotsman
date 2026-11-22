package web

import (
	"bytes"
	"embed"
	"html"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// dist is the built frontend (`just web-build`). Only its .gitkeep is in
// git, so a plain `go build` embeds no UI and serves the placeholder.
//
//go:embed all:dist
var dist embed.FS

// Dist is the embedded UI.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is a literal directory
	}
	return sub
}

const placeholder = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width">
<title>Lotsman</title>
<h1>Lotsman</h1>
<p>The web UI is not built into this binary. Run <code>just web-build</code>, then build lotsman again.</p>
`

// staticTimeout bounds writing a file; API streams are not subject to it.
const staticTimeout = 30 * time.Second
const timeZoneMeta = `<meta name="lotsman-time-zone" content=""`

// runtimeTimeZone returns the named zone supplied to the web process.
func runtimeTimeZone() *time.Location {
	name := os.Getenv("TZ")
	if name == "" || name == "Local" {
		return nil
	}
	zone, _ := time.LoadLocation(name)
	return zone
}

// ui serves the single-page app from files: a file by its path, and
// index.html for any other page, so client-side routes such as /u
// load the app. Without index.html it serves the placeholder.
type ui struct {
	files fs.FS
	index []byte
}

func newUI(files fs.FS) *ui {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return &ui{index: []byte(placeholder)}
	}
	if zone := runtimeTimeZone(); zone != nil {
		meta := `<meta name="lotsman-time-zone" content="` + html.EscapeString(zone.String()) + `"`
		index = bytes.Replace(index, []byte(timeZoneMeta), []byte(meta), 1)
	}
	return &ui{files: files, index: index}
}

func (u *ui) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(staticTimeout))
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if u.files != nil && name != "" && name != "index.html" {
		if fi, err := fs.Stat(u.files, name); err == nil && !fi.IsDir() {
			// Vite names the files under assets/ by their content hash.
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, u.files, name)
			return
		}
		if path.Ext(name) != "" {
			// A missing file, not a page: index.html in its place would
			// only confuse the script or style that wanted it.
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(u.index))
}
