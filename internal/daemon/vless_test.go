package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/config"
)

const vlessID = "b831381d-6324-4d53-ad4f-8cda48b30811"

func TestLoadVLESSUpstream(t *testing.T) {
	dir := t.TempDir()
	useResolver(t, &fakeResolver{hosts: map[string]string{"vless.provider.test": "192.0.2.20"}})
	path := filepath.Join(dir, "vl.key")
	link := "vless://" + vlessID + "@vless.provider.test:443?security=reality&pbk=wsxdeEB2O7Mqa1ny8bxMHOMyTzvWvvVQXqhNBNPMn2E&sni=www.example.com&flow=xtls-rprx-vision#Provider"
	os.WriteFile(path, []byte(link+"\n"), 0o600)
	cfg := config.Upstream{Name: "vl", Kind: config.KindVLESS, Conf: path}
	u, err := LoadUpstream(cfg, config.Default().Health)
	if err != nil {
		t.Fatal(err)
	}
	if u.Key == nil || u.Location != nil || u.Endpoint != "vless.provider.test:443" || u.Addr != config.ProxyAddress || u.MTU != 1500 || u.keyText != link {
		t.Errorf("upstream = %+v", u)
	}
	if got := u.Key.String(); got != "vless reality xtls-rprx-vision, sni www.example.com, fp chrome" {
		t.Errorf("key description = %q", got)
	}
	if err := u.Resolve(context.Background()); err != nil || u.Resolved != "192.0.2.20:443" {
		t.Errorf("Resolve: %v, %q", err, u.Resolved)
	}

	// The kind decides what the file must hold; errors never quote it.
	for name, tc := range map[string]struct {
		kind, source, want string
	}{
		"broken link":            {config.KindVLESS, strings.Replace(link, "security=reality", "security=tls", 1), "only security=reality"},
		"ss key for vless":       {config.KindVLESS, "ss://Y2hhY2hhMjAtaWV0Zi1wb2x5MTMwNTpzZWNyZXQ@192.0.2.1:8388", "want a vless:// link"},
		"dynamic key for vless":  {config.KindVLESS, "ssconf://keys.example/" + vlessID, "want a vless:// link"},
		"vless link for outline": {config.KindOutline, link, "unsupported scheme"},
	} {
		t.Run(name, func(t *testing.T) {
			os.WriteFile(path, []byte(tc.source), 0o600)
			_, err := LoadUpstream(config.Upstream{Name: "vl", Kind: tc.kind, Conf: path}, config.Default().Health)
			if err == nil || !strings.HasPrefix(err.Error(), "upstream vl: ") || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), vlessID) {
				t.Errorf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
