package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/proxy"
	"github.com/anesthetised/lotsman/internal/store"
)

func pidPath(stateDir string) string    { return filepath.Join(stateDir, "lotsman.pid") }
func statusPath(stateDir string) string { return filepath.Join(stateDir, "status.json") }
func dbPath(stateDir string) string     { return filepath.Join(stateDir, "lotsman.db") }

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
	fmt.Fprintln(w, "UPSTREAM\tKIND\tINTERFACE\tSTATE\tLATENCY\tLAST HANDSHAKE\tROUTED\tACTIVE\tSPEED MBIT/S\tINCIDENTS")
	for _, u := range s.Upstreams {
		handshake := ago(u.LastHandshake)
		if (config.Upstream{Kind: u.Kind}).Proxy() {
			handshake = "-" // proxies have no handshake of their own
		}
		state := u.State.String()
		if u.Degraded && u.State == health.Up {
			state = "degraded" // reachable, but failing its transfer probes
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", u.Name, u.Kind, u.Interface, state, u.Latency.Round(time.Millisecond), handshake,
			u.Routed, u.Active, speedText(u.Speed), incidentsText(u.Incidents))
	}
	return w.Flush()
}

// speedText is a speed test's rates as down/up in megabits per second, or
// "-" before the first test.
func speedText(s health.Speed) string {
	if s.Time.IsZero() {
		return "-"
	}
	mbits := func(rate uint64) string { return strconv.FormatFloat(float64(rate)*8/1e6, 'f', 1, 64) }
	return mbits(s.Down) + "/" + mbits(s.Up)
}

// incidentsText lists the kinds of open incidents, or "-" for none.
func incidentsText(kinds []store.IncidentKind) string {
	if len(kinds) == 0 {
		return "-"
	}
	s := make([]string, len(kinds))
	for i, k := range kinds {
		s[i] = string(k)
	}
	return strings.Join(s, ",")
}

// DefaultIncidents is how many incidents `lotsman incidents` lists without -n.
const DefaultIncidents = 50

// incidents lists incidents from the store, which the daemon writes as they
// open and close.
func incidents(configPath string, args []string) error {
	fs := flag.NewFlagSet("incidents", flag.ContinueOnError)
	upstream := fs.String("upstream", "", "only this upstream's incidents")
	n := fs.Int("n", DefaultIncidents, "how many to list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("incidents: unexpected argument %q", fs.Arg(0))
	}
	if *n < 1 {
		return fmt.Errorf("incidents: -n must be at least 1")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(dbPath(cfg.StateDir))
	if err != nil {
		return err
	}
	defer st.Close()
	list, err := st.Incidents(*upstream, *n)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// The detail goes last: it has spaces.
	fmt.Fprintln(w, "UPSTREAM\tKIND\tSTARTED\tDURATION\tDEVICES\tDETAIL")
	for _, in := range list {
		duration := "ongoing"
		if !in.Open() {
			duration = in.Ended.Sub(in.Started).String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", in.Upstream, in.Kind, in.Started.Local().Format("2006-01-02T15:04:05"), duration, in.Devices, in.Detail)
	}
	return w.Flush()
}

func checkConfig(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	// Dynamic keys first, each bounded like the daemon's fetch, so a slow
	// URL does not eat the lookup budget below.
	ups := make([]*daemon.Upstream, len(cfg.Upstreams))
	fetchErrs := make([]error, len(cfg.Upstreams))
	for i, u := range cfg.Upstreams {
		if ups[i], err = daemon.LoadUpstream(u, cfg.Health); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), max(cfg.Health.Interval/2, 5*time.Second))
		fetchErrs[i] = ups[i].Fetch(ctx)
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var warnings []string
	for i, u := range cfg.Upstreams {
		up := ups[i]
		if err := fetchErrs[i]; err != nil {
			// Not fatal, as for the daemon, which runs this before it starts.
			fmt.Printf("upstream %-10s %s  kind %s  dynamic key not fetched\n", u.Name, u.InterfaceName(), u.Kind)
			warnings = append(warnings, fmt.Sprintf("%v; the daemon uses the key it cached last, if any, or keeps the upstream down until a fetch succeeds", err))
			continue
		}
		if err := up.Resolve(ctx); err != nil {
			return err
		}
		line := fmt.Sprintf("upstream %-10s %s  kind %s  geo %s  provider %s  endpoint %s  address %s  mtu %d",
			u.Name, u.InterfaceName(), u.Kind, u.Geo, u.Provider, up.Resolved, up.Addr, up.MTU)
		if up.Key != nil {
			// The key's description never includes its secret.
			if up.Location != nil {
				line += "  dynamic"
			}
			line += fmt.Sprintf("  %s  max_flows %d", up.Key, cmp.Or(u.MaxFlows, proxy.DefaultMaxFlows))
		} else {
			line += fmt.Sprintf("  s4 %d", up.Conf.Interface.S4)
		}
		fmt.Println(line)
	}
	health := "health   probe " + cfg.Health.Probe.String()
	if slices.ContainsFunc(cfg.Upstreams, config.Upstream.Proxy) {
		health += "  dns " + cfg.Health.DNS.String()
	}
	fmt.Println(health)
	if t := cfg.Health.Transfer; t != nil {
		upload := ""
		if t.UploadURL != "" {
			upload = fmt.Sprintf("  upload_url %s  upload_bytes %d", t.UploadURL, t.UploadBytes)
		}
		fmt.Printf("transfer url %s  min_bytes %d%s  timeout %s  interval %s ±20%%\n", t.URL, t.MinBytes, upload, t.Timeout, t.Interval)
	}
	if s := cfg.Health.Speed; s != nil {
		fmt.Printf("speed    down_url %s  up_url %s  bytes %d  timeout %s  interval %s ±20%%\n", s.DownURL, s.UpURL, s.Bytes, s.Timeout, s.Interval)
	}
	fmt.Printf("incidents send_errors_per_minute %d\n", cfg.Incidents.SendErrorsPerMinute)
	for _, w := range warnings {
		fmt.Println("warning:", w)
	}
	fmt.Println("config OK")
	return nil
}
