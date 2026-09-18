package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	families := []Family{
		{Name: "up", Type: "gauge", Help: "Whether it is up."},
		{Name: "bytes_total", Type: "counter", Help: "Bytes."},
		{Name: "empty", Type: "gauge", Help: "Has no samples."},
	}
	samples := []Sample{
		{Name: "bytes_total", Labels: []Label{{"upstream", "b"}}, Value: 10},
		{Name: "up", Labels: []Label{{"upstream", "b"}}, Value: 1},
		{Name: "up", Labels: []Label{{"upstream", "a"}}, Value: 0},
		{Name: "bytes_total", Labels: []Label{{"upstream", "a"}}, Value: 1234567890123},
		{Name: "up", Value: 0.5},
	}
	var b strings.Builder
	if err := Write(&b, families, samples); err != nil {
		t.Fatal(err)
	}
	want := `# HELP up Whether it is up.
# TYPE up gauge
up 0.5
up{upstream="a"} 0
up{upstream="b"} 1
# HELP bytes_total Bytes.
# TYPE bytes_total counter
bytes_total{upstream="a"} 1.234567890123e+12
bytes_total{upstream="b"} 10
# HELP empty Has no samples.
# TYPE empty gauge
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestEscaping(t *testing.T) {
	var b strings.Builder
	Write(&b, []Family{{Name: "m", Type: "gauge"}}, []Sample{
		{Name: "m", Labels: []Label{{"user", `a"b\c` + "\n"}}, Value: 1},
	})
	want := "m{user=\"a\\\"b\\\\c\\n\"} 1\n"
	if !strings.HasSuffix(b.String(), want) {
		t.Errorf("got %q, want suffix %q", b.String(), want)
	}
}

func TestHandler(t *testing.T) {
	calls := 0
	h := Handler([]Family{{Name: "n", Type: "gauge", Help: "N."}}, func() []Sample {
		calls++
		return []Sample{{Name: "n", Value: float64(calls)}}
	})
	for want := 1; want <= 2; want++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
			t.Errorf("content type %q", ct)
		}
		if !strings.Contains(rec.Body.String(), "\nn "+string(rune('0'+want))+"\n") {
			t.Errorf("scrape %d: fresh snapshot not served:\n%s", want, rec.Body.String())
		}
	}
}
