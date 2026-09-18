# Lotsman

A self-hosted [AmneziaWG 2.0](https://docs.amnezia.org/documentation/amnezia-wg/) gateway that
routes each user to a working upstream VPN provider.

Users connect with the official Amnezia app to a single server you run. Lotsman terminates their
tunnel and forwards traffic into one of several upstream AmneziaWG tunnels — your own subscriptions
at providers like RedShield — choosing the upstream by the user's profile (geo, provider) and by
live health. When an upstream dies, its users move to the next one without reconnecting.

```
Amnezia app ──AWG2──▶ Lotsman ──AWG2──▶ provider NL ──▶ internet
                         │
                         └────AWG2──▶ provider DE ──▶ internet
```

*Lotsman* (лоцман) is the harbour pilot who knows which channel is passable today.

## What you get

- One server, one config per device per profile, zero custom client software.
- Automatic failover between upstreams with sticky selection (no flapping); connections already
  open through a live upstream stay on it while new ones move.
- Fail-closed: if no upstream is healthy for a user, their traffic is dropped, never leaked through
  the server's own IP.
- MTU and TCP MSS handled for the double encapsulation; DNS follows the chosen upstream.
- Upstreams may be plain WireGuard or AmneziaWG; clients use the Amnezia app.
- A single static binary, a YAML file, and SQLite.

## What you do not get (yet)

- All users behind one upstream share its exit IP and its reputation.
- IPv6 inside the tunnel, a web UI, more than one node.
- Throughput of a kernel WireGuard: crypto runs in userspace twice per packet.

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
just lint
```

Code, comments and commits are in English; commits follow Conventional Commits.

## License

MIT
