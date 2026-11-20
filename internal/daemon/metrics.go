package daemon

import (
	"time"

	"github.com/anesthetised/lotsman/internal/awgconf"

	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/metrics"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// Families lists every metric the daemon exposes.
var Families = []metrics.Family{
	{Name: "lotsman_build_info", Type: "gauge", Help: "Build version; always 1."},
	{Name: "lotsman_client_mtu", Type: "gauge", Help: "MTU written into client configs."},
	{Name: "lotsman_upstream_info", Type: "gauge", Help: "The upstream's kind and interface; always 1."},
	{Name: "lotsman_upstream_up", Type: "gauge", Help: "1 while the upstream passes its health probes."},
	{Name: "lotsman_upstream_flows", Type: "gauge", Help: "TCP and UDP flows a proxy upstream carried at the last health round."},
	{Name: "lotsman_upstream_flows_refused_total", Type: "counter", Help: "Flows a proxy upstream refused at its max_flows limit."},
	{Name: "lotsman_proxy_restarts_total", Type: "counter", Help: "Times a proxy upstream's helper process exited on its own, a crash included; each is restarted."},
	{Name: "lotsman_upstream_probe_latency_seconds", Type: "gauge", Help: "Latency of the last successful probe."},
	{Name: "lotsman_upstream_probes_total", Type: "counter", Help: "Probes by result."},
	{Name: "lotsman_upstream_degraded", Type: "gauge", Help: "1 while the upstream is reachable but fails its transfer probes."},
	{Name: "lotsman_upstream_transfer_seconds", Type: "gauge", Help: "Duration of the last transfer probe each way, a failed one included."},
	{Name: "lotsman_upstream_transfer_probes_total", Type: "counter", Help: "Transfer probes by direction and result."},
	{Name: "lotsman_upstream_send_errors_total", Type: "counter", Help: "Errors an AWG upstream's device logged, such as failed sends."},
	{Name: "lotsman_upstream_last_handshake_timestamp_seconds", Type: "gauge", Help: "Unix time of the last handshake with the provider; 0 if none yet."},
	{Name: "lotsman_upstream_clients", Type: "gauge", Help: "Clients whose traffic is routed through the upstream, connected or not."},
	{Name: "lotsman_upstream_active_clients", Type: "gauge", Help: "Routed clients with a handshake in the last 3 minutes."},
	{Name: "lotsman_upstream_receive_bytes_total", Type: "counter", Help: "Bytes received from the provider."},
	{Name: "lotsman_upstream_transmit_bytes_total", Type: "counter", Help: "Bytes sent to the provider."},
	{Name: "lotsman_peer_receive_bytes_total", Type: "counter", Help: "Bytes received from the client."},
	{Name: "lotsman_peer_transmit_bytes_total", Type: "counter", Help: "Bytes sent to the client."},
	{Name: "lotsman_peer_last_handshake_timestamp_seconds", Type: "gauge", Help: "Unix time of the client's last handshake; 0 if none yet."},
	{Name: "lotsman_peer_upstream", Type: "gauge", Help: "1 for the upstream the client is routed through."},
	{Name: "lotsman_reroutes_total", Type: "counter", Help: "Times a client was routed to the upstream, including the first time."},
	{Name: "lotsman_reloads_total", Type: "counter", Help: "Configuration reloads by result."},
	{Name: "lotsman_firewall_repairs_total", Type: "counter", Help: "Times firewall rules removed by another tool were reinstalled."},
}

// Metrics is the current snapshot for the Prometheus endpoint.
func (d *Daemon) Metrics() []metrics.Sample {
	d.mu.Lock()
	defer d.mu.Unlock()

	sample := func(name string, value float64, labels ...metrics.Label) metrics.Sample {
		return metrics.Sample{Name: name, Labels: labels, Value: value}
	}
	out := []metrics.Sample{
		sample("lotsman_build_info", 1, metrics.Label{Name: "version", Value: d.version}),
		sample("lotsman_client_mtu", float64(d.clientMTU())),
		sample("lotsman_reloads_total", float64(d.reloads[0]), metrics.Label{Name: "result", Value: "ok"}),
		sample("lotsman_reloads_total", float64(d.reloads[1]), metrics.Label{Name: "result", Value: "error"}),
		sample("lotsman_firewall_repairs_total", float64(d.repairs)),
	}

	now := time.Now()
	handshakes := d.downstreamHandshakes()
	clients, active := map[string]int{}, map[string]int{}
	for key, p := range d.devicePeers {
		name, ok := d.assignments[p.IP]
		if !ok {
			continue
		}
		clients[name]++
		if now.Sub(handshakes[key]) < ActiveWindow {
			active[name]++
		}
	}
	for _, u := range d.ups {
		up := metrics.Label{Name: "upstream", Value: u.Name}
		out = append(out,
			sample("lotsman_upstream_info", 1, up, metrics.Label{Name: "kind", Value: u.Kind}, metrics.Label{Name: "interface", Value: u.InterfaceName()}),
			sample("lotsman_upstream_up", boolean(u.Tracker.State() == health.Up), up),
			sample("lotsman_upstream_degraded", boolean(d.degraded(u)), up),
			sample("lotsman_upstream_probe_latency_seconds", u.Tracker.Latency().Seconds(), up),
			sample("lotsman_upstream_clients", float64(clients[u.Name]), up),
			sample("lotsman_upstream_active_clients", float64(active[u.Name]), up),
			sample("lotsman_reroutes_total", float64(d.reroutes[u.Name]), up),
		)
		if c := d.probes[u.Name]; c != nil {
			out = append(out,
				sample("lotsman_upstream_probes_total", float64(c[0]), up, metrics.Label{Name: "result", Value: "ok"}),
				sample("lotsman_upstream_probes_total", float64(c[1]), up, metrics.Label{Name: "result", Value: "fail"}),
			)
		}
		for _, kind := range []store.IncidentKind{probeDownload, probeUpload} {
			probe := metrics.Label{Name: "probe", Value: string(kind)}
			if c := d.transferCounts[transferCount{u.Name, kind}]; c != nil {
				out = append(out,
					sample("lotsman_upstream_transfer_probes_total", float64(c[0]), up, probe, metrics.Label{Name: "result", Value: "ok"}),
					sample("lotsman_upstream_transfer_probes_total", float64(c[1]), up, probe, metrics.Label{Name: "result", Value: "fail"}),
				)
			}
			if t := d.transfers[u]; t != nil && t.probes[kind] != nil {
				out = append(out, sample("lotsman_upstream_transfer_seconds", t.probes[kind].last.Seconds(), up, probe))
			}
		}
		if u.Key == nil {
			count, _ := u.errs.read()
			out = append(out, sample("lotsman_upstream_send_errors_total", float64(count), up))
		}
		st := u.link.Stats() // a proxy link's counters are the health round's: no helper request under d.mu
		if p := st.Peer; p != nil {
			out = append(out,
				sample("lotsman_upstream_last_handshake_timestamp_seconds", unixOrZero(p.LastHandshake), up),
				sample("lotsman_upstream_receive_bytes_total", float64(p.RxBytes), up),
				sample("lotsman_upstream_transmit_bytes_total", float64(p.TxBytes), up),
			)
		}
		if u.Key != nil {
			out = append(out, sample("lotsman_proxy_restarts_total", float64(d.restarts[u.Name]), up))
		}
		if f := st.Flows; f != nil {
			out = append(out,
				sample("lotsman_upstream_flows", float64(f.Flows), up),
				sample("lotsman_upstream_flows_refused_total", float64(f.Refused), up),
			)
		}
	}

	if d.down == nil {
		return out
	}
	status := map[awgconf.Key]tunnel.PeerStatus{}
	if peers, err := d.down.Peers(); err == nil {
		for _, p := range peers {
			status[p.PublicKey] = p
		}
	}
	for key, p := range d.devicePeers {
		labels := []metrics.Label{{Name: "user", Value: p.User}, {Name: "device", Value: p.Device}}
		if st, ok := status[key]; ok {
			out = append(out,
				sample("lotsman_peer_receive_bytes_total", float64(st.RxBytes), labels...),
				sample("lotsman_peer_transmit_bytes_total", float64(st.TxBytes), labels...),
				sample("lotsman_peer_last_handshake_timestamp_seconds", unixOrZero(st.LastHandshake), labels...),
			)
		}
		if name, ok := d.assignments[p.IP]; ok {
			out = append(out, sample("lotsman_peer_upstream", 1, append(labels, metrics.Label{Name: "upstream", Value: name})...))
		}
	}
	return out
}

func boolean(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unixOrZero(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}
