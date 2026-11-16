# Lotsman

A self-hosted [AmneziaWG](https://docs.amnezia.org/documentation/amnezia-wg/) gateway (AWG 3.1 by
default, AWG 2.0 selectable) that
routes each user to a working upstream VPN provider.

Users connect with the official Amnezia app to a single server you run. Lotsman terminates their
tunnel and forwards traffic into one of several upstreams — AmneziaWG tunnels or Outline
(Shadowsocks) servers, your own subscriptions at providers like RedShield — choosing the upstream by the user's profile (geo, provider) and by
live health. When an upstream dies, its users move to the next one without reconnecting.

```
Amnezia app ──AWG──▶ Lotsman ──AWG──▶ provider NL ──▶ internet
                        │
                        ├────AWG──▶ provider DE ──▶ internet
                        │
                        └──Outline (Shadowsocks)──▶ server ──▶ internet
```

*Lotsman* (лоцман) is the harbour pilot who knows which channel is passable today.

## What you get

- One server, one config per device per profile, zero custom client software.
- Automatic failover between upstreams with sticky selection (no flapping); connections already
  open through a live upstream stay on it while new ones move.
- Fail-closed: if no upstream is healthy for a user, their traffic is dropped, never leaked through
  the server's own IP.
- MTU and TCP MSS handled for the double encapsulation; DNS follows the chosen upstream.
- Two upstream kinds, mixed freely in one profile so it can fail over between protocols:
  - `awg` (default): plain WireGuard or AmneziaWG 2.0/3.x provider configs (including header
    protection and random trailers); AmneziaWG 1.5 configs are not supported.
  - `outline`: an Outline (Shadowsocks) access key, static `ss://…` or dynamic `ssconf://…`
    (fetched over HTTPS and refreshed), including Outline's Shadowsocks-over-WebSocket keys
    (`transport:` configs). A userspace TCP/IP stack in an unprivileged helper process
    per upstream carries TCP and UDP through it; ICMP (ping) does not pass.

  Clients use the Amnezia app whatever the upstream: 5.0.1.5 or later for the default `awg: 3`; set
  `awg: 2` for older apps and Android 7–8.
- Optional Prometheus metrics: per-upstream health and per-client traffic.
- A single static binary, a YAML file, and SQLite.

## What you do not get (yet)

- All users behind one upstream share its exit IP and its reputation.
- IPv6 inside the tunnel, a web UI, more than one node.
- Throughput of a kernel WireGuard: crypto runs in userspace twice per packet. Outline upstreams also
  terminate every TCP connection in userspace.
- Outline keys beyond plain Shadowsocks and Shadowsocks over WebSocket (other `$type`s of Outline's
  config language, such as TLS fragmentation or chained proxies).

## Quick start

Needs Linux with nftables, root, and Go 1.27 to build (or a release binary).

```bash
just build                       # dist/lotsman-linux-amd64 (or: just build arm64)
sudo install -m 0755 dist/lotsman-linux-amd64 /usr/local/bin/lotsman
sudo mkdir -p /etc/lotsman/upstreams
sudo cp deploy/lotsman.example.yaml /etc/lotsman/lotsman.yaml
# put your provider configs into /etc/lotsman/upstreams/ and edit lotsman.yaml
sudo lotsman config check
sudo cp deploy/lotsman.service /etc/systemd/system/
sudo systemctl enable --now lotsman
sudo lotsman user add alice -profile nl -qr # prints the config (and a QR code) for the Amnezia app
sudo lotsman upstream status
```

See [docs/OPERATIONS.md](docs/OPERATIONS.md) for the full guide and
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how it works and why.

## Responsibility

Lotsman is a tool for using *your own* provider subscriptions from one place. Whether sharing an
upstream account with other people is allowed is between you and your provider; read their terms.
The authors take no position and no responsibility.

## Development

```bash
just test              # unit tests, any platform
just test-integration  # Linux kernel tests in a privileged Docker container
just lint              # gofmt, vet, go mod verify, staticcheck, govulncheck (host and Linux)
```

`staticcheck` and `govulncheck` are pinned as `tool` dependencies in `go.mod`, so there is
nothing to install; `just lint` runs them with `go tool`. CI runs the same recipe.

Code, comments and commits are in English; commits follow Conventional Commits. Releases are
cut from tags; their notes come from [CHANGELOG.md](CHANGELOG.md), so add an entry under
`Unreleased` with your change.

## License

MIT
