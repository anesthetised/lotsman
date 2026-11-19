# Architecture

Lotsman is a self-hosted AmneziaWG gateway. Users connect with the official Amnezia client;
Lotsman terminates their tunnel and forwards traffic into one of several upstreams, AmneziaWG tunnels,
Outline (Shadowsocks) servers or VLESS + REALITY servers (the operator's own provider subscriptions),
chosen by the user's profile preference and live health.

```
user (Amnezia app) ──AWG2──▶ [lm0 TUN] ──kernel policy routing──▶ [lm-up-N TUN] ──AWG2──▶ provider ──▶ internet
                             downstream device (server)            upstream device N (client)
                                    ▲                                    ▲
                                    └──────── lotsman daemon ────────────┘
                                  peers, rules, health, selection, CLI
```

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Role | Data plane: Lotsman is in the packet path | Zero custom clients — only the official Amnezia app. Latency and a shared exit IP are accepted costs. |
| AWG runtime | `amneziawg-go` embedded as a library | No open-source AWG 2.0 kernel module exists (see below), so crypto is userspace regardless. |
| Forwarding | Kernel: real TUNs, policy routing (`ip rule`/tables), nftables | NAT, conntrack and MSS clamping come from the kernel; the Go code stays small. Linux only. |
| Preference | One peer per (user, profile) | The preference is encoded in which key the user connects with; no side channel needed. |
| State | YAML for upstreams, SQLite for profiles, users and peers | Operators edit and version the YAML; profiles and users change at runtime (CLI now, the web panel later) and apply without a restart. |
| Fail-closed | Mandatory | User traffic must never leave via the host's own IP. nftables admits only authorized peers from `lm0` into an `lm-up-*`; all other forwarding from `lm0` is dropped. |
| Host firewalls | Accept rules inserted into foreign forward chains | Docker's `FORWARD DROP` and ufw would otherwise silently drop forwarded traffic; same approach as Tailscale. |
| Proxy upstreams | A userspace TCP/IP stack per upstream on its own TUN, flows re-originated through a pluggable dialer (Shadowsocks, VLESS + REALITY through embedded xray-core), each in its own unprivileged helper process | Keeps kernel routing, pinning and fail-closed identical for every upstream kind; a panic in the stack takes down one upstream, not the gateway; see [Proxy upstreams](#proxy-upstreams-outline). |
| Control API | `lotsman.v1` over Connect on a root-owned unix socket, `/run/lotsman/control.sock` | One typed API for the web panel and, through it, the browser; the socket's file mode is the only boundary, so the daemon needs no authentication of its own; see [Control API](#control-api-127). |
| Web panel | `lotsman web`, a separate unprivileged process that forwards the same API with a scope per RPC | A bug in the browser-facing code runs as `lotsman-web`, not root, and never sees `lotsman.db`; see [Web panel](#web-panel-132). |

## AmneziaWG implementation notes (verified 2026-09-17)

- `github.com/amnezia-vpn/amneziawg-go/v3` is pinned to the tag **v3.1.20261116** (AWG 3.1,
  updated 2026-11-18). It stores `device.headers.*` atomically and applies `IpcSet` header and
  padding changes under a lock, so the v0.2.19 data race (headers rewritten by any `IpcSet` while
  the receive goroutine reads them) is gone, and I1–I5 are atomic pointers, so changing them
  during a handshake does not race either. The runtime also shifts a queued packet when S4
  changes between the TUN read and encryption, so a device configured after creation pads even
  its first packet (no TUN wrapper needed; `TestFirstPacketCarriesTransportPadding` guards it).
  With no 3.x keys the runtime is AWG 2.0 on the wire; `TestAWG2RuntimeInterop` checks this against v0.2.19, a test-only dependency.
- AWG 3.x keys, accepted on upstreams: `HeaderProtectionKey` and `RandomTrailers` must match the
  server; `ContentPaddingAddition`, `RekeyAfterTime`, `RekeyTimeout`, `RejectAfterTime`,
  `KeepaliveTimeout`, `MaxHandshakeAttempts` and `DisableCookies` are local. Header protection
  encrypts in place and trailers stay within the peer's UDP window, so neither changes the
  handshake-size checks; the client MTU subtracts the `ContentPaddingAddition` upper bound.
  `[Peer] PersistentKeepalive` is a `Range` too (UAPI `persistent_keepalive_interval`, parsed by the
  same `UintRange.FromString`).
- The public `amneziawg-linux-kernel-module` implements 1.5 parameters only; the 2.0 module is
  distributed as a binary PPA without sources. Not a dependency Lotsman can take.
- UAPI device keys for 2.0: `jc`, `jmin`, `jmax`, `s1`..`s4` (uint16), `h1`..`h4` (single value or
  `a-b` range), `i1`..`i5` (tag chains: `<b 0x..>`, `<r n>`, `<rd n>`, `<rc n>`, `<t>`). Everything
  else is stock WireGuard UAPI. `IpcGet` echoes these keys plus `last_handshake_time_sec`,
  `rx_bytes`, `tx_bytes` per peer.
- Verified in a spike: two devices in one process (server + client), both on `tun/netstack`, handshake
  and TCP transport with `S4` padding work; a mismatched `S4` blocks transport, so parameters are
  enforced, not decorative.
- `tun/netstack` (gVisor) is available and needs no privileges — the `tunnel` package can be tested on
  macOS without root. Only `dataplane/linux` needs Linux and `NET_ADMIN`.
- `S4` pads every transport message; it reduces effective MTU on both hops.
- **Plain WireGuard stays supported.** With no obfuscation parameters amneziawg-go defaults to the
  stock message types 1–4, zero padding and no junk, i.e. it is WireGuard on the wire. A provider
  that hands out a plain WireGuard config works as an upstream unchanged; `TestPlainWireGuardInterop`
  proves it against an untouched `wireguard-go`. The downstream speaks one configurable generation
  (`awg`, default 3), because obfuscation parameters, including the header protection key and
  random trailers, are per device, not per peer. Client configs carry only the shared AWG 3 keys;
  client-local ones (content padding, timings, cookies) stay at their defaults, so the client MTU
  is unchanged.

## Downstream socket binding

Downstream socket binding honors the configured numeric address and family via
`tunnel.NewBound`; upstreams continue to use `tunnel.New` and the default AWG bind.
The address bind keeps one UDP socket, captures it in each receiver across reopen,
and sends every buffer supplied by AWG even though its receive batch size is one.
Wildcards are family-specific, mapped IPv4 addresses are normalized, and native
IPv6 zones are retained. Binding failures propagate instead of widening exposure.
It omits default-bind batching, offloads and sticky-source caching; throughput
equivalence has not been measured. On Linux the socket gets StdNetBind's 7 MiB
send and receive buffers (`SO_RCVBUF`/`SO_SNDBUF`, then the `FORCE` variants past
`rmem_max`/`wmem_max`); a failure to set them is ignored. A concrete listen
address fixes reply source; wildcard replies follow host routing.

## MTU

Client tunnel MTU = `min(1500 − 80 − S4_down, min over upstreams (upstream MTU − S4_up))`.
80 bytes = IPv6 (40) + UDP (8) + WireGuard transport header (16) + Poly1305 tag (16). This is the
worst case and applies to every client, even with an IPv4 `listen`: the endpoint hostname may also
resolve over IPv6. The same value is set on `lm0` and written into client configs; nftables
additionally clamps TCP MSS to the route MTU.

## Forwarding authorization and shutdown

Approved on 2026-10-16 for #14 and #8. The nftables `authorized_peers` set starts
empty. `Route` grants access only after installing routing rules; `Unroute` revokes
access before deleting them. This separates forwarding permission from route
selection without adding deny-rule priorities or changing live-flow pinning.
Both outgoing requests and established replies require an authorized peer.

Linux `Teardown` first sets the downstream link down, then removes its rules,
routes and firewall state. It retains the downstream name before configuring the
link so a partially failed setup can also be stopped. If finding or disabling the
link fails, protection stays intact and the error is returned; an already absent
link is safe to clean up. Keeping this step inside `Teardown` avoids a separate
quiesce API and makes the ordering apply to every caller.

The daemon keeps upstream TUNs open through cleanup because route ownership is
proved against their interfaces. On a teardown error it closes downstream first
and retries once while upstreams still exist, then closes upstreams. Persistent
errors are logged; a later start reconciles owned leftovers.

## Failover semantics

Health probes bind to both the provider source address and its TUN interface.
Priority-4000 rules select `from <provider>/32 oif lm-up-<name> lookup <table>`;
providers with overlapping tunnel IPs therefore remain independently observable.
The output-interface selector applies to locally originated device-bound sockets,
not forwarded client traffic. Peer and live-flow pin rules retain their existing
selectors. Ordinary provider transport sockets and endpoint DNS resolution remain
on host routing.

Cleanup recognizes the old source-only probe shape for upgrades. It checks a
qualified rule's interface against the owned table and rejects ambiguous probe
deletions before removing routes: Linux treats omitted deletion selectors as
wildcards, so blindly deleting a legacy rule could delete a foreign qualified one.
Uninstall inspects rules before removing interface ownership evidence and handles
detached crash-leftover interfaces explicitly.

Switching a peer to another upstream changes the exit IP of *new* flows only. Existing flows are
pinned: every new flow leaving through an upstream gets a conntrack mark equal to that upstream's
routing table (`stamp_mark` chain, `ct mark set oifname map @pinmark`); later packets carry the mark
into routing (`restore_mark` chain, `meta mark set ct mark`); and when a peer moves off an upstream
that is still alive, a per-peer rule `from <peer> fwmark <table> lookup <table>` (priority 3000,
ahead of the plain `from <peer>` rule) keeps those flows on it. The pin rules are per peer, so
`Unroute` revokes the peer in the nftables `authorized_peers` set before removing its rules,
so even stale pins or main-table connected routes cannot authorize it; when an upstream goes
down its pins are deleted and the flows follow the peer's current route. Only flows through a dead
upstream are lost, and those were dead anyway.

### Throughput health (#87)

Blocking often degrades a path instead of cutting it: TCP flows freeze after 15–20 KB or crawl,
while a handshake and a DNS answer still pass. The optional transfer probe (`health.transfer`)
therefore downloads `min_bytes` from an operator-chosen URL through each upstream, bound like the
reachability probes, and requires it within `timeout`. Decided 2026-11-16:

- An absolute threshold (bytes within a time) rather than one relative to the upstream's own
  history: a freeze is absolute, and a history-based baseline would learn a slow path as normal.
- A separate *degraded* state with the reachability hysteresis (`down_after`, `up_after`) on its
  own results, so one slow sample does not flap. Reachability stays the separate signal
  (`upstream_up`).
- Selection: a degraded upstream is a last resort (`policy.Select` tries healthy, not degraded
  upstreams in every tier first, then the degraded ones by the same rules): slow connectivity
  beats a blocked client.
- Pinning: `alive` (what `SetAlive` gets) is up and either not degraded or still some peer's
  route. A frozen path holds old flows hostage as surely as a dead one, so once every peer has
  moved away its pins are released to the peers' new routes; while it is a last resort it stays
  alive, otherwise its own clients' flows would be cut every round. Peers moving changes this
  answer without a probe result, so `Reconcile` re-evaluates it after routing.
- Probes run beside the health round, not in it: at most one per upstream, never under the daemon
  lock, only while the upstream is up, and every `interval` ±20% drawn per upstream and probe, so
  the probes neither delay the 15 s round nor form a fixed period. After a failure and while
  degraded, a fifth of `interval` (at least `health.interval`), so a freeze is confirmed and a
  recovery seen in minutes, at extra cost only while the upstream fails. Run waits for them on
  shutdown.
- No built-in URL: any default would be a single third party every installation calls, and a
  recognisable signature. The URL's host is resolved by the host resolver; only the connection
  goes through the upstream, as for an AWG endpoint lookup.

## Proxy upstreams (Outline)

Decided 2026-11-16 for #61; the stack is protocol-neutral so VLESS (#86) can reuse it.

**Context.** Shadowsocks (what Outline servers speak) is a TCP/UDP proxy, not an L3 tunnel. Lotsman's
guarantees (per-upstream TUN `lm-up-<name>`, policy routing, conntrack flow pinning, nftables
fail-closed) all hang on the kernel routing client packets into an interface.

**Decision.** Every proxy upstream gets an ordinary kernel TUN, created like an AWG one, and a
gVisor netstack (`internal/proxy`) on it. The stack accepts every destination (promiscuous mode
plus spoofing, IPv4 only), terminates each TCP connection and UDP flow, and re-originates it
through a `Dialers` value: a `transport.StreamDialer` and `transport.PacketListener` from the
Outline SDK (`golang.getoutline.org/sdk`, `transport/shadowsocks`), plus an optional closer. A
`proxy.Key` parses an access key and builds the dialers for a resolved server address: `Server()`
is the `host:port` the daemon resolves, `Dialers(addr)` connects only to that address. Keys so far
are `ss://` and Outline's `transport:` configs (Shadowsocks, also over WebSocket), static or fetched
from an `ssconf://` URL. The daemon sees either kind through
one small `link` interface (`Up`, `Close`, `SetServer`, `Stats`), so loading, reload, health,
routing and status stay one code path.

Alternatives rejected: the SDK's `network/lwip2transport` (cgo, one process-wide instance, so at
most one Outline upstream); external processes such as outline-cli or sing-box (not embedded, and
sing-box is GPL); the SDK's `x/configurl` for key parsing (pulls quic-go, websocket and HTTP/2, and
its API is unstable; the `ss://` grammar is small enough to own).

**Details.**

- Channel link endpoint, not `fdbased`: amneziawg-go's `tun.Device` already handles the Linux TUN's
  virtio-net header and GRO/GSO batching, and is what the AWG devices use. Feeding the stack from
  it through `link/channel` keeps one TUN implementation, and lets the daemon tests run the stack on
  netstack TUNs without privileges, as they do for AWG.
- Dial before handshake: the TCP forwarder dials the server first and completes the client's
  handshake only on success (a reset after at most 10 s otherwise), so an unreachable server looks
  refused, not hung. A flow ends with a half-close when either side ends cleanly, and with a reset
  to the client when the server side fails. Each flow has its own context, alive as long as the
  flow, because a dialer may keep using it after returning (VLESS returns before reaching the
  server); the dial timeout cancels it only while the dial is pending.
- No ICMP: the stack does not answer pings for the addresses it impersonates, so reachability is
  never faked. ICMP errors about its own flows are processed.
- UDP: one proxy session per client 4-tuple, closed after 1 minute idle (10 s for port 53); replies
  are accepted only from the flow's destination, the only source the stack can answer from.
- Limits: at most `max_flows` flows (TCP still dialing included, default 4096) per upstream; a
  refused TCP flow is reset, a refused UDP packet dropped, both counted. Closing the stack aborts
  every endpoint, including handshakes whose client went away (they would otherwise wait minutes
  for SYN-ACK retransmissions).
- Address: every proxy TUN has `192.0.0.8/32`, the IPv4 dummy address of RFC 7600, meant for a host
  that needs a source address and has none. It is the source of probes and, after masquerading, of
  client flows inside the stack. Sharing one address is safe because probe rules select by
  interface too (see [Failover semantics](#failover-semantics)), the same property that lets AWG
  providers hand out overlapping addresses; the client subnet may not contain it.
- MTU: the TUN is 1500 and enters the client MTU computation without overhead, so it never lowers it; the
  outer path is the kernel's TCP and UDP to the server.
- Endpoint: a hostname server is resolved up front like an AWG endpoint, never per flow, and
  re-resolved when probes fail; new dialers are swapped in atomically and apply to new flows.
- Dynamic keys (`ssconf://`): loading stays offline and keeps only the URL; the key is fetched in
  the same unlocked phase as the endpoint lookup (start, reload, reopen) and when probes fail, at
  most every 5 minutes per upstream so a dead server does not become a request stream to the
  provider. A changed key goes to the running link through `SetServer`, the same path as a moved
  endpoint, so no TUN or stack is reopened. Response formats follow the Outline client
  (outline-apps `client/go/outline/parse.go`, `doParseTunnelConfig`): an `ss://` string, the legacy
  Shadowsocks object, a `transport:` config (below), or a provider `error`. The last good response is cached per upstream in the
  state directory under a SHA-256 fingerprint of the URL, so a restart while the URL is blocked
  still has a key, and a key never serves a URL it did not come from. Redirects are followed only
  to `https://`; the Outline client follows any.
- WebSocket transports (2026-11-16, #61 slice 4): Outline's `transport:` configs are parsed by our
  own reader (`internal/proxy/transport.go`) of the subset of the Outline client's config language
  that reaches a Shadowsocks server (outline-apps `client/go/outline/configregistry`: `registry.go`
  `NewDefaultTransportProvider`, `config_tcpudp.go`, `config_shadowsocks.go`,
  `config_dial_endpoint.go`, `config_websocket.go`): `tcpudp`, `shadowsocks`, `dial` and
  `websocket` endpoints; every other `$type` is refused naming the type, never quoting values. A
  missing or `direct` `udp` leaves the key without UDP (never UDP around the server), so health
  fails over UDP and the upstream stays down. TCP and UDP must name the same host because
  `Server()` is one address: the daemon resolves the WebSocket URL's host (or the endpoint's
  `endpoint` override) up front as for plain Shadowsocks; `Dialers(addr)` connects to `addr` (UDP on
  its own port), and TLS server name, certificate check and HTTP `Host` use the URL's host. The
  wire format follows outline-ss-server (`cmd/outline-ss-server/main.go`, websocket-stream and
  websocket-packet listeners) and the SDK it uses (`x/websocket`, `gorillaConn`): one WebSocket per
  TCP connection, its bytes in binary messages; one per UDP flow, one datagram per message; a close
  frame is a half-close (sent on `CloseWrite`, read as end of stream, not echoed). Library:
  `github.com/gorilla/websocket` v1.5.3 (BSD-2, no dependencies), the one the Outline SDK and server
  use. `github.com/coder/websocket` was preferred but cannot half-close: it answers a received close
  frame and closes, and its `Close` waits for the peer's while discarding data. The SDK's own
  `x/websocket` would add the `golang.getoutline.org/sdk/x` module (122 more module-graph edges,
  Psiphon, quic-go, … in its `go.mod`), force the SDK from v0.0.23 to v0.1.0, and is a v0.x API, for
  the same gorilla code underneath. Gorilla added 1 module-graph edge and about 230 KB to the
  stripped amd64 binary (parser included). The helper passes `SSL_CERT_FILE`/`SSL_CERT_DIR` on from
  the daemon's environment, so wss:// servers are verified against the same roots as `ssconf://`.
- Health: a TCP handshake proves nothing through a proxy (the stack completes it itself, and
  outline-ss-server keeps a wrong-key connection open silently), so a proxy upstream passes a round
  only when DNS queries to a `health.dns` resolver are answered over UDP and over TCP. Latency is
  the UDP query's, one round trip to the resolver like the AWG handshake probe.

**Addendum: helper processes (2026-11-16, #91).** A panic in any goroutine ends a Go process, and
the stack runs third-party code that parses what the network sends: gVisor, the Outline SDK and,
with #86, xray-core (whose spike panicked on a non-REALITY certificate, i.e. under MITM, which is
when blocking happens). In process, one such panic would take down the gateway: the listener, every
AWG upstream and every client. So every proxy upstream's stack runs in a child process of the same
binary (`lotsman proxy-helper`, hidden), one per upstream; Shadowsocks too, so there is one model.

- Split: the daemon keeps everything that needs privileges or global state: it creates the TUN
  (`d.newTUN`), owns policy, health probes, the dataplane and metrics. The helper gets the TUN
  descriptor as fd 3 and runs only the stack and the dialer. The TUN stays open in the daemon, so
  the interface, its routes and rules survive a helper's death; packets routed in meanwhile are
  dropped by the kernel.
- What crosses the boundary: fd 3 (TUN), fd 4 (one end of a Unix socket pair, the control
  connection) and stderr. Over fd 4, request/response frames of a 4-byte big-endian length and a
  JSON object (at most 64 KiB): `open` {access key text, resolved server, `max_flows`, log level},
  then `server` {key text, resolved server} for a moved server or a rotated key, `max_flows` and
  `stats` (the flow counters). Closing the connection tells the helper to stop. The key travels
  only there, never in argv or the environment, which other processes can read through `/proc`.
  The daemon sends key text, never a parsed key: a static key file's contents or a dynamic key's
  response as fetched. The helper parses either with `proxy.ParseSpecKey`: a `vless://` link, or
  what `proxy.ParseConfig` reads, which loading uses for a static Outline key file too; on `server`
  only when the text changed.
  The helper logs slog JSON records to stderr, and the daemon re-logs each with its level
  and attributes plus the upstream's name; other lines (a panic and its trace) are logged as errors,
  each cut at 4 KiB. Stderr rather than the control connection, because a panic or a fatal runtime
  error writes there regardless, and one ordered stream keeps the protocol strictly
  request/response.
- Crash handling: the daemon reaps every helper. One that exits on its own after it was up marks the
  upstream down at once (not after `down_after` rounds), drops its flow pins and reroutes its
  clients in the same step, and counts `lotsman_proxy_restarts_total`; the exit status and the
  panic's first line are logged. It is then "not up" like a device whose `Up` failed: the health
  round brings it up again, the first attempt immediately, and after an exit within a minute of
  starting the next start is held back 1 s, doubling to 1 min. Back up, it needs `up_after`
  successful probes like any down upstream. A helper that does not answer a request within 2 s
  (10 s for `open`) is killed, which counts as a crash. A helper whose stack stopped because
  reading the TUN failed exits with status 1, which counts as a crash too, so a stack that can no
  longer carry traffic is restarted instead of idling. On stop and reload the connection is closed
  and the helper killed if it has not exited within 5 s.
- Lifecycle: `Pdeathsig` (SIGKILL) kills helpers if the daemon dies; helpers are started from one
  goroutine locked to a thread that never exits, because Linux sends that signal when the thread
  that forked exits (go.dev/issue/27505). Helpers ignore SIGINT, SIGTERM and SIGHUP, so stopping the
  service or Ctrl-C reaches them only through the daemon. `/proc/self/exe` is executed, so an
  upgrade that replaced the binary on disk still starts the daemon's own version.
- Privileges: a root daemon starts helpers as the system user `lotsman-proxy` (its uid and primary
  gid, looked up at each start; no supplementary groups), or as uid and gid 65534 with a warning
  logged once when the user does not exist or resolves to root, so
  exec leaves them no capabilities (no file capabilities on the binary); they need only the
  inherited descriptor and outbound sockets. The environment is empty but for `SSL_CERT_FILE` and
  `SSL_CERT_DIR` when the daemon has them (TLS roots for wss:// keys), and the working directory `/`.
  A helper makes itself non-dumpable, so other processes of its user cannot read its memory, which
  holds the key. The dedicated user (#97) also keeps other services from signalling it: with
  uid 65534 they could kill it, which the daemon handles as a crash. The install creates the user
  and records it in the manifest, so uninstall removes it. systemd's `DynamicUser=` does not fit:
  it allocates a user for the service's own main process, the daemon, which must stay root to
  program `/proc/sys`, nftables and routes, and it has no way to hand a second uid to a child the
  daemon starts itself. The systemd unit's bounding set gains `CAP_SETUID` and
  `CAP_SETGID` for the switch. A non-root daemon starts helpers as its own user, inheriting its
  ambient capabilities.
- Health is unchanged: probes still leave the daemon through the TUN, so they measure the helper end
  to end. Tests keep the in-process runner (`proxy.InProcess`): netstack TUNs have no descriptor to
  pass, and the daemon's logic is the same behind the `proxy.Runner` seam.

Alternatives rejected: `recover` in every goroutine (third-party goroutines, and runtime fatal
errors such as concurrent map writes, cannot be recovered); a separate binary (two artifacts to keep
in step); `fdbased` I/O in the helper instead of amneziawg-go's TUN (see above); logs over the
control connection (stderr is needed for panics anyway).

**Cost.** Measured in the slice-1 spike (#61) in a privileged container on one host: a single bulk
TCP flow through kernel TUN, gVisor and Shadowsocks moved about 2 Gbit/s, and the TUN plus stack
added roughly 1.5–4 ns of CPU per byte over the Shadowsocks client alone. The provider leg is
expected to remain the bottleneck, as for AWG (see [Throughput](#throughput-2026-09-17)).

**Addendum: VLESS upstreams (2026-11-16, #86).** VLESS with REALITY and XTLS Vision (Xray) disguises
itself as TLS to a real third-party site, where Shadowsocks is increasingly caught by its
fully-encrypted look; a profile can fail over between the two. VLESS is the stack's second dialer:
`kind: vless`, a static `vless://` share link, TCP transport, UDP as XUDP.

| Option | For | Against |
|---|---|---|
| **A. Embed `github.com/xtls/xray-core` (chosen)** | REALITY, Vision and the uTLS fingerprints are maintained upstream and move with every Xray release; MPL-2.0 is file-level copyleft | A heavy module set and +7 MB; xray needs a newer gVisor than amneziawg-go pinned; Xray's `v26.x` tags are not Go versions |
| B. sing-box as a library | Cleaner Go API | GPL-3.0, already rejected for Outline |
| C. Own client on `refraction-networking/utls` and `xtls/reality` | Small | REALITY's handshake and fingerprints would have to track Xray release by release; a mistake there is exactly what gets detected |

- Embedding: per upstream one `core.Instance` built from proto structs (dispatcher, outbound
  manager, policy, one VLESS outbound with REALITY over TCP), not from Xray's JSON loader
  (`infra/conf`, which links every protocol); no inbound, DNS server or routing. The server and
  every target are IP addresses, so the instance never resolves a name. A `vless://` link is
  parsed by Lotsman (`proxy.ParseVLESS`), and its REALITY and VLESS fields are checked and
  normalized exactly as `infra/conf` does (`transport_security.go` `REALITYConfig.Build`, client
  branch; `vless.go`): Xray's client trusts its config, and the spike crashed the process in
  REALITY's crawler, which runs when the server presents a real certificate, because a config
  built without the loader lacks `spiderY`. A test runs that path in child processes with and
  without the normalization, and a fuzz test checks that every key that parses has it.
- Behaviour the stack absorbs: a VLESS dial returns before the server is reached, so the client's
  handshake completes and a dead server surfaces as an error on the flow, which the relay turns into
  a reset; each flow's context lives as long as the flow (the dialer keeps it); Xray's
  `DialUDP` socket does not end its mux connection on `Close`, so the dialer cancels the flow's
  context too; Xray's policy timeouts (5 min idle, 1–2 s after either direction ends) are raised to
  1 h so idle sessions and half-closed flows survive. VLESS carries no half-close to the target.
  Plain Vision refuses UDP to port 443 (QUIC), so browsers fall back to TCP; the
  `xtls-rprx-vision-udp443` flow allows it.
- Process-global state in Xray: its log handler (bridged to slog by `proxy.LogXray`, with Xray's
  per-flow info messages as debug and errors and warnings rate-limited to one per kind per minute),
  `internet.InitSystemDialer` (overwritten by every instance; unused, since the system dialer has no
  options here), REALITY's crawler cache and `XRAY_*` environment variables (helpers run with an
  empty environment). With one helper per upstream (#91) each instance has a process to itself;
  in process, as in tests, several instances work side by side, and the last VLESS stack started
  receives the log of all.
- Keys: VLESS links are static. `proxy.ParseSpecKey` routes `vless://` text to `ParseVLESS` and
  everything else to `ParseConfig`, which stays Outline's format, so an Outline dynamic key never
  yields a VLESS upstream, and the kind in the config decides which parser loading uses.
- Versions: xray-core v26.11.9 is pinned as `v1.260327.1-0.20261109052709-676eaf66f8f0`, the
  pseudo-version of the release tag's commit; `go get -u` and Dependabot do not see Xray releases,
  so they are bumped by hand (routine in OPERATIONS.md, "Updating Xray"). gVisor rises to xray's
  `v0.0.0-20260122175437-89a5d21be8f0` for everything (one API change: the UDP forwarder handler
  returns whether it handled the packet); amneziawg-go, the wireguard netstack used in tests and
  the proxy stack pass with it. `klauspost/compress` is raised to v1.20.2 for GO-2026-6865.
- Cost: the static linux/amd64 binary grows from 16.0 MB to 23.3 MB (+7.3 MB, +45%); linked modules
  from 26 to 45, among them grpc, protobuf, quic-go, gopher-lua, miekg/dns, utls and reality, which
  Xray's core packages import though nothing here uses them at run time. It also links
  `golang.org/x/crypto/x509roots/fallback`, a root bundle Go uses only on a host without system
  roots. In the spike, one bulk flow through TUN, gVisor and VLESS moved 4.2–4.4 Gbit/s up and
  2.8 Gbit/s down in a container; the stack, not VLESS, is the bottleneck.
- Licenses of the new modules: MPL-2.0 (xray-core, reality, gopher-luar; file-level copyleft,
  satisfied as long as their files are used unmodified), BSD (utls, miekg/dns, netipx, protobuf,
  x/ packages; klauspost/compress BSD and Apache-2.0), Apache-2.0 (grpc, go-proxyproto) and MIT
  (quic-go, gopher-lua, blake3, go-brrr, cpuid), plus `juju/ratelimit` under LGPL-3.0 with
  Canonical's exception that allows
  conveying a statically linked combined work without the relinking materials of LGPL section 4d
  and 4e.
- Upstream issue: Xray's Vision server shares per-connection state between the two directions
  without synchronization; the race detector reports it in the in-process test server every run
  (the client side is not affected in our tests). Test files that run that server carry a `!race`
  build constraint until it is fixed upstream; the report is drafted, and filing it waits for the
  owner. The gRPC test server runs no Vision, so its tests run under the race detector.

**Addendum: VLESS over gRPC (2026-11-18, #122).** Providers also hand out VLESS + REALITY over gRPC,
which Vision cannot run over, so such links carry no flow. `type=grpc` selects Xray's
`transport/internet/grpc` (importing it registers the dialer) with the same REALITY settings;
`serviceName`, `authority` and `mode` (`gun`, or `multi` for `multiMode`) map to its `Config`, and
the keepalive, window and user-agent fields keep Xray's client defaults (zero), as a share link
cannot set them. They are checked as `infra/conf` does (`GRPCConfig.Build` accepts any service name
and authority; `StreamConfig.Build` allows REALITY over RAW, XHTTP and gRPC). `alpn` is accepted
and ignored: Xray's REALITY settings have no ALPN, the ClientHello offers the fingerprint's.

- Every flow of an upstream is a stream in one HTTP/2 connection, which Xray keeps in a
  process-global map keyed by the outbound's stream settings and never closes; an instance replaced
  on reload or a server move leaves its connection to grpc-go, which closes it after 30 minutes
  without streams. Without keepalive pings a connection that stalls without a reset is noticed by
  the probes, not by gRPC.
- UDP: without Vision, Xray sends UDP to ports 53 and 443 as plain VLESS UDP and to other ports as
  XUDP; each socket of the dialer is one destination, so both work.
- Cost: the static linux/amd64 binary grows from 23.6 MB to 30.3 MB (+6.7 MB, +28%): Xray's gRPC
  package links grpc-go's HTTP/2 client and server, of which the core packages had linked only a
  few. One new module, `google.golang.org/genproto/googleapis/rpc` (Apache-2.0, gRPC's status
  protos).

## Control API (#127)

`lotsman serve` serves the control API, the protobuf package `lotsman.v1` in `api/lotsman/v1/` (one
file per service), over Connect on the unix socket `/run/lotsman/control.sock`. The web panel
(`lotsman web`, #132) is its client and forwards it to browsers with a scope per RPC. Every service
of the agreed contract is registered; until the issue that implements an RPC lands, it answers
`unimplemented`. `StatusService.Watch` streams a full `StatusSnapshot` at once and then every
`interval` (default and minimum 1 s; shorter is raised to 1 s, an invalid duration is
`invalid_argument`), built from the daemon's `Status` plus the upstreams' `geo`, `provider` and
`tags`, each peer's tunnel address and the client MTU. A stream opened before the first reconcile
waits for it, so no client sees an empty gateway. `peer_id` is `p_<row id>` of the store's peer:
opaque to clients and unchanged by renames.

`ProfileService` (#128) translates to the daemon's `Profiles`, `PutProfile` and `DeleteProfile`,
which own validation and storage; `Matcher` maps 1:1 onto `config.Match`. Errors become codes:
`daemon.ErrInvalidProfile` is `invalid_argument` with the reason, `store.ErrInUse`
`failed_precondition`, `store.ErrNotFound` `not_found`, and anything else `internal` with no
detail, so database paths and SQL never reach the browser. `ConnectionService.Watch` (#131) gives
each stream its own `flows.Meter` over conntrack, the client subnet and the daemon's `PeerRoutes`
under their `peer_id`s, with the same interval rules and readiness wait as `StatusService.Watch`;
rates are deltas between that stream's snapshots. Sources and destinations are `IP:port`, or the
bare IP for ICMP. An unknown `peer_id` is `not_found`; a failed conntrack read ends the stream
with `unavailable`, and a build without conntrack (not Linux) answers `failed_precondition`, since
retrying cannot help there.

- Trust boundary: the socket's peer is trusted, and the file mode is the boundary. The socket is
  `0660 root:lotsman-web`; whoever can connect has the operator's full control, and the daemon
  checks no credentials. The `Lotsman-User` header that `UserSelfService` will read is trusted
  for the same reason: only the panel can send it, and the panel strips it from browsers.
- Lifecycle: the socket is created only after the instance locks (#114) are held, so a refused
  second `serve` never touches the running daemon's socket. It is bound inside a temporary
  `0700` directory, given its mode and group, and renamed into place, so it is never reachable
  with the wrong ones and a socket left by a crashed daemon is replaced in one step. One that still
  accepts connections (a daemon in another network namespace, which the locks allow) is refused,
  and `serve` fails. On shutdown request contexts end with the daemon's, so open streams close, and
  the socket is removed before the locks are released.
- Without the `lotsman-web` group the socket is created `0600` (root only) and the daemon logs a
  warning, instead of failing to start: the gateway does not need the panel, and an install without
  one must not break. The group is looked up at start, so creating it later needs a restart.
- Transport: HTTP/1.1 and h2c (cleartext HTTP/2) on the socket, no TLS; Connect, gRPC and gRPC-Web
  all work. Under systemd, `RuntimeDirectory=lotsman` provides `/run/lotsman` (`ProtectSystem=strict`
  makes `/run` read-only otherwise) and `CAP_CHOWN` lets the daemon hand the socket to the group.

| Option | For | Against |
|---|---|---|
| **A. connect-go v2 (chosen)** | Stable since 2026-10; plain-message handler signatures, stream interfaces that tests can fake, an in-process transport for tests; its only dependencies, protobuf and go-cmp, are already ours | Six weeks old; part of the ecosystem (otelconnect, grpchealth) still targets v1, and we use none of it |
| B. connect-go v1 | Mature, still maintained | A new API would start on the version its maintainers are moving away from; a migration later |
| C. grpc-go | Already linked by xray (#122) | Browsers cannot speak it without a proxy; the panel would translate every RPC |

Code generation: `just gen` runs `buf generate` with `protoc-gen-go` and `protoc-gen-connect-go`;
the generated code is committed, and `just lint` (so CI) runs `just gen-check`, which fails when
the protos are unformatted, break buf's `STANDARD` lint rules or differ from the committed code.
The lint exceptions are the request and response naming rules: the agreed contract shares messages
between RPCs. The TypeScript client (#135) is another plugin in `buf.gen.yaml`.

| Tooling option | For | Against |
|---|---|---|
| **A. Plugins in `go.mod`, buf in its own `tools/go.mod` (chosen)** | Pinned, reproducible, nothing installed globally; the plugins come from modules the binary already links, so `go.mod` gains two `tool` lines and they always match the runtime | A second module to bump by hand; run as `go tool -modfile=tools/go.mod buf` |
| B. Everything in `go.mod` | One file | buf adds about 75 modules to `go.mod` and moves `google.golang.org/protobuf` to a pre-release pseudo-version for the daemon too, as tools share the module graph |
| C. buf's remote plugins | No local plugins | Network and BSR rate limits on every generation; versions pinned only by name |
| D. protoc from the system | Common | Not pinned; one more thing to install on every machine and in CI |

Cost: the static linux/amd64 binary grows from 30.3 MB to 31.0 MB (+0.7 MB, +2.3%; 30,265,504 to
30,970,016 bytes, `just build`). One new module, `connectrpc.com/connect/v2` (Apache-2.0);
`google.golang.org/protobuf` was already linked.

## Web panel (#132)

`lotsman web` runs as the system user `lotsman-web` (unit `deploy/lotsman-web.service`: no
capabilities, `ProtectSystem=strict`, `StateDirectory=lotsman-web`). Its primary group is
`lotsman-web`, which is what lets it connect to the control socket; it cannot read `lotsman.yaml`
or `lotsman.db`. It listens on loopback behind the operator's TLS reverse proxy.

```
browser ──https──▶ reverse proxy ──http──▶ lotsman web ──Connect, unix socket──▶ lotsman serve
                                           scopes, origin,          (trusts its peer)
                                           sessions (#133),
                                           links (#134), UI
```

- Forwarding: `/api/<procedure>` goes to `<procedure>` on the socket through
  `httputil.ReverseProxy` over a unix-socket transport, with `FlushInterval: -1` so each stream
  message is passed on at once. The panel forwards bytes, not messages: it needs no code per RPC,
  and the daemon stays the only place that decodes and validates them. The scope table
  (`internal/web.Scopes`) is keyed by procedure name; a test compares it with the descriptors, so
  a new RPC without a scope fails CI, and a procedure missing from the table is refused at run time.
- Scopes: operator (every service but `UserSelfService`), user (`UserSelfService`), internal
  (`UserSelfService.ResolveLink`: the panel resolves links itself; a browser never reaches it) and
  public (none yet). An `Authenticator` interface decides operator and user; until #133 and #134 it
  is `NoAuth`, which refuses both with `unauthenticated`. There is no bypass flag: integration
  tests replace the authenticator inside their own test binary.
- What reaches the daemon: only the RPC protocol headers (`Content-Type`, `Content-Encoding`,
  `Accept-Encoding`, `Te`, `Connect-*`, `Grpc-*`, `X-Grpc-Web`). Cookies, credentials, forwarded
  addresses and any `Lotsman-*` header from the browser are dropped; for user-scope calls the panel
  then sets `Lotsman-User` to the user it resolved. It is an allowlist rather than stripping
  `Lotsman-*` alone, so a future trusted header cannot leak through by default.
- Browser checks: API calls and every non-GET/HEAD request need `Origin` equal to `public_url`'s
  origin; `Host` is never used, so DNS rebinding and a spoofed `Host` get nowhere. Plain page loads
  carry no `Origin` and serve only the public UI. `X-Forwarded-For` is read only when the peer is in
  `trusted_proxies`, walking back from the last hop to the first untrusted address; the result is
  in the request context for rate limits (#133, #134) and logs. Every response carries a
  `default-src 'self'` CSP with nothing inline and `frame-ancestors 'none'`, `X-Frame-Options:
  DENY`, `Referrer-Policy: no-referrer`, `nosniff` and `Cross-Origin-Opener-Policy: same-origin`;
  API responses are `no-store`.
- Limits: only POST reaches the API (no RPC is marked idempotent, so Connect never uses GET); a
  request body is read whole, at most 64 KiB within 10 s, before forwarding (the API has no client
  streams). The server has `ReadHeaderTimeout` 10 s and `IdleTimeout` 2 min but no `ReadTimeout`
  or `WriteTimeout`: on HTTP/1 either would end long-running `Watch` streams, so the body read and
  each static file write get their own deadlines instead.
- UI: `internal/web/dist` is embedded with `//go:embed all:dist`; git tracks only its `.gitkeep`.
  Without `index.html` every page is a placeholder generated in Go, so `go build` needs no Node.
  An existing file is served as is (`assets/` with a one-year immutable cache, as Vite hashes those
  names); any other path without an extension gets `index.html` (`no-cache`), so client routes such
  as `/u/<token>` load the app; a missing path with an extension is a 404 rather than HTML in place
  of a script.
- State: `web.db` (SQLite, `0600`) in the state directory; the first migration creates the
  `passkeys`, `sessions` and `enrollments` tables for #133, which may reshape them freely before
  the first release. A database from a newer panel is refused rather than misread.

| Option | For | Against |
|---|---|---|
| **A. Reverse proxy of the HTTP stream (chosen)** | No code per RPC; protocol-agnostic (Connect, gRPC-Web); the daemon's errors and streams pass through unchanged | The panel cannot filter inside messages; the daemon must enforce per-user limits, as #134 plans anyway |
| B. Re-implementing each service as a Connect client of the daemon | Could reshape or filter responses in the panel | One handler per RPC, written twice; a second place to validate; stream plumbing per RPC |

| Config option | For | Against |
|---|---|---|
| **A. `/etc/lotsman/web.yaml`, `0640 root:lotsman-web` (chosen)** | The panel sees only its own settings; same format and strict parsing as `lotsman.yaml`; room for #133's session settings | A second file to install |
| B. A `web:` section in `lotsman.yaml`, given to the panel with systemd `LoadCredential=` | One file | The panel would read the daemon's whole config, which is root's alone on purpose; couples the two parsers |
| C. Flags in the unit | No file | Lists such as `trusted_proxies` in a command line; every setting change edits the unit |

Cost: the static linux/amd64 binary grows by about 0.2 MB (30,982,304 to 31,174,816 bytes, +0.6%,
without a built UI). No new module: `net/http/httputil`, `embed` and the already linked
`modernc.org/sqlite` and `connectrpc.com/connect/v2`.

## Traffic counters (#129)

`StatusService.Watch` carries each peer's and each upstream's bytes since the daemon started and
their rates, plus a gateway total. Directions are from the client's side: down is towards the
client.

- Peers: the downstream device's per-peer counters, from the same UAPI dump that `Status` and the
  metrics read. They count the tunnel's packets as sent and received, so encrypted, with AWG's
  overhead.
- Upstreams, every kind: the kernel's counters of the upstream's TUN, `lm-up-<name>`, read over
  netlink (`traffic.InterfaceCounters`). The kernel transmits into the TUN what the clients send
  (up) and receives from it what the AWG device or proxy stack writes back (down). Netlink, not
  `/sys/class/net`, because sysfs shows the network namespace it was mounted in, not the daemon's.
  The counts include the upstream's health probes: DNS and handshake probes are a few hundred
  bytes a round, a transfer probe (#87) shows as a burst.
- Total: every peer's bytes, peers removed since included; its rates are the current peers' sum.

| Option for upstreams | For | Against |
|---|---|---|
| **A. The kernel's counters of the upstream's TUN (chosen)** | One meaning for every kind, so AWG and proxy upstreams compare; the daemon holds the TUN, so the counters survive a helper's crash; no change to the helper protocol and no helper request every second | Counts probes; IP bytes, not the provider's wire bytes |
| B. Byte counters in the helper's relay, over the `stats` request | Exactly what the flows carried | A helper request per upstream every second, which waits up to 2 s for a stuck helper; counters restart with each helper; payload bytes, unlike AWG; a protocol change |
| C. The provider peer's counters for AWG (as the metrics), B for proxies | Reuses an existing source for AWG | Two meanings of bytes side by side in one table |

The helper still reports only flow counts; nothing in it changed.

**Sampler.** `internal/traffic` holds the logic behind a `Source` interface (`Daemon.Counters`
in production, a fake in tests). One sampler per daemon reads every counter each second
(`TrafficInterval`), started by `Run` and stopped before the devices close; every Watch stream
reads its latest sample, so the cost does not grow with viewers. A series' total is the sum of
the deltas between readings:

- the first reading of a series counts in full: the daemon creates its devices, so their counters
  start with it, and a peer or upstream added later gained its bytes since the previous sample;
- a counter that goes back restarted (an interface recreated on reload, a peer re-added): what it
  holds is new, and the total keeps growing;
- a series missing from a reading, such as an upstream whose interface is being recreated, leaves
  the snapshot but not the sampler, so it keeps its total when it comes back. Peer rows and
  upstream names are few, so they are never forgotten;
- rates divide by the time since the series' previous reading, on `time.Now`'s monotonic clock, so
  a wall-clock step changes nothing; no elapsed time means no rate rather than a huge one.

A rate is over the daemon's last 1 s sample, whatever the stream's interval. The panel asks for
1 s; a stream at 5 s gets 1 s rates every 5 s, not 5 s averages. A peer or upstream the sampler
has not read yet, such as one added since its last sample, has no `traffic` in the snapshot, and
`total` is unset before the first sample. `lotsman status` is unchanged: it reads the status file
the daemon writes every health round, where a rate would be stale.

**Cost.** Measured in the Linux test container (Docker on an Apple M1 Max, arm64,
`BenchmarkTrafficSample` and `BenchmarkInterfaceCounters`): one sample of a gateway with 50 peers
and two upstreams takes about 0.2 ms, 85 % of it the downstream device's UAPI dump, plus about
0.1 ms per upstream for the netlink read. That is under 0.05 % of one core with a handful of
upstreams. The static linux/amd64 binary grows by 24 KB (30,982,304 to 31,006,880 bytes); no new
module.

## Open items

- [x] The official Amnezia client (macOS) connects to a Lotsman downstream device on a real host
      and exits through a provider AmneziaWG upstream (2026-09-17).
- [x] Throughput measured (below).

## Throughput (2026-09-17)

Host: VPS, 2 vCPU Xeon E5-2630 v4 @ 2.2 GHz, Debian 13, one AmneziaWG provider upstream.
Client: macOS with the Amnezia app over a residential line. `iperf3 -P 4`, 8 s; HTTPS via Cloudflare
speed endpoints, 50 MB.

| Leg | Down | Up |
|---|---|---|
| A. Client → host directly, no VPN (`iperf3`) | 78 Mbit/s | 331 Mbit/s |
| B. Client → host through the Lotsman tunnel, one encapsulation (`iperf3` to `lm0`) | 46 Mbit/s | 133 Mbit/s |
| C. Client → internet through the whole chain (HTTPS) | 11 Mbit/s | 66 Mbit/s |
| Host → internet through the provider upstream only (HTTPS, source bound to the tunnel address) | 17 Mbit/s | 82 Mbit/s |
| Host → internet directly (HTTPS) | 594 Mbit/s | — |

Reading: the provider leg is the bottleneck (17 Mbit/s down through the provider against 594 direct
from the same host); Lotsman adds no measurable limit on top of it. The daemon peaked at 4 % of one
core during the tests, so userspace double encryption is far from being the constraint at these
rates. Leg A shows the residential line itself caps downloads below 80 Mbit/s. Client MTU in this
setup is 1260 because of the provider's own MTU and S4.
