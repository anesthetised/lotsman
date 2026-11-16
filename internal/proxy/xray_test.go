package proxy

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	xlog "github.com/xtls/xray-core/common/log"
)

func TestXrayLogLevels(t *testing.T) {
	var out bytes.Buffer
	now := time.Unix(0, 0)
	h := &xrayLog{
		log:  slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})),
		now:  func() time.Time { return now },
		last: map[string]time.Time{},
	}
	record := func(sev xlog.Severity, text string) string {
		out.Reset()
		h.Handle(&xlog.GeneralMessage{Severity: sev, Content: errors.New(text)})
		return out.String()
	}
	const realCert = "transport/internet/reality: REALITY: received real certificate (potential MITM or redirection)"
	for _, tc := range []struct {
		name, text, want string
		sev              xlog.Severity
		after            time.Duration
	}{
		{"error", "[101] " + realCert, "level=ERROR", xlog.Severity_Error, 0},
		{"same kind, other flow", "[202] " + realCert + " > cause", "level=DEBUG", xlog.Severity_Error, time.Second},
		{"other kind", "app/x: other", "level=ERROR", xlog.Severity_Error, 0},
		{"a minute later", "[303] " + realCert, "level=ERROR", xlog.Severity_Error, time.Minute},
		{"warning", "core: Xray started", "level=WARN", xlog.Severity_Warning, 0},
		{"info", "app/dispatcher: default route", "level=DEBUG", xlog.Severity_Info, 0},
	} {
		now = now.Add(tc.after)
		if got := record(tc.sev, tc.text); !strings.Contains(got, tc.want) || !strings.Contains(got, "component=xray") {
			t.Errorf("%s: logged %q, want %s", tc.name, got, tc.want)
		}
	}
	if got := xrayLogKind("[1] [2] a/b: msg > c > d"); got != "a/b: msg" {
		t.Errorf("kind = %q", got)
	}

	// The kinds remembered are bounded; at the bound, new kinds are debug
	// messages until old ones expire.
	for i := range 2 * xrayLogKinds {
		record(xlog.Severity_Error, strings.Repeat("x", i+1))
	}
	if len(h.last) > xrayLogKinds {
		t.Errorf("%d kinds remembered", len(h.last))
	}
	if got := record(xlog.Severity_Error, "new kind"); !strings.Contains(got, "level=DEBUG") {
		t.Errorf("at the bound: %q", got)
	}
	now = now.Add(time.Minute)
	if got := record(xlog.Severity_Error, "new kind"); !strings.Contains(got, "level=ERROR") {
		t.Errorf("after expiry: %q", got)
	}
}
