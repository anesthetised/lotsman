package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
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

func writeStatus(stateDir string, s statusFile) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := statusPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statusPath(stateDir))
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
	fmt.Fprintln(w, "UPSTREAM\tINTERFACE\tSTATE\tLATENCY\tLAST HANDSHAKE\tROUTED\tACTIVE")
	for _, u := range s.Upstreams {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n", u.Name, u.Interface, u.State, u.Latency.Round(time.Millisecond), ago(u.LastHandshake), u.Routed, u.Active)
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
		up, err := daemon.LoadUpstream(ctx, u, cfg.Health)
		if err != nil {
			return err
		}
		fmt.Printf("upstream %-10s %s  endpoint %s  address %s  mtu %d  s4 %d\n",
			u.Name, u.InterfaceName(), up.Conf.Peers[0].Endpoint, up.Addr, up.MTU, up.Conf.Interface.S4)
	}
	for _, p := range cfg.Profiles {
		fmt.Printf("profile  %-10s prefer %v\n", p.Name, p.Prefer)
	}
	fmt.Println("config OK")
	return nil
}
