# Architecture

Lotsman is a self-hosted AmneziaWG 2.0 gateway. Users connect with the official Amnezia client;
Lotsman terminates their tunnel and forwards traffic into one of several upstreams, AmneziaWG tunnels
or Outline (Shadowsocks) servers (the operator's own provider subscriptions), chosen by the user's
profile preference and live health.

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
| State | YAML for upstreams/profiles, SQLite for users/peers | Operators edit and version the YAML; users are created at runtime. |
| Fail-closed | Mandatory | User traffic must never leave via the host's own IP. nftables admits only authorized peers from `lm0` into an `lm-up-*`; all other forwarding from `lm0` is dropped. |
| Host firewalls | Accept rules inserted into foreign forward chains | Docker's `FORWARD DROP` and ufw would otherwise silently drop forwarded traffic; same approach as Tailscale. |
| Proxy upstreams | A userspace TCP/IP stack per upstream on its own TUN, flows re-originated through a pluggable dialer (Shadowsocks first), each in its own unprivileged helper process | Keeps kernel routing, pinning and fail-closed identical for every upstream kind; a panic in the stack takes down one upstream, not the gateway; see [Proxy upstreams](#proxy-upstreams-outline). |

## AmneziaWG implementation notes (verified 2026-09-17)

- `github.com/amnezia-vpn/amneziawg-go/v3` is pinned to the tag **v3.1.20260828** (AWG 3.1,
  updated 2026-11-16). It stores `device.headers.*` atomically and applies `IpcSet` header and
  padding changes under a lock, so the v0.2.19 data race (headers rewritten by any `IpcSet` while
  the receive goroutine reads them) is gone. `ipackets` (I1–I5) is still written without
  synchronisation, so Lotsman sets them only before `Up`. With no 3.x keys the runtime is AWG 2.0
  on the wire; `TestAWG2RuntimeInterop` checks this against v0.2.19, a test-only dependency.
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
equivalence has not been measured. A concrete listen address fixes reply source;
wildcard replies follow host routing.

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
  response as fetched. The helper parses either with `proxy.ParseConfig`, which loading uses for a
  static key file too, and on `server` only when the text changed.
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
- Privileges: a root daemon starts helpers as uid and gid 65534 with no supplementary groups, so
  exec leaves them no capabilities (no file capabilities on the binary); they need only the
  inherited descriptor and outbound sockets. The environment is empty but for `SSL_CERT_FILE` and
  `SSL_CERT_DIR` when the daemon has them (TLS roots for wss:// keys), and the working directory `/`.
  A helper makes itself non-dumpable, so other processes of uid 65534 cannot read its memory, which
  holds the key. They can still signal it, i.e. kill it, which the daemon handles as a crash; a
  dedicated system user would close that (a follow-up; uid 65534 for now). The systemd unit's bounding set gains `CAP_SETUID` and
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

## Open items

- [x] The official Amnezia client (macOS) connects to a Lotsman downstream device on a real host
      and exits through a RedShield upstream (2026-09-17).
- [x] Throughput measured (below).

## Throughput (2026-09-17)

Host: Selectel VPS, 2 vCPU Xeon E5-2630 v4 @ 2.2 GHz, Debian 13, one RedShield upstream (LT).
Client: macOS with the Amnezia app over a residential line. `iperf3 -P 4`, 8 s; HTTPS via Cloudflare
speed endpoints, 50 MB.

| Leg | Down | Up |
|---|---|---|
| A. Client → host directly, no VPN (`iperf3`) | 78 Mbit/s | 331 Mbit/s |
| B. Client → host through the Lotsman tunnel, one encapsulation (`iperf3` to `lm0`) | 46 Mbit/s | 133 Mbit/s |
| C. Client → internet through the whole chain (HTTPS) | 11 Mbit/s | 66 Mbit/s |
| Host → internet through the RedShield upstream only (HTTPS, source bound to the tunnel address) | 17 Mbit/s | 82 Mbit/s |
| Host → internet directly (HTTPS) | 594 Mbit/s | — |

Reading: the provider leg is the bottleneck (17 Mbit/s down through RedShield against 594 direct
from the same host); Lotsman adds no measurable limit on top of it. The daemon peaked at 4 % of one
core during the tests, so userspace double encryption is far from being the constraint at these
rates. Leg A shows the residential line itself caps downloads below 80 Mbit/s. Client MTU in this
setup is 1260 because of the provider's own MTU and S4.
