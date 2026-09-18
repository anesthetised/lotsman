# Changelog

All notable changes to Lotsman are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow SemVer.

## [Unreleased]

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
