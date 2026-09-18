# Operations

## Requirements

- Linux with nftables and TUN (any mainstream distribution from the last few years).
- Root: Lotsman creates TUN interfaces, sets sysctls, and programs policy routing and nftables.
- UDP port reachable by your users (default 51820).
- One AmneziaWG config per upstream from your provider, routing `0.0.0.0/0`.

## Install

```bash
just build
sudo install -m 0755 dist/lotsman-linux-amd64 /usr/local/bin/lotsman
sudo mkdir -p /etc/lotsman/upstreams
sudo cp deploy/lotsman.example.yaml /etc/lotsman/lotsman.yaml
sudo cp deploy/lotsman.service /etc/systemd/system/
```

Put each provider config into `/etc/lotsman/upstreams/<name>.conf`, reference them in
`lotsman.yaml`, then:

```bash
sudo lotsman config check
sudo systemctl enable --now lotsman
```

`config check` parses everything, resolves endpoints and prints the interface each upstream gets.

### Removing it

`deploy/uninstall.sh` stops the service, removes the binary, unit, config and state directory,
cleans up any leftover interfaces or rules, and restores `net.ipv4.ip_forward` to the value it had
before the install. It reads `/etc/lotsman/INSTALLED`, a manifest you write at install time:

```
ip_forward_before=0
path=/usr/local/bin/lotsman
path=/etc/systemd/system/lotsman.service
path=/etc/lotsman
path=/var/lib/lotsman
```

### Docker

```bash
docker build -f deploy/Dockerfile -t lotsman .
docker run -d --name lotsman \
  --cap-add NET_ADMIN --device /dev/net/tun \
  --sysctl net.ipv4.ip_forward=1 --sysctl net.ipv4.conf.default.rp_filter=2 \
  -p 51820:51820/udp \
  -v /etc/lotsman:/etc/lotsman:ro -v lotsman-state:/var/lib/lotsman \
  lotsman
docker exec lotsman lotsman user add alice -profile nl
```

The two `--sysctl` flags matter: `/proc/sys` is read-only inside the container, so Lotsman can
only verify them, not set them.

## Upgrading

```bash
just build
scp dist/lotsman-linux-amd64 server:/tmp/lotsman
sudo /tmp/lotsman config check              # the new binary must accept the current config
sudo install -m 0755 /tmp/lotsman /usr/local/bin/lotsman
sudo systemctl restart lotsman              # clients reconnect on their own within seconds
sudo lotsman upstream status
```

If the new version changes the database schema, the daemon snapshots the old database to
`state_dir/lotsman.db.v<N>-<timestamp>.bak` before migrating. To roll back: stop the service,
reinstall the previous binary, move the backup over `lotsman.db`, start. The unit runs
`config check` before `serve` and stops retrying after five failed starts in five minutes, so a
broken upgrade shows up in `systemctl status lotsman` instead of crash-looping.

## Configuration

`/etc/lotsman/lotsman.yaml` — see `deploy/lotsman.example.yaml` for a commented example.

| Key | Meaning |
|---|---|
| `listen` | UDP address the downstream device binds to. |
| `endpoint` | `host:port` written into client configs. |
| `subnet` | IPv4 range for clients; `.1` is the gateway. |
| `dns` | Resolvers written into client configs; queries go through the client's upstream. |
| `state_dir` | Server identity, SQLite database, pid and status files. |
| `upstreams[]` | `name` (≤ 9 chars, becomes `lm-up-<name>`), `conf` path, free-form `geo` and `provider` labels. |
| `profiles[]` | `name` and `prefer`: ordered tiers of `{geo, provider}` matchers; empty = any upstream. |
| `health` | `interval`, `probe` (`ip:port` reachable through every upstream), `down_after`, `up_after`. |

`systemctl reload lotsman` (SIGHUP) applies the file live, without dropping tunnels: upstreams are
added, removed or replaced (a changed provider config counts as a replacement), profiles and
`health` are swapped in, and every client is re-evaluated. Clients whose upstream was removed are
blocked for a moment and rerouted on the same pass. `listen`, `subnet` and `state_dir` cannot change
live; a reload that touches them is rejected as a whole and logged. A config that fails validation is
logged and ignored. A changed client MTU only reaches clients that re-import their config.

## Users

```bash
lotsman user add alice -profile nl                   # config for alice's "default" device
lotsman user add alice -profile nl -device phone -qr # her phone: own key, QR on stderr
lotsman user add alice -profile eu -device phone     # a second profile on the same phone
lotsman user show alice -profile nl -device phone
lotsman user list
lotsman user rm alice -profile eu -device phone      # one config
lotsman user rm alice -device phone                  # every config of that device
lotsman user rm alice                                # everything
```

WireGuard binds one key to one endpoint, so a config must never be active on two devices at the
same time — give each device its own with `-device`. A device has one config per profile; which
config is active decides where the traffic exits. The config goes to stdout (redirect it to a
file), the QR code to stderr. Adding or removing takes effect immediately when the daemon is
running (the CLI nudges it), otherwise on the next start.

## Watching it

```bash
lotsman upstream status
journalctl -u lotsman -f
ip rule show            # one "from <client>/32 lookup 100x" per routed client
nft list table inet lotsman
```

Health transitions and every routing decision are logged. `status.json` in the state directory is
what `upstream status` prints, refreshed every health interval.

## How failover behaves

- An upstream is *down* after `down_after` consecutive failed probes and *up* again after `up_after`
  successes. Defaults: 45 s to declare dead, 30 s to trust it again.
- A client stays on its upstream while it is healthy, even if a sibling in the same tier becomes
  faster. It moves when its upstream goes down, or when an upstream in a *better* tier recovers.
- Moving changes the exit IP for *new* connections only. Connections a client already has open
  through an upstream that is still alive stay pinned to it (conntrack marks plus a per-client
  `fwmark` rule) until they close; only when the old upstream is dead do they fail. The tunnel to
  Lotsman itself never drops.
- No healthy upstream for a profile: the client's rule is removed and nftables drops its packets.
  Nothing leaves through the server's own address.

## Identity and secrets

`state_dir/identity.json` holds the server key and the S1–S4/H1–H4 parameters every client config
embeds. Deleting it invalidates every issued config. `state_dir/lotsman.db` holds client private
keys so configs can be re-shown; treat both files like the server key (mode 0600, root only).

## Troubleshooting

**The provider changed its server IP.** Nothing to do if the provider config names a hostname:
when an upstream's probes fail, Lotsman re-resolves the name and, if the address changed, points
the running tunnel at it (`upstream endpoint changed` in the log). A config with a literal IP needs
editing and `systemctl reload lotsman`.

**Clients connect but nothing loads.** Check `upstream status`; if everything is `down`, the
probes cannot get out. Test an upstream config by hand with `awg-quick`, and confirm the probe
target is reachable through it.

**Sites open, large downloads or video hang.** MTU. `config check` prints the client MTU; make
sure clients actually imported the config with that `MTU` line. Try a lower value in a provider
config's `MTU` if the provider's path is narrower than it claims.

**Replies do not come back through an upstream.** `sysctl net.ipv4.conf.lm-up-*.rp_filter` must
be `2` (loose). Lotsman sets it, but a hardening tool may reset it; in Docker set
`net.ipv4.conf.default.rp_filter=2` at start.

**Another firewall on the host.** Lotsman owns `table inet lotsman`. Docker (`iptables -P FORWARD
DROP`) and ufw add their own forward chains whose drop would win over Lotsman's accept, so on start
Lotsman also inserts two `accept` rules tagged `comment "lotsman"` at the top of every other
forward chain and removes them on shutdown. Only nftables-backed chains are handled; on a host
still using `iptables-legacy` allow `lm0 → lm-up-*` and the replies by hand. The UDP listen port
must be open for input.

**Restart left rules behind.** Lotsman removes its rules, routes and nftables table on shutdown and
recreates them on start, so a crash is harmless.
