package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/proxy"
)

func pidPath(stateDir string) string    { return filepath.Join(stateDir, "lotsman.pid") }
func statusPath(stateDir string) string { return filepath.Join(stateDir, "status.json") }

// statusFile is what the daemon writes after every health round.
type statusFile struct {
	UpdatedAt time.Time               `json:"updated_at"`
	ClientMTU int                     `json:"client_mtu"`
	Upstreams []daemon.UpstreamStatus `json:"upstreams"`
	Peers     []daemon.PeerStatus     `json:"peers"`
}

// readStatus returns the last status the daemon wrote, or an error when
// there is none (daemon not running or not ready yet).
func readStatus(stateDir string) (statusFile, error) {
	var s statusFile
	data, err := os.ReadFile(statusPath(stateDir))
	if err != nil {
		return s, fmt.Errorf("no status yet (is the daemon running?): %w", err)
	}
	return s, json.Unmarshal(data, &s)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}

func upstreamStatus(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	s, err := readStatus(cfg.StateDir)
	if err != nil {
		return err
	}
	fmt.Printf("updated %s ago, client MTU %d\n", time.Since(s.UpdatedAt).Round(time.Second), s.ClientMTU)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "UPSTREAM\tKIND\tINTERFACE\tSTATE\tLATENCY\tLAST HANDSHAKE\tROUTED\tACTIVE")
	for _, u := range s.Upstreams {
		handshake := ago(u.LastHandshake)
		if (config.Upstream{Kind: u.Kind}).Proxy() {
			handshake = "-" // proxies have no handshake of their own
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n", u.Name, u.Kind, u.Interface, u.State, u.Latency.Round(time.Millisecond), handshake, u.Routed, u.Active)
	}
	return w.Flush()
}

func checkConfig(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, u := range cfg.Upstreams {
		up, err := daemon.LoadUpstream(u, cfg.Health)
		if err != nil {
			return err
		}
		if err := up.Resolve(ctx); err != nil {
			return err
		}
		line := fmt.Sprintf("upstream %-10s %s  kind %s  endpoint %s  address %s  mtu %d",
			u.Name, u.InterfaceName(), u.Kind, up.Resolved, up.Addr, up.MTU)
		if up.Key != nil {
			// The key's description never includes its secret.
			line += fmt.Sprintf("  %s  max_flows %d", up.Key, cmp.Or(u.MaxFlows, proxy.DefaultMaxFlows))
		} else {
			line += fmt.Sprintf("  s4 %d", up.Conf.Interface.S4)
		}
		fmt.Println(line)
	}
	for _, p := range cfg.Profiles {
		fmt.Printf("profile  %-10s prefer %v\n", p.Name, p.Prefer)
	}
	health := "health   probe " + cfg.Health.Probe.String()
	if slices.ContainsFunc(cfg.Upstreams, config.Upstream.Proxy) {
		health += "  dns " + cfg.Health.DNS.String()
	}
	fmt.Println(health)
	for _, w := range cfg.Warnings() {
		fmt.Println("warning:", w)
	}
	fmt.Println("config OK")
	return nil
}
