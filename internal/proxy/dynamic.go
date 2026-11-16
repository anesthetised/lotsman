package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Location is where a dynamic access key is served: Outline's ssconf://
// URL, which stands for https://, or the https:// URL itself. Whoever has
// it gets the key, so it is a secret: String never shows it, and no error
// quotes it.
type Location struct {
	url *url.URL
}

// IsDynamic reports whether s is a dynamic key's location rather than a key.
func IsDynamic(s string) bool {
	scheme, _, ok := strings.Cut(strings.TrimSpace(s), "://")
	return ok && (strings.EqualFold(scheme, "ssconf") || strings.EqualFold(scheme, "https"))
}

// ParseLocation reads ssconf://host[:port]/path or https://…, as Outline
// clients accept them. The fragment, a display name, is dropped, as the
// clients do. Nothing is fetched.
func ParseLocation(s string) (*Location, error) {
	s = strings.TrimSpace(s)
	if !IsDynamic(s) {
		return nil, errors.New("dynamic key: want an ssconf:// or https:// URL")
	}
	_, rest, _ := strings.Cut(s, "://")
	u, err := url.Parse("https://" + rest)
	if err != nil {
		return nil, errors.New("dynamic key: malformed URL")
	}
	if u.Hostname() == "" {
		return nil, errors.New("dynamic key: URL has no host")
	}
	u.Fragment, u.RawFragment = "", ""
	return &Location{url: u}, nil
}

// String describes the location without revealing it.
func (l *Location) String() string { return "dynamic key URL (not shown)" }

// Fingerprint identifies the location without revealing it, so a cached key
// can be matched to the URL it came from.
func (l *Location) Fingerprint() string {
	sum := sha256.Sum256([]byte(l.url.String()))
	return hex.EncodeToString(sum[:])
}

// maxConfigSize bounds a dynamic key's response; real ones are a few hundred
// bytes.
const maxConfigSize = 64 << 10

// NewFetchClient is the HTTP client dynamic keys are fetched with: TLS
// verified against roots (nil: the system's), and redirects followed only to
// https://, at most 10. Requests carry no timeout of their own; the caller's
// context bounds them.
func NewFetchClient(roots *x509.CertPool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL.Scheme != "https" {
				return errors.New("redirect to a non-https URL refused")
			}
			return nil
		},
	}
}

// FetchKey downloads a dynamic key with client and parses it. It returns the
// key and the response text, which ParseConfig reads again, for a cache.
// Errors quote neither the URL nor the response.
func FetchKey(ctx context.Context, client *http.Client, loc *Location) (Key, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.url.String(), nil)
	if err != nil {
		return nil, "", errors.New("fetch dynamic key: malformed URL")
	}
	resp, err := client.Do(req)
	if err != nil {
		// A *url.Error quotes the URL; what it wraps does not.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, "", fmt.Errorf("fetch dynamic key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("fetch dynamic key: HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigSize+1))
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, "", fmt.Errorf("fetch dynamic key: read response: %w", err)
	}
	if len(body) > maxConfigSize {
		return nil, "", fmt.Errorf("fetch dynamic key: response exceeds %d KiB", maxConfigSize>>10)
	}
	text := strings.TrimSpace(string(body))
	key, err := ParseConfig(text)
	if err != nil {
		return nil, "", err
	}
	return key, text, nil
}

// ParseConfig reads an access key as Outline clients do (outline-apps,
// client/go/outline/parse.go, doParseTunnelConfig): a dynamic key's
// response or a static key file. It is a YAML document, JSON included, that
// is either
//
//   - a key as a string, ss://…;
//   - an Outline config object, {transport: …} (see parseOutlineConfig),
//     which may carry Shadowsocks over WebSocket;
//   - the legacy Shadowsocks object {server, server_port, method, password,
//     prefix}, prefix being raw characters, one byte each;
//   - {error: {message, details}}, the provider refusing, such as for an
//     expired subscription.
//
// Errors never quote the text, except a provider's error message, which is
// meant for users.
func ParseConfig(text string) (Key, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("access key: empty")
	}
	var doc any
	// Not wrapped: YAML errors can quote the document.
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, errors.New("access key: neither an ss:// key nor a YAML or JSON object")
	}
	switch v := doc.(type) {
	case string:
		if IsDynamic(v) {
			return nil, errors.New("access key: another dynamic key URL, want an ss:// key")
		}
		return ParseKey(v)
	case map[string]any:
		if e, ok := v["error"]; ok {
			return nil, providerError(e)
		}
		key, err := parseOutlineConfig(v)
		if err != nil {
			return nil, fmt.Errorf("access key: %w", err)
		}
		return key, nil
	default:
		return nil, errors.New("access key: neither an ss:// key nor a YAML or JSON object")
	}
}

// legacyShadowsocks reads Outline's legacy JSON key. Field values are never
// quoted: any of them may be the password.
func legacyShadowsocks(v map[string]any) (*shadowsocksKey, error) {
	str := func(name string, required bool) (string, error) {
		raw, ok := v[name]
		if !ok || raw == nil {
			if required {
				return "", fmt.Errorf("ss key: %s is missing", name)
			}
			return "", nil
		}
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("ss key: %s must be a string", name)
		}
		return s, nil
	}
	server, err := str("server", true)
	if err != nil {
		return nil, err
	}
	var port int
	switch p := v["server_port"].(type) {
	case int:
		port = p
	case string: // tolerated, as YAML often quotes numbers
		if port, err = strconv.Atoi(p); err != nil {
			return nil, errors.New("ss key: server_port must be a number")
		}
	case nil:
		return nil, errors.New("ss key: server_port is missing")
	default:
		return nil, errors.New("ss key: server_port must be a number")
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("ss key: server_port must be a number from 1 to 65535")
	}
	method, err := str("method", true)
	if err != nil {
		return nil, err
	}
	password, err := str("password", true)
	if err != nil {
		return nil, err
	}
	prefix, err := str("prefix", false)
	if err != nil {
		return nil, err
	}
	return newShadowsocksKey(net.JoinHostPort(server, strconv.Itoa(port)), method, password, prefix)
}

// providerError reports a provider's refusal by its message, quoted and
// shortened. Details are left out: they are meant for support, not logs.
func providerError(e any) error {
	var message string
	if m, ok := e.(map[string]any); ok {
		message, _ = m["message"].(string)
	}
	if message == "" {
		return errors.New("dynamic key: the provider returned an error without a message")
	}
	if r := []rune(message); len(r) > 200 {
		message = string(r[:200]) + "…"
	}
	return fmt.Errorf("dynamic key: the provider returned an error: %q", message)
}
