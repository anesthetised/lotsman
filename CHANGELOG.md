# Changelog

All notable changes to Lotsman are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow SemVer.

## [Unreleased]

### Fixed
- Honor the downstream `listen` address and address family. The default
  `0.0.0.0:51820` now opens only IPv4; `[::]:51820` opens only IPv6 (#13).
- Bind health probes and their policy routes to the intended upstream interface,
  so providers with identical tunnel IPs retain independent health and failover (#10).
- Reject missing upstream peer keys, unsafe active junk ranges/counts, and MTUs
  that exceed the padding-adjusted tunnel or UDP limits during configuration loading (#19).
- Validate advertised endpoints before creating users or rendering client configs:
  require a nonempty host and a numeric UDP port from 1 to 65535 (#21).
- Reject client subnet prefixes with host bits set before creating users or peers;
  report the canonical network prefix instead of failing later during IP allocation (#20).
- Disable downstream ingress before removing routing and firewall protection on shutdown,
  including failed startup. Retain protection if disabling fails and retry cleanup after closing
  the downstream device, before closing upstreams (#8).
- Deny peers without an upstream assignment in nftables, including upstream connected subnets;
  revoke forwarding before route cleanup so stale rules cannot keep a peer connected (#14).
- `user add`, `user show` and `user rm` reject unexpected arguments instead of ignoring them;
  `user rm alice phone -profile nl` used to delete the whole user (#15).
- Create the initial server identity atomically, so concurrent first starts share one complete
  identity and never overwrite an existing key (#16).
- A peer that reuses a freed IP is routed in the same reconcile that revokes the old peer's key;
  if the old peer's cleanup fails, the new peer waits for the retry (#17).
- Upstreams added or replaced on reload use the incoming `health` thresholds (#18).
- Block clients whose profile is removed on reload, clearing their routes, flow pins, and
  reported assignment even when their upstream remains configured (#6).
- Retry failed route and flow-pin cleanup after upstream failure, peer removal, and upstream
  removal or replacement on reload, instead of forgetting stale forwarding state (#7).
- The database, its WAL/SHM files and migration backups are created with mode 0600, and existing
  ones are tightened on start. Other local users and tools can no longer read them (#11).
- Retry a failed health update so flow pins to a dead upstream are eventually removed (#12).
- The Linux dataplane and `uninstall.sh` delete and overwrite only IP rules and routes of
  Lotsman's exact shape in routing tables 1000-1099 that hold nothing else. Occupied tables are
  skipped, and startup fails if none is free (#9).

## [0.2.0] - 2026-09-18

### Added
- `upstream status` shows `ROUTED` and `ACTIVE` clients (handshake within 3 minutes) instead of
  the ambiguous `CLIENTS`; `user list` shows each client's upstream and last handshake;
  `lotsman_upstream_active_clients` metric.
- The status file is written right after the first reconcile instead of one health interval later.
- Prometheus metrics endpoint, enabled with `metrics.listen` (#3): upstream health, probe
  latency and counts, handshake times, byte counters per upstream and per client, current
  upstream per client, reroute and reload counters.

## [0.1.2] - 2026-09-18

### Added
- Flow pinning: connections already open through a live upstream stay on it when a client is
  moved; only new connections change exit.
- Hostname endpoints are re-resolved when an upstream's probes fail, so a provider changing its
  IP no longer needs a restart.
- End-to-end tests cover reload without changes and with an edited provider file.

## [0.1.1] - 2026-09-18

### Fixed
- Reload replaced an unchanged upstream on every SIGHUP when its provider file used a hostname
  endpoint.
- A replaced upstream's recreated interface was left without address, route or link up.

## [0.1.0] - 2026-09-18

First release: AmneziaWG 2.0 gateway with profile-based upstream selection, health checks with
hysteresis, fail-closed forwarding, per-device client configs with QR output, live configuration
reload, database backups before migrations, systemd unit and Docker image.

[Unreleased]: https://github.com/anesthetised/lotsman/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/anesthetised/lotsman/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/anesthetised/lotsman/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/anesthetised/lotsman/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/anesthetised/lotsman/releases/tag/v0.1.0
