# Architecture

Lotsman is a self-hosted AmneziaWG 2.0 gateway. Users connect with the official Amnezia client;
Lotsman terminates their tunnel and forwards traffic into one of several upstream AmneziaWG tunnels
(the operator's own provider subscriptions), chosen by the user's profile preference and live health.

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
| Fail-closed | Mandatory | User traffic must never leave via the host's own IP. nftables drops anything from `lm0` not routed into an `lm-up-*`. |
| Host firewalls | Accept rules inserted into foreign forward chains | Docker's `FORWARD DROP` and ufw would otherwise silently drop forwarded traffic; same approach as Tailscale. |

## AmneziaWG implementation notes (verified 2026-09-17)

- `github.com/amnezia-vpn/amneziawg-go` is pinned to the **master pseudo-version
  `v0.2.20-0.20260724121833-457d920a1a7d`**, not a tag. The `v1.0.x` tags are *older* (July 2025,
  AWG 1.5 only: `i1..i5`, `j1..j3`, `itime`, no `s3`/`s4`, no header ranges); the live line is
  `v0.2.x`. The latest tag, v0.2.19, has a data race: every `IpcSet` on a running device — including
  a plain peer add/remove — rewrites `device.headers.*` while the receive goroutine reads them.
  `master` stores them atomically. It also carries "AWG 3+" knobs (`header_protection_key`,
  `content_padding_addition`, timing ranges) that Lotsman does not use yet. Move back to a tag once
  one includes the fix.
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
  proves it against an untouched `wireguard-go`. The downstream side is AmneziaWG 2.0 only, because
  obfuscation parameters are per device, not per peer.

## MTU

Client tunnel MTU = `min(1500 − 60 − S4_down, min over upstreams (upstream MTU − S4_up))`.
60 bytes = IPv4 (20) + UDP (8) + WireGuard transport header (16) + Poly1305 tag (16). The same value is
set on `lm0` and written into client configs; nftables additionally clamps TCP MSS to the route MTU.

## Failover semantics

Switching a peer to another upstream changes its exit IP: established TCP flows break; the user's
tunnel to Lotsman stays up and new flows work immediately. Conntrack-based flow pinning (drain old
flows on the old upstream while it is still alive) is a v2 item.

## Open items

- [ ] Confirm the official Amnezia client connects to a Lotsman downstream device on a real host
      (the end-to-end test uses amneziawg-go on both sides).
- [ ] Throughput numbers through the full chain (`iperf3`).
