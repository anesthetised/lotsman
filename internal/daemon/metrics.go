package daemon

import (
	"time"

	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/metrics"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// Families lists every metric the daemon exposes.
var Families = []metrics.Family{
	{Name: "lotsman_build_info", Type: "gauge", Help: "Build version; always 1."},
	{Name: "lotsman_client_mtu", Type: "gauge", Help: "MTU written into client configs."},
	{Name: "lotsman_upstream_up", Type: "gauge", Help: "1 while the upstream passes its health probes."},
	{Name: "lotsman_upstream_probe_latency_seconds", Type: "gauge", Help: "Latency of the last successful probe."},
	{Name: "lotsman_upstream_probes_total", Type: "counter", Help: "Probes by result."},
	{Name: "lotsman_upstream_last_handshake_timestamp_seconds", Type: "gauge", Help: "Unix time of the last handshake with the provider; 0 if none yet."},
	{Name: "lotsman_upstream_clients", Type: "gauge", Help: "Clients currently routed through the upstream."},
	{Name: "lotsman_upstream_receive_bytes_total", Type: "counter", Help: "Bytes received from the provider."},
	{Name: "lotsman_upstream_transmit_bytes_total", Type: "counter", Help: "Bytes sent to the provider."},
	{Name: "lotsman_peer_receive_bytes_total", Type: "counter", Help: "Bytes received from the client."},
	{Name: "lotsman_peer_transmit_bytes_total", Type: "counter", Help: "Bytes sent to the client."},
	{Name: "lotsman_peer_last_handshake_timestamp_seconds", Type: "gauge", Help: "Unix time of the client's last handshake; 0 if none yet."},
	{Name: "lotsman_peer_upstream", Type: "gauge", Help: "1 for the upstream the client is routed through."},
	{Name: "lotsman_reroutes_total", Type: "counter", Help: "Times a client was routed to the upstream, including the first time."},
	{Name: "lotsman_reloads_total", Type: "counter", Help: "Configuration reloads by result."},
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
	}

	clients := map[string]int{}
	for _, name := range d.assignments {
		clients[name]++
	}
	for _, u := range d.ups {
		up := metrics.Label{Name: "upstream", Value: u.Name}
		out = append(out,
			sample("lotsman_upstream_up", boolean(u.Tracker.State() == health.Up), up),
			sample("lotsman_upstream_probe_latency_seconds", u.Tracker.Latency().Seconds(), up),
			sample("lotsman_upstream_clients", float64(clients[u.Name]), up),
			sample("lotsman_reroutes_total", float64(d.reroutes[u.Name]), up),
		)
		if c := d.probes[u.Name]; c != nil {
			out = append(out,
				sample("lotsman_upstream_probes_total", float64(c[0]), up, metrics.Label{Name: "result", Value: "ok"}),
				sample("lotsman_upstream_probes_total", float64(c[1]), up, metrics.Label{Name: "result", Value: "fail"}),
			)
		}
		if peers, err := u.Device.Peers(); err == nil && len(peers) == 1 {
			out = append(out,
				sample("lotsman_upstream_last_handshake_timestamp_seconds", unixOrZero(peers[0].LastHandshake), up),
				sample("lotsman_upstream_receive_bytes_total", float64(peers[0].RxBytes), up),
				sample("lotsman_upstream_transmit_bytes_total", float64(peers[0].TxBytes), up),
			)
		}
	}

	if d.down == nil {
		return out
	}
	status := map[string]tunnel.PeerStatus{}
	if peers, err := d.down.Peers(); err == nil {
		for _, p := range peers {
			status[p.PublicKey.Hex()] = p
		}
	}
	for key, p := range d.devicePeers {
		labels := []metrics.Label{{Name: "user", Value: p.User}, {Name: "device", Value: p.Device}, {Name: "profile", Value: p.Profile}}
		if st, ok := status[key.Hex()]; ok {
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
