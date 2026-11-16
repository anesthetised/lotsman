# Changelog

All notable changes to Lotsman are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow SemVer.

## [Unreleased]

### Added
- Dynamic Outline keys: an `outline` upstream's key file may hold an `ssconf://` (or `https://`) URL.
  The key is fetched over verified HTTPS (redirects to `https://` only) at start, on every reload,
  and when the upstream fails its probes, at most every 5 minutes; a changed key applies to the
  running upstream without reopening it. The response may be an `ss://` key or Outline's JSON key,
  and a provider's `error` object fails the fetch. The last good key is cached in
  `state_dir/ssconf/<name>.json` (0600) for restarts while the URL is unreachable; without a fetch
  or a cached key the upstream stays down and is retried, and the daemon still starts. The URL is
  never logged (#61).
- Transfer probe for throttled upstreams, off unless `health.transfer` is set: every `interval`
  (default 5m, ±20% per upstream) each reachable upstream downloads `min_bytes` (default 64 KiB)
  from `url` (one URL or a list; no built-in default) within `timeout` (default 10s), bound to the
  upstream like the other probes; after a failure and while degraded, every fifth of `interval`
  (at least `health.interval`). `down_after` failures mark it degraded and `up_after` passes
  clear it. A degraded upstream is a last resort: clients use it only when their profile has no
  other healthy upstream; once nobody is routed through it, its pinned flows are released as when
  it is down. `upstream status` shows `degraded`, `config check` prints the settings, and
  `lotsman_upstream_degraded`, `lotsman_upstream_transfer_seconds` and
  `lotsman_upstream_transfer_probes_total` are new metrics (#87).
- Outline (Shadowsocks) upstreams: `kind: outline` with `conf` pointing at a file holding an
  `ss://` access key (SIP002 or legacy, AEAD ciphers, Outline's `prefix`). A userspace TCP/IP stack
  (gVisor) on the upstream's TUN carries TCP and UDP through the server, so profiles, failover, flow
  pinning and fail-closed work as for AmneziaWG; ICMP does not pass. Health is a DNS query over UDP
  and over TCP to `health.dns` (default `1.1.1.1:53`); `max_flows` (default 4096) caps flows per
  upstream. `config check`, `upstream status` and the new `lotsman_upstream_info` metric show the
  kind; `lotsman_upstream_flows` and `lotsman_upstream_flows_refused_total` cover the stack (#61).
- `awg: 2 | 3` selects the AmneziaWG generation of the listener, default 3: the identity gains a
  header protection key, and client configs carry `HeaderProtectionKey` and `RandomTrailers = on`
  (Amnezia app 5.0.1.5 or later). Restart only (#60).
- Upstreams accept AmneziaWG 3.x provider configs: `HeaderProtectionKey`,
  `ContentPaddingAddition`, `RekeyAfterTime`, `RekeyTimeout`, `RejectAfterTime`,
  `KeepaliveTimeout`, `MaxHandshakeAttempts`, `RandomTrailers` and `DisableCookies`. The client
  MTU subtracts each upstream's `ContentPaddingAddition` upper bound (#59).
- `health.probe` accepts a list of `ip:port` targets; a probe round succeeds if any target
  answers, and latency is the fastest answer (#51).
- `tags` on upstreams and `tag` in profile tiers, so several countries can form one tier picked by
  latency. A matcher with several fields requires all of them; warnings and `config check` print
  matchers in the config's own spelling, such as `{geo: NL, tag: eu}` (#27).
- `-log-level` flag and `LOTSMAN_LOG_LEVEL` (`debug`, `info`, `warn`, `error`; default `info`)
  set the daemon's log level; at `debug` the AmneziaWG device logs become visible. An invalid
  value is rejected before any command runs (#56).
- `[Peer] PersistentKeepalive` in provider configs accepts a range `a-b` besides a single value,
  as AmneziaWG 3.x does. Timings take no Lotsman bounds beyond the runtime's (0 selects its
  default); only a `RejectAfterTime` above 3074457345 s is rejected, because the runtime's key
  timer (three times that) overflows and wipes every session's keys at once (#76).

### Changed
- New identities draw S3 and S4 from 12 upward; existing identity files are upgraded on load
  (derived header protection key, S1–S4 raised to 12), which invalidates configs issued before (#60).
- amneziawg-go moves from a master pseudo-version to the tag `amneziawg-go/v3 v3.1.20260828`.
  AWG 1.5-only keys (`J1`–`J3`, `Itime`) are rejected with a message to request a 2.0+ config (#59).
- CI and `just lint` run `go mod verify`, staticcheck and govulncheck for the host and the Linux
  integration build; the tools are pinned with `tool` directives in `go.mod`. The toolchain is
  raised to go1.27.4 for GO-2026-6859 (#54).

### Fixed
- The first data packet of every AmneziaWG device (each upstream, and `lm0` towards clients) went
  out without its S4 transport padding and was dropped by the peer, which delayed an upstream's
  first health probe by a round. The runtime reads S4 before it blocks on the TUN, so the TUN's
  first read now waits for the first configuration (#84).
- Firewall rules removed by another tool (`nft flush ruleset`, a ufw or firewalld reload, a Docker
  or ufw forward chain added later) are reinstalled and every routed client is admitted again,
  instead of everyone staying blocked until a restart; each repair is logged as an error and
  counted in `lotsman_firewall_repairs_total` (#47). Lotsman follows nftables change
  notifications, so the repair comes about 100 ms after the change instead of at the next health
  round, which closes the window in which the host forwarded clients without Lotsman's drop and
  NAT; the health round stays as the fallback (#81).
- Drop everything VPN clients send to the host itself (SSH, Docker bridges, services bound to
  `0.0.0.0`) except a ping to the gateway, the `metrics.listen` endpoint and replies to
  connections the host opened; forwarded traffic is unchanged (#46).
- Upstreams that fail to open on reload, and devices left down after a dataplane error, are
  retried every health round instead of staying out of service until the next SIGHUP; the
  failure is logged once as "not running, will retry" (#52).
- `user add` and `user rm` send SIGUSR1, which only reconciles, instead of SIGHUP, so a
  half-edited `lotsman.yaml` is no longer applied; SIGHUP still reloads, then reconciles. The CLI
  signals only a process running its own executable and explains on stderr when it refuses (#49).
- Removing the last upstream a `prefer` matcher matches no longer fails config load or reload.
  A matcher that matches nothing, or only upstreams an earlier matcher took (the warning names
  it), is a warning in `config check` and the reload log; a profile whose tiers are all empty
  is still an error (#50).
- Resolve provider endpoints outside the daemon lock at startup, on reload and when a failing
  upstream re-resolves, so slow or dead DNS no longer stalls status, metrics, reconciliation or
  reloads. Every endpoint lookup times out after the probe timeout (half the health interval, at
  least 1 s). SIGHUP is handled only once the daemon is ready, so a reload or `user add` during
  startup waits instead of racing it (#44).
- Keep a running upstream when its changed provider config fails to read, parse, validate or
  resolve on reload; only TUN creation happens after the old device is closed. `user add` and
  `user show` no longer resolve provider endpoints, so they work while DNS is down; `config check`
  still resolves them (#45).
- Assume an IPv6 outer header (80 bytes of encapsulation overhead) for every client MTU,
  so clients that reach Lotsman over IPv6 no longer get a 20-byte-too-large MTU; the
  client MTU without a tighter upstream drops from 1440 to 1420 (#43).
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
- Reject provider configs before device startup when S1, S2 or S3 plus their 148/92/64-byte
  handshake message exceed the endpoint family's UDP payload limit (65507 IPv4, 65527 IPv6),
  or when an I1–I5 chain has a negative tag size, a packet size above that limit, or a size
  sum that overflows. Strings the runtime ignores or refuses to build stay accepted (#32).
- Disable downstream ingress before removing routing and firewall protection on shutdown,
  including failed startup. Retain protection if disabling fails and retry cleanup after closing
  the downstream device, before closing upstreams (#8).
- Deny peers without an upstream assignment in nftables, including upstream connected subnets;
  revoke forwarding before route cleanup so stale rules cannot keep a peer connected (#14).
- `user add`, `user show` and `user rm` reject unexpected arguments instead of ignoring them;
  `user rm alice phone -profile nl` used to delete the whole user (#15).
- `version`, `serve`, `config check`, `user list` and `upstream status` reject unexpected
  arguments and flags instead of ignoring them (#29).
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
