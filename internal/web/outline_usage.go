package web

import (
	"bytes"
	"context"
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
)

const (
	outlineUsagePath       = "/api/outline-usage"
	outlineDetailsURL      = "https://api.getoutlinevpn.com/get-access-key-details"
	outlineRefreshInterval = 5 * time.Minute
)

// OutlineUsageConfig maps upstream names to the provider's access key names.
type OutlineUsageConfig struct {
	TokenFile string            `yaml:"token_file"`
	Upstreams map[string]string `yaml:"upstreams"`
}

func (c *OutlineUsageConfig) validate() error {
	if !filepath.IsAbs(c.TokenFile) {
		return errors.New("outline_usage.token_file: absolute path required")
	}
	if len(c.Upstreams) == 0 {
		return errors.New("outline_usage.upstreams: at least one mapping required")
	}
	for upstream, key := range c.Upstreams {
		if upstream == "" || strings.TrimSpace(upstream) != upstream || key == "" || strings.TrimSpace(key) != key {
			return errors.New("outline_usage.upstreams: names must be nonempty and have no surrounding whitespace")
		}
	}
	return nil
}

type outlineQuota struct {
	Limit     float64   `json:"limit"`
	Remaining float64   `json:"remaining"`
	Unit      string    `json:"unit"`
	UpdatedAt time.Time `json:"updatedAt"`
	Stale     bool      `json:"stale"`
}

type outlineUsage struct {
	cfg    *OutlineUsageConfig
	client *http.Client
	log    *slog.Logger

	mu     sync.RWMutex
	quotas map[string]*outlineQuota
}

// StartOutlineUsage fetches quota data in the background for the lifetime of
// the panel. The handler serves only the cache, even during an API failure.
func StartOutlineUsage(ctx context.Context, cfg *OutlineUsageConfig, log *slog.Logger) http.Handler {
	if cfg == nil {
		return nil
	}
	client := &http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	u := &outlineUsage{cfg: cfg, client: client, log: log, quotas: make(map[string]*outlineQuota, len(cfg.Upstreams))}
	for upstream := range cfg.Upstreams {
		u.quotas[upstream] = nil
	}
	go func() {
		defer client.CloseIdleConnections()
		tick := time.NewTicker(outlineRefreshInterval)
		defer tick.Stop()
		for {
			u.refresh(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return u
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
		return &outlineQuota{Limit: limit, Remaining: remaining, Unit: details.Unit, UpdatedAt: time.Now().UTC()}, nil
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
