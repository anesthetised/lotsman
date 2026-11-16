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
| Proxy upstreams | A userspace TCP/IP stack per upstream on its own TUN, flows re-originated through a pluggable dialer (Shadowsocks first) | Keeps kernel routing, pinning and fail-closed identical for every upstream kind; see [Proxy upstreams](#proxy-upstreams-outline). |

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
`proxy.Key` parses an access key and builds the dialers for a resolved server address; `ss://` is
the only one so far, static or fetched from an `ssconf://` URL. The daemon sees either kind through
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
  Shadowsocks object, or a provider `error`. The last good response is cached per upstream in the
  state directory under a SHA-256 fingerprint of the URL, so a restart while the URL is blocked
  still has a key, and a key never serves a URL it did not come from. Redirects are followed only
  to `https://`; the Outline client follows any.
- Health: a TCP handshake proves nothing through a proxy (the stack completes it itself, and
  outline-ss-server keeps a wrong-key connection open silently), so a proxy upstream passes a round
  only when DNS queries to a `health.dns` resolver are answered over UDP and over TCP. Latency is
  the UDP query's, one round trip to the resolver like the AWG handshake probe.

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
