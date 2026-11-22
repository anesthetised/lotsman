package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
)

const (
	outlineUsagePath       = "/api/outline-usage"
	outlineDetailsURL      = "https://api.getoutlinevpn.com/get-access-key-details"
	outlineRefreshInterval = 5 * time.Minute
)

// OutlineUsageConfig maps upstream names to the provider's access key names.
type OutlineUsageConfig struct {
	TokenFile          string            `yaml:"token_file"`
	Upstreams          map[string]string `yaml:"upstreams"`
	ExpirationTimeZone string            `yaml:"expiration_time_zone"`
}

func (c *OutlineUsageConfig) validate() error {
	if !filepath.IsAbs(c.TokenFile) {
		return errors.New("outline_usage.token_file: absolute path required")
	}
	if len(c.Upstreams) == 0 {
		return errors.New("outline_usage.upstreams: at least one mapping required")
	}
	if c.ExpirationTimeZone != "" {
		if _, err := time.LoadLocation(c.ExpirationTimeZone); err != nil || c.ExpirationTimeZone == "Local" {
			return errors.New("outline_usage.expiration_time_zone: use a named timezone")
		}
	}
	for upstream, key := range c.Upstreams {
		if upstream == "" || strings.TrimSpace(upstream) != upstream || key == "" || strings.TrimSpace(key) != key {
			return errors.New("outline_usage.upstreams: names must be nonempty and have no surrounding whitespace")
		}
	}
	return nil
}

type outlineQuota struct {
	Limit     float64    `json:"limit"`
	Remaining float64    `json:"remaining"`
	Unit      string     `json:"unit"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Stale     bool       `json:"stale"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

type outlineUsage struct {
	cfg            *OutlineUsageConfig
	client         *http.Client
	log            *slog.Logger
	expirationZone *time.Location

	mu     sync.RWMutex
	quotas map[string]*outlineQuota
}

// StartOutlineUsage fetches quota data in the background for the lifetime of
// the panel. The handler serves only the cache, even during an API failure.
func StartOutlineUsage(ctx context.Context, cfg *OutlineUsageConfig, daemon http.RoundTripper, log *slog.Logger) http.Handler {
	transport := connecthttp.NewTransport(&http.Client{Transport: daemon, Timeout: 5 * time.Second}, "http://lotsman")
	gateway := lotsmanv1connect.NewStatusServiceClient(connect.NewClient(transport))
	if cfg == nil {
		go func() {
			tick := time.NewTicker(outlineRefreshInterval)
			defer tick.Stop()
			for {
				if _, err := gateway.ReportOutlineUsage(ctx, &lotsmanv1.ReportOutlineUsageRequest{}); err == nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}()
		return nil
	}
	client := &http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	expirationZone := runtimeTimeZone()
	if cfg.ExpirationTimeZone != "" {
		expirationZone, _ = time.LoadLocation(cfg.ExpirationTimeZone)
	}
	u := &outlineUsage{cfg: cfg, client: client, log: log, expirationZone: expirationZone, quotas: make(map[string]*outlineQuota, len(cfg.Upstreams))}
	for upstream := range cfg.Upstreams {
		u.quotas[upstream] = nil
	}
	go func() {
		defer client.CloseIdleConnections()
		tick := time.NewTicker(outlineRefreshInterval)
		defer tick.Stop()
		for {
			u.refresh(ctx)
			if _, err := gateway.ReportOutlineUsage(ctx, u.routingReadings()); err != nil && ctx.Err() == nil {
				log.Warn("could not report Outline limits to the gateway", "code", connect.CodeOf(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return u
}

func (u *outlineUsage) routingReadings() *lotsmanv1.ReportOutlineUsageRequest {
	payload := &lotsmanv1.ReportOutlineUsageRequest{Upstreams: make(map[string]*lotsmanv1.OutlineUsageReading, len(u.cfg.Upstreams))}
	u.mu.RLock()
	defer u.mu.RUnlock()
	for name, key := range u.cfg.Upstreams {
		hash := sha256.Sum256([]byte(key))
		row := &lotsmanv1.OutlineUsageReading{KeyHash: hash[:]}
		if quota := u.quotas[name]; quota != nil {
			row.ObservedAt = timestamppb.New(quota.UpdatedAt)
			row.Exhausted = quota.Remaining == 0
			if quota.ExpiresAt != nil {
				row.ExpiresAt = timestamppb.New(*quota.ExpiresAt)
			}
		}
		payload.Upstreams[name] = row
	}
	return payload
}

func (u *outlineUsage) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	u.mu.RLock()
	rows := u.quotas // published snapshots are immutable
	u.mu.RUnlock()
	writeJSON(w, rows)
}

func (u *outlineUsage) refresh(ctx context.Context) {
	secret, err := os.ReadFile(u.cfg.TokenFile)
	token := strings.TrimSpace(string(secret))
	if err != nil || token == "" {
		u.log.Warn("Outline quota token file unavailable or empty")
		token = ""
	}

	rows := make(map[string]*outlineQuota, len(u.cfg.Upstreams))
	byKey := make(map[string]*outlineQuota)
	for upstream, key := range u.cfg.Upstreams {
		if ctx.Err() != nil {
			return
		}
		quota, seen := byKey[key]
		if token != "" && !seen {
			quota, err = u.fetch(ctx, token, key)
			if err != nil && ctx.Err() == nil {
				u.log.Warn("Outline quota refresh failed", "upstream", upstream, "error", err)
			}
			byKey[key] = quota
		}
		if previous := u.quotas[upstream]; quota == nil && previous != nil {
			stale := *previous
			stale.Stale = true
			quota = &stale
		}
		rows[upstream] = quota
	}
	u.mu.Lock()
	u.quotas = rows
	u.mu.Unlock()
}

func (u *outlineUsage) fetch(ctx context.Context, token, key string) (*outlineQuota, error) {
	body, err := json.Marshal(map[string]string{"access_key_name": key})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, outlineDetailsURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Outline-Bot-Api-Secret-Token", token)
	res, err := u.client.Do(req)
	if err != nil {
		return nil, errors.New("could not reach the provider")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider returned HTTP %d", res.StatusCode)
	}
	const maxBody = 64 << 10
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	var details struct {
		Status          bool            `json:"status"`
		Key             string          `json:"access_key_name"`
		Limit           json.RawMessage `json:"plan_bandwidth"`
		Remaining       json.RawMessage `json:"remaining_bandwidth"`
		LegacyRemaining json.RawMessage `json:"reaming_bandwidth"` // spelling in Outline VPN's API
		Unit            string          `json:"bandwidth_unit"`
		ExpirationDate  json.RawMessage `json:"expiration_date"`
	}
	if err != nil || len(payload) > maxBody || json.Unmarshal(payload, &details) != nil || !details.Status || details.Key != key {
		return nil, errors.New("invalid provider response")
	}
	if len(details.Remaining) == 0 {
		details.Remaining = details.LegacyRemaining
	}
	limit, limitOK := quotaAmount(details.Limit)
	remaining, remainingOK := quotaAmount(details.Remaining)
	if !limitOK || !remainingOK || limit == 0 {
		return nil, errors.New("invalid quota numbers")
	}
	switch details.Unit = strings.ToUpper(strings.TrimSpace(details.Unit)); details.Unit {
	case "B", "KB", "MB", "GB", "TB":
		quota := &outlineQuota{Limit: limit, Remaining: remaining, Unit: details.Unit, UpdatedAt: time.Now().UTC()}
		// The provider returns a wall-clock date with no UTC offset.
		var expirationDate string
		if u.expirationZone != nil && json.Unmarshal(details.ExpirationDate, &expirationDate) == nil {
			if expires, err := time.ParseInLocation("02.01.2006 15:04:05", expirationDate, u.expirationZone); err == nil {
				expiry := expires.UTC()
				quota.ExpiresAt = &expiry
			}
		}
		return quota, nil
	default:
		return nil, errors.New("unsupported quota unit")
	}
}

func quotaAmount(raw json.RawMessage) (float64, bool) {
	value := string(raw)
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	amount, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(value), ",", "."), 64)
	return amount, err == nil && amount >= 0 && !math.IsInf(amount, 0)
}
