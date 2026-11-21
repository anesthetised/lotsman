# Changelog

All notable changes to Lotsman are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow SemVer.

## [Unreleased]

### Added
- Exit checks and an incident log (#167). An optional upload probe (`health.transfer.upload_url`,
  `upload_bytes`) runs beside the download probe and, failing, makes the upstream degraded the
  same way; optional speed tests (`health.speed`) record each upstream's throughput both ways in
  its status and history; AmneziaWG send errors are counted per upstream
  (`lotsman_upstream_send_errors_total`). Down, download, upload and send-error trouble opens an
  incident per upstream and kind, closed when it clears and kept as long as the hourly history:
  `lotsman incidents`, `IncidentService.List`, and in the console an Incidents page with the open
  count in the nav, incident badges on the overview, and the speed, a speed chart and the
  incidents on each upstream's page. The transfer probe metrics gain a `probe` label
  (`download`, `upload`).
- `lotsman profile rename OLD NEW`: renames a profile and moves its peers with it in one
  transaction; their keys, addresses and history stay, so their configs keep working. Profile names
  may now be capitalized (`Netherlands`) and are unique ignoring case; tags stay lowercase. The
  daemon reads peers and profiles in one transaction, so a rename never leaves a peer unrouted for a
  pass, and its status shows the new name at once.
- Web panel deployment guide (#138): the operations guide covers the install permissions
  (`/etc/lotsman` `0750 root:lotsman-web`; the base install now creates `lotsman.yaml` `0600` and
  `upstreams/` `0700`), recovering a unit stopped by
  its start limit (`systemctl reset-failed`), Caddy and nginx examples with the reason for
  `flush_interval -1` and `proxy_buffering off`, what `trusted_proxies` needs to list, checking
  the install, what changing `public_url`'s host does to passkeys and to links already sent, backing
  up `web.db` (WAL mode: `sqlite3 .backup` or stop the unit first), and removing a Caddy installed
  for the panel through the manifest's `package=` entries. Checked by hand on the test gateway
  with Caddy. No code change.
- Web panel: the user page behind the secret link (#137) — devices with QR configs, `.conf`
  download and copy; own live traffic and its 24 h and 7 d history; the upstreams in use with their
  7-day availability; adding and removing devices and switching their profile; import
  instructions for the Amnezia app. Mobile first, RU and EN.
- Web panel: operator console (#136) — upstream pages with history, profiles, users and devices
  with QR configs and links, live connections. `/admin` lists the upstreams (state, latency, live
  rate, 7-day availability); an upstream's page adds its routed peers and history over 1 hour to
  90 days. **Profiles** creates, edits and deletes profiles with the daemon's warnings; **Users**
  adds and removes devices and users, shows a device's config as a QR code, text or `.conf`, and
  issues (shown once) or revokes a user's link; **Connections** shows a peer's or every peer's
  open connections live, never stored. RU and EN, keyboard accessible, 1280 px down to tablet
  width. New frontend dependency `lean-qr` 2.7.4 (about 4 KB gzipped), which also draws the user
  page's QR codes in place of `uqr`; only the pages that show QR codes load it.
- `UserSelfService.AddDevice` refusals with `resource_exhausted` carry a `SelfServiceLimit` detail
  (`REASON_MAX_DEVICES` with `max_devices`, or `REASON_RATE_LIMITED` with `retry_after`), so clients
  tell the two limits apart without reading the message; the user page says how long to wait.
- Profiles and live connections in the control API. `ProfileService` `List`, `Put` and `Delete`
  serve the stored profiles with the checks and warnings of `lotsman profile` (#128): a profile the
  checks refuse is `invalid_argument`, deleting one that peers use `failed_precondition`, a missing
  one `not_found`, and other failures `internal` without detail. `ConnectionService.Watch` streams
  a peer's or every peer's conntrack flows at once and then every `interval` (default and minimum
  1 s), each stream with its own rates, endpoints as `IP:port` (bare IP for ICMP); an unknown
  `peer_id` is `not_found`, a failed conntrack read `unavailable` (#131). No new dependency.
- Availability and traffic history (#130): the daemon writes each upstream's up ratio and latency
  and each upstream's and peer's bytes (down towards the client) once a minute, in one
  transaction, into `lotsman.db` (migration 5; a backup of the old database is made as for any
  migration), and serves them through `HistoryService.Upstream` and `.Peer` (name = upstream,
  or `p_<id>`) as samples of a step: one minute for ranges within `history.minute_retention`
  (default `168h`), one hour beyond; the requested `step` is rounded up to a multiple of that and
  until at most 2000 samples come back. Minute rows fold into hour rows after `minute_retention`
  and hour rows are dropped after `history.hour_retention` (default `2160h`), hourly, in one
  transaction. A degraded upstream counts as not up. About 15 MiB for 5 upstreams and 50 peers
  at the defaults (see the operations guide). No new dependency.
- Operator sign-in for the web panel with passkeys (#133). `lotsman web enroll` prints a one-time
  link (`<public_url>/enroll#<token>`, 15 minutes, stored hashed in `web.db`) that enrolls a
  passkey; any operator can issue more from the console's **Passkeys** page, which also lists and
  revokes them. All operators are equal. Sessions are a `__Host-` cookie (`HttpOnly`, `Secure`,
  `SameSite=Strict`) that ends after 1 hour idle, 12 hours in all, on sign-out or when its passkey
  is revoked. Sign-in and enrollment are limited per client address (10, then one per 30 s). The
  RP ID is `public_url`'s host, which must now be a name, not an IP address; changing it means
  enrolling every passkey again. The endpoints are the panel's own, under `/auth/`. Frontend:
  `/login`, `/enroll` and `/admin/passkeys`; the console sends a signed-out browser to `/login`.
  Back up `web.db`. New dependency `github.com/go-webauthn/webauthn` v0.18.2 (and 8 modules it
  needs); the binary grows by 2.5 MB.
- User links and device self-service (#134). `lotsman user link NAME` prints a new link to the
  user's page in the web panel, `<public_url>/u#<token>` (the address from `-url` or the panel's
  `public_url`, else the token alone), and revokes the previous one; `-revoke` ends it. The token is
  in the fragment, which browsers never send, so it stays out of proxy access logs. Tokens are
  256-bit, stored as SHA-256 only (a new `user_links` table, migration 6, made automatically),
  shown once and never logged. The page (frontend route `/u`) keeps the token in memory and sends
  it as `Authorization: Bearer`; the panel resolves it through
  the daemon, caches it for 30 s, ends a live view whose link was revoked, refuses an address after
  10 failed links (one more every 6 s), and never forwards the header. The control API implements
  `UserService` (list, add and remove devices and users, configs, issue and revoke links) and
  `UserSelfService`: a user sees and changes only their own devices (another user's peer is
  `not_found`), may add devices up to `users.max_devices` (new in `lotsman.yaml`, default 5; `0`
  turns adding off, another profile on a device included) and at most 10 at once, then one every 6
  minutes, and their `Watch` shows their own peers and the upstreams their profiles may use, without
  those upstreams' routed, active and traffic. `UpstreamHistory` serves those upstreams' up ratio
  and latency, without bytes, and `PeerHistory` the user's own peers, with the rules and codes of
  `HistoryService`; any other name is `not_found` as a missing one is. Changes made through the API
  apply at once, without the CLI's SIGUSR1.
- Web panel frontend scaffold in `web/` (#135): Solid 2, @solidjs/router, Kobalte 2 alpha (patched
  for a dismiss bug, behind `web/src/ui`), Tailwind 4, uPlot, RU and EN, light and dark themes. The
  Connect-web client is generated from `api/` (`just web-gen`); a deterministic fake daemon serves
  development and tests. Routes `/admin` and `/u`. `just web-install|web-dev|web-build|web-test`
  and a `web` CI job (frozen lockfile, typecheck, Vitest, build, codegen check, Playwright smoke under
  the production CSP). The build goes to `internal/web/dist`, which only tracks `.gitkeep`. Go
  builds need no Node.
- Web panel process, `lotsman web` (#132): runs as the unprivileged user `lotsman-web`
  (`deploy/lotsman-web.service`, no capabilities, `StateDirectory=lotsman-web`) with its own
  config, `/etc/lotsman/web.yaml` (`listen`, default `127.0.0.1:8080`; `public_url`;
  `trusted_proxies`; `socket`; `state_dir`; see `deploy/web.example.yaml`). It serves the embedded
  UI from `internal/web/dist`, or a placeholder page when the binary was built without
  `just web-build`, with single-page-app fallback, and forwards `/api/<procedure>` to the daemon's
  control socket, streams included, after checking the RPC's scope (operator, user, internal or
  public; an RPC without one is refused). User links are not implemented yet, so user calls answer
  `unauthenticated`. API calls need `Origin` equal to `public_url`;
  `X-Forwarded-For` counts only from `trusted_proxies`; only RPC protocol headers reach the daemon,
  so a browser's `Lotsman-*` headers never do; every response carries a strict CSP,
  `frame-ancestors 'none'`, `Referrer-Policy: no-referrer` and `nosniff`. Request bodies are
  limited to 64 KiB. The panel keeps `web.db` (`0600`) in its state directory with the tables for
  passkeys, sessions and enrollment tokens. Install: `useradd --system --gid lotsman-web …
  lotsman-web`, and `user=lotsman-web` plus the unit and `/var/lib/lotsman-web` in the manifest;
  `deploy/uninstall.sh` also stops `lotsman-web`. The operations guide has Caddy and nginx
  examples. The binary grows by about 0.2 MB. No new dependency.
- Live traffic in the control API: `StatusService.Watch` fills `traffic` on every peer and
  upstream and the snapshot's `total`: bytes since the daemon started and bytes per second over the
  last second, down meaning towards the client. Peers count their tunnel's packets on the
  downstream device; upstreams of every kind count the packets crossing their `lm-up-*` interface,
  read from the kernel over netlink, so a proxy helper needs no change and a helper restart loses
  nothing. One sampler in the daemon reads every counter each second for all streams; it keeps
  totals across counter restarts and series that come back, and keeps removed peers' bytes in
  the total. About 0.4 ms of CPU per second with 50 peers and two upstreams; the binary grows
  by 24 KB. `lotsman status` is unchanged (#129).
- Control API for the web panel: `lotsman serve` serves `lotsman.v1` over Connect on the unix socket
  `/run/lotsman/control.sock` (`0660 root:lotsman-web`; root-only with a warning when the group is
  missing). The socket's peer is trusted, so its file mode is the boundary. It is created after the
  instance locks, replaces one a crashed daemon left, refuses one that still answers, and is
  removed on stop. The whole agreed contract is committed in `api/lotsman/v1/` with its generated
  Go code (`just gen`; `just lint` fails on stale code). `StatusService.Watch` sends a full
  snapshot at once and then every `interval` (default and minimum 1 s), with each upstream's
  state, latency, handshake, `geo`, `provider`, `tags` and client counts, each peer's opaque
  `peer_id`, tunnel address, upstream and handshake, and the client MTU. RPCs whose issue has not
  landed return `unimplemented`. Install: `groupadd --system lotsman-web` and `group=lotsman-web` in the
  manifest, which `deploy/uninstall.sh` now removes; the unit gains `RuntimeDirectory=lotsman` and
  `CAP_CHOWN`. The binary grows by about 0.7 MB. New module: `connectrpc.com/connect/v2` (#127).
- Live connections from conntrack (#131): the new
  `internal/flows` package reads the kernel's conntrack table over netlink, keeps clients' flows
  into the internet, maps each to its peer and current upstream, and computes per-flow byte rates
  from the deltas between polls (down = towards the client), at most 500 flows per snapshot,
  fastest first. `serve` turns on `net.netfilter.nf_conntrack_acct` once its firewall is up, and
  logs a warning and carries on without it; `deploy/uninstall.sh` restores the
  `nf_conntrack_acct_before=` value of the manifest. Connections are live only: never stored or
  logged, numeric destinations, no reverse DNS. No new dependency.
- `lotsman profile list|add|set|rm|import`: profiles live in the database and change without a
  restart. Each argument after the name is one preference tier, best first: `key=value` pairs
  joined by commas (`geo`, `provider`, `tag`) or `any`; no tiers means any upstream. `add` and
  `set` validate against the upstreams in `lotsman.yaml` as the config did and report the same
  warnings; the running daemon reroutes the profile's clients at once (SIGUSR1, as for `user add`).
  `rm` refuses while peers use the profile. `import FILE` copies the `profiles:` key of an older
  config, all or nothing, and skips profiles already stored. Schema migration 3 adds the
  `profiles` and `profile_tiers` tables (#128).
- VLESS over gRPC: a `vless` upstream's link may use `type=grpc` with `security=reality` and no
  `flow` (Vision runs over TCP only; a grpc link with a flow is refused). `serviceName` (also
  Xray's `/<service>/<stream>` form), `mode` (`gun`, the default, or `multi`) and `authority` are
  read and checked as Xray's loader does, and apply to grpc only; `alpn` is accepted and ignored on
  either transport, as Xray does under REALITY. Every flow is a stream in one HTTP/2 connection to
  the server; UDP goes to ports 53 and 443 as plain VLESS UDP and to others as XUDP. `config check`
  shows `vless reality grpc` (`grpc multi`), never the service name. The binary grows by about
  6.7 MB (grpc-go's HTTP/2 transport); new module `google.golang.org/genproto/googleapis/rpc`
  (#122).
- Outline Shadowsocks-over-WebSocket keys: an `outline` upstream's key file, or a dynamic key's
  response, may be an Outline `transport:` config (YAML or JSON) as the Outline client reads it:
  `tcpudp`, `shadowsocks`, `dial` and `websocket` (`wss://`, `ws://`) endpoints; other `$type`s are
  refused when loading. Each TCP connection and UDP flow gets its own WebSocket, as
  outline-ss-server expects. The URL's host is resolved once like a plain server and is the TLS
  server name and HTTP `Host`; certificates are verified against the system roots (`SSL_CERT_FILE`
  and `SSL_CERT_DIR` now reach the helper processes). TCP and UDP must use the same host; a config
  without a `udp` transport loads but stays down, as UDP is never sent around the server. The URL
  is never logged. New dependency: `github.com/gorilla/websocket` (#61).
- VLESS upstreams: `kind: vless` with `conf` pointing at a file holding a `vless://` share link
  for a VLESS + REALITY + XTLS Vision server over TCP (`flow=xtls-rprx-vision`, or
  `xtls-rprx-vision-udp443` to let QUIC through). They run on the Outline upstreams' stack, in a
  helper process each, with xray-core v26.11.9 embedded: TCP and UDP (XUDP), health by DNS over UDP
  and TCP, `max_flows`, the same metrics, `config check` and `upstream status`. The link's REALITY
  fields are validated as Xray's own loader does; errors and logs never quote the link. Xray's log
  joins the daemon's (`component=xray`), per-flow messages at debug and errors rate-limited.
  Static links only. The binary grows by about 7 MB. gVisor moves to xray's version and
  klauspost/compress to v1.20.2 (GO-2026-6865) (#86).
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
- **Breaking:** `endpoint` must be an IP address and port; a hostname fails `config check` and
  `serve`. Client configs carry it as written, and a client would resolve a name before its tunnel
  is up, through DNS that may be blocked or poisoned, or reach an HTTP-only proxy such as a CDN in
  front of the web panel.
- Web panel: a new device's route is set in the same controls as an existing device's, instead
  of copying another device's route; with devices already there the form waits behind an "Add a
  device" button. The user page's Exits section lists every allowed exit, saying which devices use
  it now or that none does.
- **Breaking:** per-device routes and user rights replace profiles and upstream tags (#161). A
  device is one tunnel with its own route: countries in order (the first with an available exit
  wins) or the fastest of them, each country optionally narrowed to some providers, and
  "only the selected countries" (on by default: when none is available the device is blocked
  rather than sent elsewhere). Routes live on the gateway, so changing one never touches the client
  config. The operator sets per-user rights: whether the user edits their own routes, whether they
  see providers, and which upstreams their devices may use (default all). Upstreams need `geo`
  (ISO 3166-1 alpha-2) and `provider`; `tags:` is rejected. The CLI becomes `user
  add|list|rights|rm|link` and `device add|route|show|rm USER/DEVICE`; every `profile` command,
  `ProfileService` and the console's Profiles page are gone, as is the `profile` metrics label. The
  store migration drops every profile, every peer and its history; users and their links stay, so
  devices must be added again and their configs reissued. The web panel edits routes on the user
  page and in the console's new user page, which also sets the rights.
- Breaking: profiles moved from `lotsman.yaml` into the database (#128). A config with a
  `profiles:` key fails `config check` and `serve` with the `lotsman profile import` command to
  run; existing peers stay blocked until their profiles are imported. `user add -profile` must name
  a stored profile. `config check` validates the stored profiles against the upstreams and prints
  their warnings, in the CLI's tier syntax (`tier 2 geo=FI`). A reload that takes the last upstream
  from a stored profile the running config serves is rejected; a profile without any upstream at
  start only warns and its peers stay blocked, so the gateway still comes up.
- amneziawg-go moves to `amneziawg-go/v3 v3.1.20261116`, which re-checks S4 when a packet reaches
  encryption, so the first packet of a device configured after creation is padded without help.
  Lotsman's TUN wrapper that held the first read until the first configuration is removed;
  `TestFirstPacketCarriesTransportPadding` stays as the regression guard. The runtime also rejects
  `jmin` above `jmax` and negative `<r>`, `<rc>`, `<rd>` and `<dz>` lengths, makes I1–I5 changes
  during a handshake race-free and fixes a timer locking order; Lotsman's own validation, with its
  clearer errors, runs first and is unchanged (#105).
- Each Outline upstream's stack runs in its own helper process (`lotsman proxy-helper`, started by
  the daemon with the TUN descriptor; the key goes over a socket pair, never argv or environment),
  as uid/gid 65534 without capabilities. A crash, a kill, a helper that stops answering or a stack
  whose TUN read failed takes that upstream down at once, moves its clients, restarts the helper
  with backoff and counts `lotsman_proxy_restarts_total`; the gateway and other upstreams keep
  running. A dynamic key's rotation reaches the running helper as key text. The systemd unit's `CapabilityBoundingSet` adds `CAP_SETUID`
  and `CAP_SETGID`: reinstall `deploy/lotsman.service` when upgrading (#91).
- New identities draw S3 and S4 from 12 upward; existing identity files are upgraded on load
  (derived header protection key, S1–S4 raised to 12), which invalidates configs issued before (#60).
- amneziawg-go moves from a master pseudo-version to the tag `amneziawg-go/v3 v3.1.20260828`.
  AWG 1.5-only keys (`J1`–`J3`, `Itime`) are rejected with a message to request a 2.0+ config (#59).
- CI and `just lint` run `go mod verify`, staticcheck and govulncheck for the host and the Linux
  integration build; the tools are pinned with `tool` directives in `go.mod`. The toolchain is
  raised to go1.27.4 for GO-2026-6859 (#54).
- The required `integration` CI job runs `just test-integration` (every package, serially, in the
  privileged test image) instead of two hand-built test binaries, so the integration cases in
  `health`, `tunnel`, `daemon` and `proxy` run on every PR. `just test-integration` accepts
  `go test` flags and packages (#116).
- The downstream UDP socket gets 7 MiB send and receive buffers on Linux, as AWG's default bind
  does (`SO_RCVBUF`/`SO_SNDBUF`, then the `FORCE` variants with `CAP_NET_ADMIN`). With the kernel
  default (208 KiB) a client-to-gateway TCP flow lost datagrams to `UdpRcvbufErrors` (#102).
- The systemd unit's description is `Lotsman AmneziaWG gateway`, without the protocol version, which
  is no longer fixed at 2.0 (`awg: 3` is the default) (#101).
- Proxy upstream helpers run as the dedicated system user `lotsman-proxy` when it exists, instead
  of uid/gid 65534, so other services running as `nobody` can no longer signal (kill) them; the
  daemon falls back to 65534 with one warning when the user is missing. Create it at install
  (`useradd --system --user-group --no-create-home --shell /usr/sbin/nologin lotsman-proxy`) and
  record `user=lotsman-proxy` in `/etc/lotsman/INSTALLED` so `deploy/uninstall.sh` removes it (#97).
- `lotsman serve` sets a soft Go memory limit of 40% of the host's RAM (or of the cgroup v2
  `memory.max` when lower) when `GOMEMLIMIT` is unset; an explicit `GOMEMLIMIT` wins and helpers
  are unaffected. amneziawg-go's unbounded buffer pools pushed a small VPS to 240–280 MiB RSS under
  load; a 200 MiB limit about halves the peak for at most a 20% loss in the slowest case (#106).

### Fixed
- AWG upstreams on low-PMTU paths can set `disable_udp_gso: true` to avoid
  `sendmmsg: message too long` when Linux rejects an oversized GSO segment (#165).
  The setting keeps syscall batching and applies to the individual upstream.
- Web panel: live pages reopen their Watch stream when it drops or ends, instead of replacing the
  page with "Load failed". A phone suspending the page in the background (iOS Safari), a network
  switch or a panel restart now only pauses the live numbers; reopening backs off from 1 s to 30 s
  and goes at once when the page is back in the foreground or the device online. Only errors a new
  stream cannot fix (a revoked link, a signed-out operator, a removed peer) still show. Leaving a
  page aborts its stream at once.
- A deleted peer's id is never given to a new peer (`peers.id` is `AUTOINCREMENT`), so the API's
  `peer_id` and the history it keys stay unique.
- `status.json` follows the daemon's health rounds instead of a timer started with the daemon, so
  after a reload that shortens `health.interval` `upstream status` and `user list` no longer lag by
  the old interval (#117). `serve` also waits for the status file to be removed before it releases
  its instance locks.
- `user rm NAME -profile ""` (an unset shell variable, say) removed every config of the user, or
  of the device with `-device`; an explicitly empty `-profile` is now rejected for all `user`
  commands, and omitting it still selects the whole device or user (#113).
- `user add` no longer leaves a peer, or a new user, behind when it fails: the profile and the
  upstream configs are checked before anything is written, and the user and its first config are
  created in one transaction, so an unreadable provider file or a full subnet changes nothing. A
  failure to write the config to stdout is reported with the `user show` command that prints it
  again (#115).
- A second `lotsman serve` started next to a running one, in the same network namespace or on the
  same `state_dir`, no longer disturbs it. It used to overwrite and then remove the running
  daemon's pid file and, once its own startup failed on the busy `lm0`, delete `table inet
  lotsman` (NAT and the fail-closed drop) until the running daemon's firewall watch reinstalled
  it. `serve` now takes an exclusive
  lock per network namespace (abstract Unix socket `@lotsman`) and per state directory (flock on
  `state_dir/serve.lock`) before anything else and exits with `another lotsman serve is running
  …` if either is held. A daemon whose startup fails before dataplane setup also no longer tears
  down rules, routes or the firewall it never installed (#114).
- The first data packet of every AmneziaWG device (each upstream, and `lm0` towards clients) went
  out without its S4 transport padding and was dropped by the peer, which delayed an upstream's
  first health probe by a round. The runtime read S4 before it blocked on the TUN; fixed by the
  runtime itself in amneziawg-go v3.1.20261116 (#84, #105).
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
- Requests to a proxy upstream's helper process (starting it, changing its server, key or
  `max_flows`, stopping it on reload or shutdown) no longer run under the daemon lock, so a stuck
  or dying helper no longer stalls status, metrics, reconciliation or the other upstreams for up
  to 10 s. `upstream status` and `/metrics` no longer ask the helper either: the flow counters
  in `lotsman_upstream_flows` and `lotsman_upstream_flows_refused_total` are those of the last
  health round (#98).
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
