package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type quotaTransport func(*http.Request) (*http.Response, error)

func (f quotaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOutlineUsage(t *testing.T) {
	const keyAL = "key-al"
	const keyLV = "key-lv"
	const token = "fixture-api-token"
	path := filepath.Join(t.TempDir(), "outline.token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, "outline_usage:\n  token_file: "+path+"\n  upstreams:\n    ol-al: "+keyAL+"\n    ol-lv: "+keyLV+"\n")
	calls := map[string]int{}
	client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.String() != "https://api.getoutlinevpn.com/get-access-key-details" ||
			r.Header.Get("X-OUTLINE-BOT-API-SECRET-TOKEN") != token {
			t.Fatal("incorrect provider request")
		}
		var body struct {
			Key string `json:"access_key_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		calls[body.Key]++
		code, reply := http.StatusOK, `{"status":true,"access_key_name":"key-lv","plan_bandwidth":500,"remaining_bandwidth":"464.32","bandwidth_unit":"GB","expiration_date":123}`
		if body.Key == keyAL {
			reply = `{"status":true,"access_key_name":"key-al","plan_bandwidth":"500","reaming_bandwidth":"417,64","bandwidth_unit":"GB","expiration_date":"03.02.2030 12:34:56"}`
			if calls[keyAL] > 1 {
				code, reply = http.StatusServiceUnavailable, ""
			}
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(reply))}, nil
	})}
	usage := &outlineUsage{cfg: cfg.OutlineUsage, client: client, log: discardLog(), expirationZone: time.FixedZone("fixture", 2*3600)}
	usage.refresh(t.Context())
	usage.refresh(t.Context())
	panel := func(auth Authenticator, handler http.Handler) http.Handler {
		return New(Deps{Config: cfg, Auth: auth, OutlineUsage: handler, UI: fstest.MapFS{}, Log: discardLog()})
	}
	request := func(origin string) *http.Request {
		r := httptest.NewRequest("POST", "/api/outline-usage", nil)
		r.Header.Set("Origin", origin)
		return r
	}

	for _, tc := range []struct {
		name   string
		auth   fakeAuth
		origin string
		want   int
	}{
		{"anonymous", fakeAuth{}, testOrigin, 401},
		{"user link", fakeAuth{user: "alice"}, testOrigin, 401},
		{"wrong origin", fakeAuth{operator: true}, "https://elsewhere.example", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := serve(panel(tc.auth, usage), request(tc.origin)); w.Code != tc.want {
				t.Fatalf("status %d, want %d", w.Code, tc.want)
			}
		})
	}
	operator := fakeAuth{operator: true}
	if w := serve(panel(operator, nil), request(testOrigin)); w.Code != http.StatusNoContent {
		t.Fatalf("disabled integration: HTTP %d", w.Code)
	}
	w := serve(panel(operator, usage), request(testOrigin))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("operator quota response: HTTP %d, headers %v", w.Code, w.Header())
	}
	if strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), keyAL) || strings.Contains(w.Body.String(), keyLV) {
		t.Fatal("the quota response exposed a provider credential or identifier")
	}
	var rows map[string]outlineQuota
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if al, lv := rows["ol-al"], rows["ol-lv"]; len(rows) != 2 ||
		al.Remaining != 417.64 || al.Limit != 500 || al.Unit != "GB" || !al.Stale || al.UpdatedAt.IsZero() ||
		lv.Remaining != 464.32 || lv.Limit != 500 || lv.Unit != "GB" || lv.Stale || lv.UpdatedAt.IsZero() {
		t.Errorf("quotas by upstream = %+v", rows)
	}
	if calls[keyAL] != 2 || calls[keyLV] != 2 {
		t.Errorf("provider requests = %v", calls)
	}
	expiry := rows["ol-al"].ExpiresAt
	if want := time.Date(2030, 2, 3, 10, 34, 56, 0, time.UTC); expiry == nil || !expiry.Equal(want) || rows["ol-lv"].ExpiresAt != nil {
		t.Fatalf("provider expiries = %v and %v", expiry, rows["ol-lv"].ExpiresAt)
	}
	quota := *usage.quotas["ol-al"]
	quota.Remaining = 0
	usage.quotas["ol-al"] = &quota
	usage.quotas["ol-lv"] = nil
	readings := usage.routingReadings().GetUpstreams()
	if al, lv := readings["ol-al"], readings["ol-lv"]; !al.GetExhausted() ||
		!al.GetObservedAt().AsTime().Equal(quota.UpdatedAt) || !al.GetExpiresAt().AsTime().Equal(*expiry) ||
		lv.GetObservedAt() != nil || lv.GetExhausted() || len(lv.GetKeyHash()) != 32 {
		t.Fatal("invalid mapping of confirmed and unknown Outline readings")
	}
}
