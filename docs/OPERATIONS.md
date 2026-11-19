# Operations

## Requirements

- Linux with nftables and TUN (any mainstream distribution from the last few years).
- Root: Lotsman creates TUN interfaces, sets sysctls, and programs policy routing and nftables.
- UDP port reachable by your users (default 51820).
- Per upstream, an AmneziaWG config from your provider routing `0.0.0.0/0`, an Outline access
  key (`ss://…`, `ssconf://…`, or an Outline `transport:` config such as a WebSocket key), or a
  VLESS + REALITY link (`vless://…`).

## Install

```bash
just build
sudo install -m 0755 dist/lotsman-linux-amd64 /usr/local/bin/lotsman
sudo install -d -m 0700 /etc/lotsman /etc/lotsman/upstreams
sudo install -m 0600 deploy/lotsman.example.yaml /etc/lotsman/lotsman.yaml
sudo cp deploy/lotsman.service /etc/systemd/system/
getent passwd lotsman-proxy >/dev/null || sudo useradd --system --user-group --no-create-home \
  --shell /usr/sbin/nologin lotsman-proxy
getent group lotsman-web >/dev/null || sudo groupadd --system lotsman-web
```

The `useradd` step creates the system user that proxy upstream helpers run as (see
[Helper processes](#helper-processes)); it has no login shell and no home. Skip it if you have no
`outline` or `vless` upstreams. Without it the helpers fall back to uid/gid 65534 (`nobody`) and
the daemon logs a warning once.

The `groupadd` step creates the group that may use the [control socket](#control-socket), the web
panel's. Without it the socket is root-only and the daemon logs a warning at start. The
[web panel](#web-panel) has its own install steps.

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
nf_conntrack_acct_before=0
path=/usr/local/bin/lotsman
path=/etc/systemd/system/lotsman.service
path=/etc/lotsman
path=/var/lib/lotsman
path=/etc/systemd/system/lotsman-web.service
path=/var/lib/lotsman-web
user=lotsman-proxy
user=lotsman-web
group=lotsman-web
```

Take the two `_before` values from `sysctl -n net.ipv4.ip_forward` and
`sysctl -n net.netfilter.nf_conntrack_acct` before the first start. Leave
`nf_conntrack_acct_before=` empty when the second prints nothing: `nf_conntrack` is not loaded
yet, and a fresh load starts at 0 (see [Live connections](#live-connections)).

Add `user=lotsman-proxy` only if your install created that user (the `useradd` above ran),
`user=lotsman-web` only if you installed the [web panel](#web-panel) and created its user, and
`group=lotsman-web` only if it created that group: the script removes each `user=` entry with
`userdel` and then each `group=` entry with `groupdel`, so a user or group that existed before
Lotsman is left alone. If either fails, the script says so and carries on.

If you installed Caddy for the [web panel](#reverse-proxy) from Caddy's apt repository, add:

```
package=caddy
path=/etc/apt/sources.list.d/caddy-stable.list
path=/usr/share/keyrings/caddy-stable-archive-keyring.gpg
```

The script removes the `path=` entries with the rest and then runs `apt-get purge -y` on every
`package=` entry (Debian and Ubuntu only). Record `package=caddy` only if the install put Caddy
there for the panel: a Caddy that was already on the host, serving other sites, would be purged
too.

### Docker

```bash
docker build -f deploy/Dockerfile -t lotsman .
docker run -d --name lotsman \
  --cap-add NET_ADMIN --device /dev/net/tun \
  --sysctl net.ipv4.ip_forward=1 --sysctl net.ipv4.conf.default.rp_filter=2 \
  -p 51820:51820/udp \
  -v /etc/lotsman:/etc/lotsman:ro -v lotsman-state:/var/lib/lotsman \
  lotsman
docker exec lotsman lotsman profile add nl geo=NL
docker exec lotsman lotsman user add alice -profile nl
```

The two `--sysctl` flags matter: `/proc/sys` is read-only inside the container, so Lotsman can
only verify them, not set them. For byte counts in [live connections](#live-connections), also
pass `--sysctl net.netfilter.nf_conntrack_acct=1`; the container may fail to start with it
when the host has no `nf_conntrack` loaded.

## Upgrading

```bash
just build
scp dist/lotsman-linux-amd64 server:/tmp/lotsman
sudo /tmp/lotsman config check              # the new binary must accept the current config
sudo install -m 0755 /tmp/lotsman /usr/local/bin/lotsman
sudo cp deploy/lotsman.service /etc/systemd/system/ && sudo systemctl daemon-reload  # if the unit changed
sudo systemctl restart lotsman              # clients reconnect on their own within seconds
sudo systemctl try-restart lotsman-web      # the web panel, if installed, runs the same binary
sudo lotsman upstream status
```

Coming from a version that kept profiles in `lotsman.yaml`, `config check` fails until they are
moved; see [Moving profiles out of lotsman.yaml](#moving-profiles-out-of-lotsmanyaml).

If the new version changes the database schema, the daemon snapshots the old database to
`state_dir/lotsman.db.v<N>-<timestamp>.bak` before migrating. To roll back: stop the service,
reinstall the previous binary, move the backup over `lotsman.db`, start. The unit runs
`config check` before `serve` and stops retrying after five failed starts in five minutes, so a
broken upgrade shows up in `systemctl status lotsman` instead of crash-looping.

The database, its `-wal`/`-shm` files and the backups hold client private keys, so Lotsman keeps
them 0600 and tightens existing ones on start; only the daemon's user can read them. The state
directory's mode is yours to set (the unit uses `StateDirectoryMode=0700`).

## Configuration

`/etc/lotsman/lotsman.yaml` — see `deploy/lotsman.example.yaml` for a commented example.

| Key | Meaning |
|---|---|
| `listen` | Numeric UDP address and family the downstream device binds to; `0.0.0.0` is IPv4-only and `[::]` is IPv6-only. |
| `awg` | AmneziaWG generation of the listener, for all clients: `3` (default; header protection and random trailers, Amnezia app 5.0.1.5 or later) or `2` (AWG 2.0). Restart only. Changing it invalidates issued client configs; re-show them with `user show`. |
| `endpoint` | Nonempty host and numeric UDP port (1–65535) written into client configs; hostname, IPv4, and bracketed IPv6 forms are supported. |
| `subnet` | Canonical IPv4 network prefix, /24 or larger; the first host address is the gateway. |
| `dns` | Resolvers written into client configs; queries go through the client's upstream. |
| `state_dir` | Server identity, SQLite database, pid, status and `serve.lock` files, cached dynamic Outline keys. |
| `upstreams[]` | `name` (≤ 9 chars, becomes `lm-up-<name>`), `kind` (`awg`, the default, `outline` or `vless`), `conf` path (the provider's AmneziaWG config, for `outline` a file with the access key or its `ssconf://` URL, for `vless` a file with the `vless://` link), free-form `geo` and `provider` labels, `tags`: a list of free-form groups (lowercase letters, digits, `-`; no duplicates), and for the proxy kinds `outline` and `vless` `max_flows` (default 4096; see [Outline upstreams](#outline-upstreams) and [VLESS upstreams](#vless-upstreams)). |
| `health` | `interval`, `probe` (an `ip:port` reachable through every AWG upstream, or a list of them), `dns` (a resolver `ip:port`, or a list, queried through every Outline and VLESS upstream; default `1.1.1.1:53`), `down_after`, `up_after`, and the optional `transfer` block (off when absent; see [Throttled upstreams](#throttled-upstreams-transfer-probe)). |
| `history` | `minute_retention` (default `168h`, 7 days; at least `1h`) and `hour_retention` (default `2160h`, 90 days; not shorter than `minute_retention`): how long the [history](#history) keeps samples of a minute and of an hour. Go durations, so hours (`168h`), not days. Applies on reload. |
| `users` | `max_devices`: how many devices a user may have when adding one on their [page](#user-links), default 5; another profile on a device they have does not count. `0` turns adding off, another profile included. The operator's commands are not limited. |

`listen` binds exactly the configured numeric address; hostnames and an omitted
host are unsupported. An occupied port, unavailable family, or unassigned address
fails startup without falling back to wildcard. Native IPv6 zones are supported;
IPv4-mapped IPv6 literals use their IPv4 address. The default `0.0.0.0:51820` no
longer also exposes IPv6. A listen change requires a restart.

Downstream uses a simple UDP socket without AWG's batching/GSO/GRO optimizations;
upstream devices retain their existing bind. Throughput impact is unmeasured.
On Linux its send and receive buffers are 7 MiB, as in AWG's default bind, set
beyond `rmem_max`/`wmem_max` when the daemon has `CAP_NET_ADMIN`; without it they
are capped by those sysctls.
Wildcard replies use host routing to choose their source; use a concrete listen
address when replies must originate from one fixed local IP.

Subnet prefixes with host bits set are rejected: use `10.77.0.0/24`, not
`10.77.0.255/24`. The network address, gateway, and broadcast address are reserved
and are never allocated to clients.

Provider configurations require a nonzero public key for each peer. With active
junk packets (`Jc > 0`), `Jmin` must not exceed `Jmax`; equal bounds produce a
fixed size. Ordered unequal bounds have an exclusive upper limit. Junk packets,
active I signature packets, and the initiation together must fit a 128-packet
send batch, and each generated junk packet must fit the endpoint's UDP payload
limit (65507 bytes for IPv4, 65527 for IPv6).

An omitted or zero provider MTU uses 1420. The MTU minus provider `S4` padding
must be at least 68; MTU plus `S4` and the 32-byte transport header/tag must fit
the endpoint's UDP payload limit. `config check` rejects these invalid settings
before a tunnel is started. These checks do not validate I packet lengths or
the UDP size of S1–S3-padded handshake packets.

AmneziaWG 2.0 and 3.x provider exports load as they are. `HeaderProtectionKey` (base64, 32 bytes)
and `RandomTrailers` must match the provider's server; header protection requires S1–S4 of at least
12, and an all-zero key disables it. Flags take `on`/`off`, `true`/`false` or `1`/`0`. The upper
bound of `ContentPaddingAddition` lowers the client MTU like `S4` does. Timings in seconds
(`RekeyAfterTime`, `RekeyTimeout`, `RejectAfterTime`, `KeepaliveTimeout`, `MaxHandshakeAttempts`)
and `[Peer] PersistentKeepalive` take a single value or a range `a-b`; the runtime picks from the
range each time it uses it, and 0 means its default (keepalive off). Lotsman adds no bounds beyond
the runtime's, except that `RejectAfterTime` may not exceed 3074457345 s, past which the runtime's
key timer overflows. AmneziaWG 1.5 configs
(`J1`–`J3`, `Itime`) are refused; ask the provider for a 2.0 or newer config.

Providers may assign the same tunnel IP. Health probes bind to the selected
upstream interface as well as its source IP, and policy routing selects by both.
Host applications using a provider tunnel must also bind its interface; binding
only the provider source IP no longer selects that provider's route. Provider
outer UDP transport and endpoint DNS resolution still use ordinary host routing.

Startup cleans both legacy source-only and current interface-qualified probe
rules when ownership is clear. If a legacy rule's deletion could also remove a
foreign rule, startup stops with a manual-cleanup error. Uninstall also reports
the collision and preserves ambiguous interfaces and routes for inspection.

Profiles are not in this file: they live in the database, see [Profiles](#profiles). A config that
still has the `profiles:` key of earlier versions fails `config check` and `serve` with the command
that moves them.

`systemctl reload lotsman` (SIGHUP) applies the file live, without dropping tunnels: upstreams are
added, removed or replaced (a changed provider config counts as a replacement), `health` is swapped
in, and every client is re-evaluated. A reload that takes the last upstream from a stored profile the
running config serves is rejected as a whole; change the profile first. A profile that has no
upstream anyway (say, after a restart with an upstream removed) does not stop the start or later
reloads: it is logged as a warning and its peers stay blocked until the profile or the upstreams
change. A replacement is read, validated and its
endpoint resolved before the running upstream is touched (the lookup is bounded by the probe
timeout and does not block status, metrics or client changes); if any of that fails, including an
unreadable provider file, the old upstream keeps running and routing its clients, and the error is
logged and counted in `lotsman_reloads_total{result="error"}`. An upstream that is configured but
cannot be opened (a new one whose file, DNS or TUN fails, or a replacement whose device fails after
the old one closed) stays out of service and is retried every health interval; the error is logged
once as `upstream is not running, will retry every health round`, and `upstream added` marks the
recovery. A failed dataplane update is retried the same way. Clients whose upstream was removed are
blocked for a moment and rerouted on the same pass. `listen`, `subnet` and `state_dir` cannot change
live; a reload that touches them is rejected as a whole and logged. A config that fails validation is
logged and ignored. A changed client MTU only reaches clients that re-import their config.

A peer whose profile is missing from the database (only possible for a database from before
profiles moved there, until they are imported) is blocked on the next reconciliation, even if an
upstream it used stays healthy. Its routes and flow pins are removed and its current assignment is
cleared; reconciliation reports the missing profile until it is added or the peer is removed, and
`config check` and `profile list` warn about it.

If route cleanup fails, the daemon logs the error and retries on subsequent reconciliations.
It retains the previous assignment in status until cleanup succeeds, and finishes removing old
routes and flow pins before routing that peer again, including through a replacement upstream
with the same name. The Linux dataplane revokes firewall authorization before deleting routes,
so a successful revocation blocks traffic even while route cleanup is pending. An error revoking
authorization is reported and retried, not treated as a successful block. Removing a peer also
revokes its downstream key even while route cleanup is pending.

### Outline upstreams

An Outline (Shadowsocks) server is an upstream like any other: profiles prefer it by its labels,
health decides, flows are pinned and fail-closed applies. Put the access key the Outline manager
gave you, one `ss://…` line, into a file and point `conf` at it:

```yaml
upstreams:
  - {name: ol-fi, kind: outline, conf: /etc/lotsman/upstreams/ol-fi.key, geo: FI, tags: [eu]}
```

```bash
sudo install -m 0600 /dev/stdin /etc/lotsman/upstreams/ol-fi.key <<< 'ss://…'
```

The key is a password: keep the file 0600 and root-only, like a provider config. Lotsman never logs
it, and neither error messages nor `config check` quote it. Accepted forms are SIP002
(`ss://base64url(method:password)@host:port`, or a percent-encoded `method:password`) and the
legacy `ss://base64(method:password@host:port)`; the `#name` part is ignored. Ciphers:
`chacha20-ietf-poly1305` (what Outline uses), `aes-256-gcm`, `aes-192-gcm`, `aes-128-gcm`; older
stream ciphers and SIP003 plugins are refused. Outline's `prefix` parameter (salt prefix disguise,
percent-encoded bytes) is applied to TCP, as the Outline client does, and may take at most half of
the salt (16 bytes with chacha20). A hostname server
is resolved at start and on reload (IPv4), and again when the upstream fails its probes, as for
AmneziaWG endpoints; the address change applies to new flows.

**WebSocket keys.** Outline servers can also take Shadowsocks over WebSockets, usually behind a web
server or CDN on port 443. Such a key is not an `ss://` line but an Outline config, in YAML or
JSON, the same text a provider serves from an `ssconf://` URL; put it in the key file as it is:

```yaml
transport:
  $type: tcpudp
  tcp:
    $type: shadowsocks
    endpoint:
      $type: websocket
      url: wss://example.com/SECRET_PATH/tcp
    cipher: chacha20-ietf-poly1305
    secret: SS_SECRET
  udp:
    $type: shadowsocks
    endpoint:
      $type: websocket
      url: wss://example.com/SECRET_PATH/udp
    cipher: chacha20-ietf-poly1305
    secret: SS_SECRET
```

Each TCP connection and each UDP flow gets its own WebSocket: TCP as a byte stream in binary
messages, UDP as one datagram per message, as outline-ss-server expects. The URL's host is resolved
like a plain server (once, not per flow); the connection goes to that address, with the URL's host
as TLS server name and HTTP `Host`, and the certificate is checked against the system roots (the
helper process sees `SSL_CERT_FILE` and `SSL_CERT_DIR` if the daemon's environment sets them; such a
file must be readable by the helper user, `lotsman-proxy`). The URL, whose path is a secret, is never logged or quoted;
`config check` and the log show `shadowsocks <cipher> over WebSocket` and the server as `host:port`.

What is accepted is what the Outline client accepts for these keys: `transport` as an `ss://`
string, a `$type: tcpudp` pair, or a Shadowsocks object without `$type` (used for TCP and UDP
alike); for `tcp` and `udp`, an `ss://` string or `$type: shadowsocks` with `endpoint`, `cipher`,
`secret` and optional `prefix` (applied to UDP salts too when given in `udp`, as Outline does) or the
legacy `server`/`server_port`/`method`/`password` fields; an `endpoint` is `host:port`,
`{$type: dial, address: host:port}`, or `{$type: websocket, url: wss://…}` (`ws://` works too;
`https://`/`http://` are read as `wss://`/`ws://`) with an optional `endpoint: host:port` to
connect to instead of the URL's host. Any other `$type` (such as `first-supported`, `iptable`,
`tls`, a dialer chained behind another) is refused when loading, naming the type. TCP and UDP must
reach the same host (ports may differ), since an upstream has one server address. A `tcpudp` key
without `udp` (or with `udp: {$type: direct}`) is loaded, but Lotsman never sends UDP around the
server as the Outline client would: that upstream has no UDP, so its DNS health check fails and it
stays down. A `reporter` section is ignored.

**Dynamic keys.** A provider that rotates keys hands out an `ssconf://host/path` URL instead (an
`https://` URL works too; `ssconf://` means `https://`). Put the URL in the key file the same way.
The URL is a credential: whoever has it gets the key, so it gets the same treatment as a key: it
is never logged or quoted. Lotsman fetches the key over HTTPS from the host's own network (not
through an upstream), with standard certificate checks against the system roots, following
redirects only to `https://`, each fetch bounded by half the health interval, at least 5 s. The response may be an `ss://` key, Outline's JSON (`server`, `server_port`,
`method`, `password`, optional `prefix`) or an Outline `transport:` config as described under
WebSocket keys; an `{"error": {"message": …}}` answer is a failed fetch whose message is logged.
The key is fetched:

- at start and on every reload, even if the file did not change, so `systemctl reload lotsman` picks
  up a rotated key;
- when the upstream fails its probes, at most once per 5 minutes per upstream.

A changed key applies to the running upstream at once, for new flows; open ones stay on the old
key (`upstream key changed` in the log). A failed fetch keeps the running key and is logged as a
warning. Every fetched key is also saved in `state_dir/ssconf/<name>.json` (0600, written
atomically, the URL itself not stored), so a restart works while the URL is unreachable: the key
cached for the same URL is used, with a warning. With neither a fetch nor a cached key, the daemon
still starts; that upstream stays down (`upstream is not running, will retry every health round`)
and is fetched again every health round until it comes up. `config check` fetches the key too, and
reports a failure as a warning, not an error, so `ExecStartPre` does not block the start.

How it works: the kernel routes the client's packets into `lm-up-<name>` as usual, and a userspace
TCP/IP stack terminates each TCP connection and UDP flow there and opens a matching one through the
Shadowsocks server. The stack runs in a helper process per Outline upstream (see
[Helper processes](#helper-processes)). Consequences:

- No ICMP: ping and traceroute through an Outline upstream get no answer.
- The client's TCP handshake completes only once the server accepted the connection; if the server
  is unreachable, the client gets a reset after up to 10 s. A connection the server breaks off is
  reset towards the client too.
- UDP needs the server's UDP relay. A UDP flow ends after 1 minute without packets, DNS (port 53)
  after 10 seconds.
- With a WebSocket key, every TCP connection and UDP flow first opens its WebSocket (TCP, TLS and an
  HTTP upgrade, at most 10 s), so it starts a few round trips later, and the reported health latency
  includes that.
- Every Outline upstream's interface has the address `192.0.0.8/32` (the IPv4 dummy address,
  RFC 7600); health probes and masqueraded client flows leave from it. The client `subnet` must not
  contain it. Its MTU is 1500 and adds nothing to the client MTU.
- `max_flows` caps TCP and UDP flows the upstream carries at once (default 4096). Beyond it, new TCP
  connections are reset and UDP packets that would start a flow are dropped; both are counted in
  `lotsman_upstream_flows_refused_total`. A reload changes it without touching open flows.
- CPU: TCP termination and Shadowsocks encryption run in the helper. In a container one flow moved
  about 2 Gbit/s; the stack adds roughly 1.5–4 ns of CPU per byte over Shadowsocks itself.

Health: a TCP handshake proves nothing here (the stack completes it, and an Outline server keeps a
connection with a wrong key open without answering), so an Outline upstream is up only when a DNS
query to a `health.dns` resolver gets an answer both over UDP and over TCP through it. Either
failing counts as a failed round. The reported latency is the UDP query's. `health.probe` is not
used for Outline upstreams.

### VLESS upstreams

A VLESS server with REALITY, with XTLS Vision over TCP or without a flow over gRPC, the Xray setups
that panels such as 3x-ui and Marzban create, is an upstream like an Outline one: the same userspace stack in a helper process, the same
health checks, `max_flows`, metrics and consequences as listed above. Put the server's share link,
one `vless://…` line, into a file and point `conf` at it:

```yaml
upstreams:
  - {name: vl-nl, kind: vless, conf: /etc/lotsman/upstreams/vl-nl.key, geo: NL, tags: [eu]}
```

```bash
sudo install -m 0600 /dev/stdin /etc/lotsman/upstreams/vl-nl.key <<< 'vless://…'
```

The link carries the user ID, a password in effect: keep the file 0600 and root-only. Lotsman never
logs it, and neither error messages nor `config check` quote any part of the link (`config check`
shows the flow or `grpc`, `sni` and fingerprint, never the gRPC `serviceName` or `authority`). Links
are static only; for a new link, replace the file and reload, which restarts the upstream. A link
looks like one of

```
vless://<uuid>@<host>:<port>?security=reality&pbk=<key>&sni=<site>&flow=xtls-rprx-vision&type=tcp&fp=chrome&sid=<hex>&spx=%2F#name
vless://<uuid>@<host>:<port>?type=grpc&security=reality&encryption=none&sni=<site>&fp=firefox&pbk=<key>&sid=<hex>&alpn=h2&serviceName=<name>#name
```

| Part | Meaning |
|---|---|
| `uuid` | Required: the user ID, a UUID, or 1–30 characters that Xray maps to one. |
| `host:port` | Required: the server. A hostname is resolved like an AmneziaWG endpoint (IPv4, at start, on reload and when probes fail); the Xray instance itself never resolves names. |
| `security` | Required, `reality`. TLS, no security and the other Xray securities are refused. |
| `type` | `tcp` (the default) or `raw`, Xray's newer name for it, or `grpc`; in any case, as Xray reads it. `headerType`, if present, `none`. xhttp, WebSocket and the other transports are refused. |
| `encryption` | `none` or absent; VLESS encryption (`mlkem768x25519plus…`) is refused. |
| `flow` | For `tcp` required: `xtls-rprx-vision` or `xtls-rprx-vision-udp443` (see QUIC below). For `grpc` empty or absent: Vision runs over TCP only. |
| `serviceName` | `grpc` only: the server's gRPC service name (its `grpcSettings.serviceName`), or Xray's custom path `/<service>/<stream>`; empty is allowed, as in Xray. |
| `mode` | `grpc` only: `gun` (the default) or `multi`, Xray's `multiMode`, which must match the server's choice. `guna` and other modes are refused. |
| `authority` | `grpc` only: the HTTP/2 `:authority`; by default the server's address, as in Xray. |
| `alpn` | Accepted and ignored, as Xray does under REALITY: the ClientHello offers the ALPN of the fingerprint (`fp`). |
| `sni` | Required: the site the server impersonates. |
| `pbk` | Required: the server's REALITY public key, 32 bytes in unpadded base64url. |
| `sid` | The REALITY short ID: up to 16 hex digits, an even number of them; empty by default. |
| `fp` | The TLS fingerprint (uTLS ClientHello): `chrome` by default; `firefox`, `safari`, `ios`, `android`, `edge`, `360`, `qq`, `random`, `randomized` and the others Xray knows. `unsafe` and `hellogolang` are refused, as Xray does. |
| `spx` | The path REALITY's crawler starts at (below); must start with `/`, default `/`. |
| `pqv` | Optional: the server's ML-DSA-65 key (unpadded base64url, 1952 bytes) for post-quantum verification of its certificate. |

The `#name` part is ignored. Any other parameter must be absent or empty (panels export empty
`host=` and `path=`); one with a value is refused by name. Lotsman checks and normalizes the
REALITY and gRPC fields exactly as Xray's own config loader does, so a link Xray rejects is rejected
here.

How it differs from an Outline upstream:

- The flows go through Xray (embedded, see [ARCHITECTURE.md](ARCHITECTURE.md)): one instance per
  upstream in its helper, with this server as its only outbound.
- The client's TCP handshake completes before the server is reached. A server that refuses
  connections shows up as a reset after about 2 s, while Xray retries; one whose packets are
  dropped, after Xray's connection attempts time out.
- No half-close: when a client shuts down its sending side, the target sees nothing until the
  connection ends. The rare protocol that waits for the client's end of stream before answering
  hangs.
- UDP travels inside a TCP connection to the server (XUDP), one per UDP flow: blocking the server's
  TCP port blocks UDP too, and the server needs no UDP port. Over gRPC each UDP flow is a stream
  instead, carried as plain VLESS UDP to ports 53 and 443 and as XUDP to the others, as Xray does
  without Vision.
- Over gRPC every flow of the upstream is a stream in one HTTP/2 connection to the server; a reload
  or a moved server opens a new one. Xray's client defaults apply: no keepalive pings and Chrome's
  user agent. A connection that stalls without being reset stalls every flow until the probes take
  the upstream down. Each Xray instance's connection outlives it until gRPC closes it after 30
  minutes without streams.
- QUIC over TCP: `xtls-rprx-vision` refuses UDP to port 443, as every Xray client does. Browsers then fall
  back to HTTPS over TCP, whose TLS Vision passes through without the telltale TLS-in-TLS record
  pattern. `xtls-rprx-vision-udp443` (client side only; the server's flow stays
  `xtls-rprx-vision`) carries QUIC as UDP inside the connection instead, where Vision cannot
  disguise it. Prefer the plain flow unless QUIC through this upstream matters.
- A TCP flow without traffic for an hour ends, as does one whose other direction ended an hour
  earlier.
- When the server presents a real certificate instead of REALITY's (a wrong `pbk` or `sid`, which
  makes the server relay the client to its `sni` site, an address hijack, or interception with a
  certificate the host trusts), the flow fails and Xray logs `REALITY: received
  real certificate (potential MITM or redirection)` as an error. It then fetches a page of the
  `sni` site, starting at `spx`, as a browser would, to look less like a proxy client. The upstream
  goes down by its probes.

Xray's log appears in the daemon's with `component=xray` and the `upstream` attribute. Its per-flow
messages, failed flows included, are debug messages; its errors and warnings keep their level once
per minute per kind of message and are debug messages after that, so a failure repeated for every
flow cannot flood the log. Each helper logs Xray's start (`core: Xray 26.11.9 started`) as a
warning.

**Updating Xray.** Lotsman pins `github.com/xtls/xray-core` by a pseudo-version: Xray tags its
releases `v26.x.y`, which Go does not accept as versions of a module whose path has no `/v26`, so
`go get -u`, `@latest` (stuck at `v1.260327.0`) and Dependabot never see a new release. Check the
[releases](https://github.com/XTLS/Xray-core/releases) when preparing a Lotsman release and on
Xray security advisories, and pin the release tag's commit:

```bash
git ls-remote https://github.com/XTLS/Xray-core refs/tags/v26.11.9   # the tag's commit
go get github.com/xtls/xray-core@676eaf66f8f0                         # becomes v1.260327.1-0.20261109052709-676eaf66f8f0
go mod tidy
```

Then compare Xray's `infra/conf/transport_security.go` (`REALITYConfig.Build`, the client branch),
`infra/conf/transport_method.go` (`GRPCConfig.Build`), `infra/conf/transport_internet.go`
(`StreamConfig.Build`, the transports REALITY allows) and `infra/conf/vless.go` with
`internal/proxy/vless.go`, which replicates their checks; watch for
a gVisor bump, which amneziawg-go and the proxy stack share; and run the full test suite including
the Linux one (`just test`, `just lint`, `just test-integration`). Update the version named in this
section and in ARCHITECTURE.md.

### Helper processes

Each proxy upstream's (Outline or VLESS) stack runs in a child process of the daemon, so a crash in
it (a bug in gVisor, the Outline SDK or Xray, or malicious input) takes down that upstream, not the
gateway:

```
$ ps -o pid,ppid,user,args --ppid "$(systemctl show -p MainPID --value lotsman)"
    PID    PPID USER     COMMAND
   4242    4200 65534    lotsman proxy-helper
```

A helper is the daemon's binary re-executed from `/proc/self/exe`, so its process name (`comm`) is
`exe`: `ps -C lotsman` misses it, and `pidof lotsman` can return a helper's pid next to the
daemon's. Take the daemon's pid from systemd as above.

There is one per proxy upstream; the daemon logs `proxy helper started upstream=<name> pid=<pid>`
for each. A root daemon runs them as the system user `lotsman-proxy` (uid and primary gid, no
supplementary groups) without capabilities: they hold only the upstream's TUN descriptor, open
outbound connections to the server and are not dumpable, so other processes of that user cannot read
the key from their memory. The unit's `CapabilityBoundingSet`
must include `CAP_SETUID` and `CAP_SETGID` for this (as `deploy/lotsman.service` does); without
them every proxy upstream fails to start with `operation not permitted`. In Docker, the default
capabilities include both. Helpers ignore SIGTERM and SIGINT and die with the daemon. Their logs
appear in the daemon's log with the `upstream` attribute, at the daemon's log level.

The daemon looks the user up each time it starts a helper, so creating it takes effect at the next
helper start, with no daemon restart; check with `ps` as above (the USER column) or
`getent passwd lotsman-proxy`. If the user does not exist, the daemon logs `proxy helpers run as
uid/gid 65534 (nobody)` once and falls back to that. Other services often run as `nobody` too: they
cannot read a helper's memory, but they can kill it (handled as a crash, below), which is why the
dedicated user is the recommended setup. A `lotsman-proxy` that resolves to uid or gid 0 is treated
as missing. A file named by `SSL_CERT_FILE` (WebSocket keys) must be readable by that user.

When a helper exits on its own, the log shows `proxy helper exited upstream=<name> status=…` (with
`panic=…` and the stack trace lines as `proxy helper output` when it panicked), the upstream is down
at once, its clients move to the next healthy upstream (or are blocked, fail-closed), and
`lotsman_proxy_restarts_total{upstream}` increases. The daemon starts a new helper right away; if it
exited within a minute of starting, the next start waits 1 s, doubling up to 1 minute
(`restart held back` in the log). Once running, the upstream returns after `up_after` good probes.
A helper that stops answering the daemon for 2 s is killed and handled the same way, and one whose
stack stopped because reading its TUN failed exits with status 1 (`read tun, the upstream stops`
just before) and is restarted too.

## Profiles

A profile is what a client's traffic prefers: an ordered list of tiers. The first tier with a
healthy upstream wins, and within a tier the lowest latency, so a tag such as `near` groups several
countries into one tier. Profiles live in the database and change without a restart:

```bash
lotsman profile add nl geo=NL                       # one tier
lotsman profile add eu tag=near tag=eu any          # near, else eu, else anything
lotsman profile add de geo=DE,provider=acme geo=DE  # an upstream must match every key of a tier
lotsman profile add auto                            # no tiers: any upstream
lotsman profile set eu tag=eu                       # replaces every tier
lotsman profile list
lotsman profile rm de                               # refused while a peer uses it
```

Each argument after the name is one tier, best first, so no quoting is needed: `key=value` pairs
joined by commas, with keys `geo`, `provider` and `tag`, or `any` for every upstream. `profile list`
prints tiers in the same form. Names are lowercase letters, digits and `-`, up to 32 characters.

`profile add` and `set` check the profile against the upstreams in `lotsman.yaml`: every tier must
match at least one upstream, so a typo in a geo or provider is refused, and a `geo` or `provider`
value is at most 64 characters without spaces, commas or `=`. A tier that only matches upstreams an
earlier tier already took is stored with a warning (also in `config check` and the start and reload
logs). A stored tier that matches nothing after an upstream is removed also only warns, so an
upstream can be removed without editing every profile; only a profile left with no upstream at all
fails the check. The daemon checks the stored profiles again at start and on every reload
against the config it applies. A change applies at once when the daemon is running: like `user
add`, the command sends SIGUSR1 and the daemon reroutes the clients of that profile on the spot,
keeping their open connections pinned to an upstream that is still alive. `user add -profile`
must name a stored profile.

### Moving profiles out of lotsman.yaml

Earlier versions kept profiles under `profiles:` in `lotsman.yaml`. After upgrading, `config
check` names the command that copies them into the database; run it once, then delete the key:

```bash
sudo lotsman -config /etc/lotsman/lotsman.yaml profile import /etc/lotsman/lotsman.yaml
sudoedit /etc/lotsman/lotsman.yaml   # delete profiles:
sudo lotsman config check
```

`profile import FILE` reads the `profiles:` key of FILE, validates every profile before storing
any, and skips profiles already in the database, so running it again is harmless. Until the
profiles are imported, existing clients are blocked.

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

`user rm` removes the wider set only when `-profile` is left out; an explicitly empty `-profile ""`
(for instance an unset shell variable) is rejected. `user add` validates the profile and the
upstream configs before it writes anything, and creates the user together with the first config in
one step, so a rejected command leaves nothing behind. If it cannot write the config to stdout
after saving it, it says so; print it again with `user show`.

WireGuard binds one key to one endpoint, so a config must never be active on two devices at the
same time — give each device its own with `-device`. A device has one config per profile; which
config is active decides where the traffic exits. The config goes to stdout (redirect it to a
file), the QR code to stderr. Adding or removing takes effect immediately when the daemon is
running: the CLI sends it SIGUSR1, which reconciles users and profiles from the database but does
not re-read `lotsman.yaml` (use `systemctl reload lotsman` for that). Before signalling, the CLI checks that the
pid in `lotsman.pid` belongs to this same `lotsman` executable; otherwise it signals nothing, says
why on stderr, and the change applies on the next reconcile or start. After replacing the binary,
restart the daemon. Run these commands as the daemon's user (normally root).

### User links

A user's link opens their own page in the [web panel](#web-panel), `<public_url>/u#<token>`.
There they see their devices and the exits their profiles use, with their availability, download
each config (text and QR code), add or remove their own devices and switch a device's profile (add
the new profile, then remove the old one).

```bash
lotsman user link alice                  # print a new link; her previous one stops working
lotsman user link alice -url https://vpn.example.com
lotsman user link alice -revoke          # end her link; she keeps her configs
```

The link is printed once, to stdout. Whoever has it can download the user's configs, so treat it
like a private key: send it over a channel you trust, and if it leaks, run `user link` again
(rotation) or `-revoke`. Only a SHA-256 of the 256-bit token is stored, so a lost link cannot be
shown again, only replaced. The address comes from `-url`, else from `public_url` in the
[panel's config](#panel-configuration) (`$LOTSMAN_WEB_CONFIG` or `/etc/lotsman/web.yaml`); with
neither, the command prints only the token and says how the address is built. `user rm` of the
whole user also ends their link. The panel trusts a resolved link for 30 seconds, so a rotated or
revoked link stops working within 30 seconds, and a live view it opened within a minute.

Users are limited on their page, not in the CLI:

- `users.max_devices` in `lotsman.yaml` (default 5) caps the devices a user can have when adding
  one; another profile on an existing device does not count, so profile switches keep working. `0`
  turns adding off, another profile included (`permission_denied`); users can still remove
  devices. Reload with `systemctl reload lotsman`.
- At most 10 additions at once, then one every 6 minutes, per user (`resource_exhausted` past it).
  Both limits answer `resource_exhausted` with a `SelfServiceLimit` detail that names the limit and
  carries `max_devices` or `retry_after`.
- A client address (an IPv6 `/64`) whose links fail to resolve 10 times is refused for a while
  (one more try every 6 seconds), so the panel does not pass guesses on to the daemon.

The token is never logged, by the daemon or the panel. It is in the link's fragment, after `#`,
which browsers never send, so your reverse proxy's access log sees only `/u`; the page sends the
token to the panel in a header, which proxies do not log by default. A link cut off before the
`#` part, or with a garbled token, shows "link missing or invalid" instead of the page.

## Watching it

```bash
lotsman upstream status
journalctl -u lotsman -f
ip rule show            # one "from <client>/32 lookup 100x" per routed client
nft list table inet lotsman
```

Health transitions and every routing decision are logged. `status.json` in the state directory is
what `upstream status` and the live columns of `user list` read; the daemon writes it right after
its first reconcile and then after every health round, so a reload that changes `health.interval`
changes the refresh rate at once.

The daemon logs at `info` by default. For more detail, pass `-log-level debug` before `serve` or set
`LOTSMAN_LOG_LEVEL=debug` (`debug`, `info`, `warn`, `error`; the flag wins). `debug` also shows the
AmneziaWG device logs, including handshake diagnostics. In the systemd unit, add
`Environment=LOTSMAN_LOG_LEVEL=debug` and restart; an invalid value stops `lotsman` before it starts.

`STATE` is `up`, `down`, `unknown` (not probed to a verdict yet) or `degraded` (reachable, but
failing its [transfer probes](#throttled-upstreams-transfer-probe)). `KIND` is `awg`, `outline` or `vless`. `LAST HANDSHAKE` is the upstream device's handshake with its provider;
a proxy upstream has none and shows `-`. `ROUTED` counts clients whose traffic is directed through the upstream — every client in the
database is routed somewhere whether or not it is connected. `ACTIVE` counts those with a
WireGuard handshake in the last three minutes, which is what "connected" means for WireGuard;
`user list` shows each client's last handshake and current upstream (`blocked` when no healthy
upstream matches its profile, `-` when the daemon is not running).

### Control socket

The daemon serves its control API (`lotsman.v1` over Connect, defined in `api/lotsman/v1/`) on
`/run/lotsman/control.sock` for the web panel. Anyone who can connect to it controls the gateway:
the daemon trusts the socket's peer and checks no credentials, so the file mode is the boundary.
It is `0660 root:lotsman-web`; add only the panel's user to that group.

```bash
ls -l /run/lotsman/control.sock     # srw-rw---- 1 root lotsman-web ...
```

The socket appears when the daemon starts and is removed when it stops; one left by a crash is
replaced at the next start. `serve` fails with `control socket … another process serves it` when a
running daemon (in another network namespace) already serves that path. Without the `lotsman-web`
group the socket is `0600 root:root` and the daemon logs `control socket is root-only`; create the
group and restart. The unit provides `/run/lotsman` (`RuntimeDirectory=lotsman`) and needs
`CAP_CHOWN` in its `CapabilityBoundingSet` to hand the socket to the group, as
`deploy/lotsman.service` does. Answering so far:

- `StatusService.Watch`: the gateway's status and traffic, at once and then every second or
  `interval`.
- `ProfileService` (`List`, `Put`, `Delete`): the [profiles](#profiles) of `lotsman profile`, with
  the same checks and warnings. `Put` creates or replaces one and reroutes its clients before it
  answers. `List` also returns the running upstreams' labels (`geo`, `provider`, `tags`), which the
  panel suggests in its tier fields. Errors: `invalid_argument` for a profile the checks refuse (no
  name, a tier that matches no upstream), `failed_precondition` for deleting one that peers use, `not_found` for one that does
  not exist.
- `ConnectionService.Watch`: [live connections](#live-connections), for one `peer_id` or every
  peer. Errors: `not_found` for an unknown `peer_id`, `unavailable` when the kernel's table cannot
  be read.
- `HistoryService` (`Upstream`, `Peer`): the [history](#history) of an upstream or a `peer_id`.
  Errors: `invalid_argument` for a bad range or step, `not_found` for an unknown name.
- `UserService` and `UserSelfService`: users, devices, configs and [links](#user-links), and a
  user's own view, history included.

Every other RPC returns `unimplemented`. Self-service RPCs act for the user named in the `Lotsman-User` request header,
which only the [web panel](#web-panel) sets.

## Web panel

`lotsman web` is the web panel: it serves the browser UI and forwards the UI's calls to the
daemon over the [control socket](#control-socket). It runs as the unprivileged user
`lotsman-web`, whose primary group, `lotsman-web`, may use the socket. It never opens
`lotsman.db`; its own state, `web.db` (operator passkeys, sessions, enrollment tokens), lives in
`/var/lib/lotsman-web`. It listens on loopback and needs a reverse proxy in front for TLS.

Operators sign in with passkeys (see [Operators and passkeys](#operators-and-passkeys)). Users
reach their page through their [link](#user-links). A binary built without the frontend
(`go build`, `just build` without `just web-build`) serves a page saying so, and the panel logs a
warning at start.

The operator console is under `/admin`. **Upstreams** lists each upstream's state, latency, live
rate and 7-day availability; an upstream's own page adds the peers routed through it, the last 5
minutes' rate and its history over 1 hour to 90 days (availability, average rate, latency).
**Profiles** creates, edits and deletes profiles, with the daemon's warnings and the matching
`lotsman profile add` or `set` command; the tier fields suggest the upstreams' geo, provider and tag
values that fit the rest of the tier, and a tier that matches no upstream is flagged before saving; a profile that devices use cannot be deleted. **Users** lists users
with their devices and profiles: add a device (which creates its user), remove devices or whole
users, show a device's config as a QR code, as text or as a `.conf` download, and issue or revoke a
user's [link](#user-links). A link is shown once, when it is issued: only its hash is kept, so to
see it again, issue a new one, which ends the old one. **Connections** shows the open connections
of one peer or of all peers, live from conntrack: nothing is stored, destinations are `IP:port`
without reverse DNS, and at most 500 are sent at once (the fastest). **Passkeys** is described
below.

Install, after the gateway itself:

```bash
getent passwd lotsman-web >/dev/null || sudo useradd --system --gid lotsman-web --no-create-home \
  --shell /usr/sbin/nologin lotsman-web
sudo chgrp lotsman-web /etc/lotsman && sudo chmod 0750 /etc/lotsman
sudo chmod 0600 /etc/lotsman/lotsman.yaml && sudo chmod 0700 /etc/lotsman/upstreams  # older installs
sudo install -m 0640 -g lotsman-web deploy/web.example.yaml /etc/lotsman/web.yaml
sudoedit /etc/lotsman/web.yaml               # public_url at least
sudo cp deploy/lotsman-web.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now lotsman-web
```

The panel must be able to read `web.yaml`, so `/etc/lotsman` is `0750 root:lotsman-web` and
`web.yaml` `0640 root:lotsman-web`. With the directory `0700 root` the panel cannot read its
config and fails at start. The group must not read anything else there: the install above makes
`lotsman.yaml` `0600` and `upstreams/` `0700`, and the second line fixes an older install that
copied `lotsman.yaml` with `cp` (`0644`).

Record `user=lotsman-web` and the unit and state paths in the [manifest](#removing-it). If the
daemon was already running when the group was created, restart it once so the socket gets the
group.

If the panel fails to start several times in a row (a config it cannot read, a bad key), systemd
stops retrying: the unit allows 5 starts within 300 seconds (`StartLimitBurst=5`,
`StartLimitIntervalSec=300` in `deploy/lotsman-web.service`). Read the cause with `journalctl -u
lotsman-web`, fix it, then clear the counter before starting again:

```bash
sudo systemctl reset-failed lotsman-web && sudo systemctl start lotsman-web
```

Then set up the [reverse proxy](#reverse-proxy), enroll the first operator's passkey, as below,
and run through [Checking it](#checking-it).

### Panel configuration

`/etc/lotsman/web.yaml`, separate from `lotsman.yaml` because that file is root's alone and the
panel must not read the daemon's config. Keep it `0640 root:lotsman-web`. `lotsman web` reads
`-config` (given before `web`), else `$LOTSMAN_WEB_CONFIG`, else `/etc/lotsman/web.yaml`; see
`deploy/web.example.yaml`.

| Key | Meaning |
|---|---|
| `listen` | Numeric TCP address, default `127.0.0.1:8080`. `0.0.0.0` and `[::]` are refused: only the reverse proxy should reach the panel. |
| `public_url` | Required. The address browsers use, `https://host[:port]` with no path (plain `http` only for a loopback host). The host must be a name, not an IP address: it is the passkeys' RP ID. Every API and sign-in call must carry exactly this `Origin`; the `Host` header plays no part. Passkeys are bound to its host, so changing the host means enrolling every passkey again (see below). |
| `trusted_proxies` | Addresses or prefixes whose `X-Forwarded-For` the panel believes, such as `[127.0.0.1, "::1"]` for a proxy on the same host. Default none: the client address is the peer's own. Used for rate limits and logs. |
| `socket` | The daemon's control socket, default `/run/lotsman/control.sock`. |
| `state_dir` | Where `web.db` lives, default `/var/lib/lotsman-web` (the unit's `StateDirectory=`). |

Changes need `systemctl restart lotsman-web`. A bad config stops the panel at start, with the
key in the journal. The log level follows `-log-level` or `$LOTSMAN_LOG_LEVEL`, as for `serve`.

### Operators and passkeys

Operators sign in with a passkey (WebAuthn): one kept by the device (Touch ID, Windows Hello, an
Android phone), a password manager or a security key. There are no passwords and no roles: every
passkey is an operator with full control. Each passkey needs a PIN or biometric check on every
use.

The first operator, or anyone locked out, gets in from the server:

```bash
sudo lotsman web enroll
```

It prints a one-time link, such as `https://panel.example.com/enroll#…`, valid for 15 minutes. Open
it in the browser that will keep the passkey, name the passkey (`laptop`), and follow the
browser's prompt; the browser is then signed in. The command writes `web.db` directly, so the
panel need not be running; run it as root or as `lotsman-web` (`sudo -u lotsman-web lotsman web
enroll`), and it hands any file it creates to the state directory's owner. Treat the link like a
password until it is used: whoever opens it first becomes an operator. The token after `#` never
reaches the server's or the proxy's logs, and `web.db` keeps only its hash.

In the console, **Passkeys** lists every passkey with when it was created and last used. **Issue
enrollment link** makes another one-time link for a new operator or another device; send it over a
channel you trust. **Revoke** deletes a passkey and ends its sessions at once; revoking the last
one locks the console until `lotsman web enroll`. Deleting a passkey in the panel does not remove
it from the device or password manager; remove it there too.

Sessions are a `__Host-lotsman-session` cookie (`HttpOnly`, `Secure`, `SameSite=Strict`). A session
ends after an hour without a request, after 12 hours in any case, on **Sign out**, and when its
passkey is revoked. An open live view is not cut when its session ends; the next call is refused
and the page goes to the sign-in page. Sign-in and enrollment attempts are limited per client
address (from `trusted_proxies`, see above; an IPv6 /64 counts as one): ten at once, then one per
30 seconds; past that the panel answers `429`.

**Changing `public_url`'s host** invalidates every passkey: browsers bind a passkey to the host
name (the RP ID) and will not offer it to another. Enroll again with `lotsman web enroll` after the
change and revoke the old entries. A change of port or of the proxy alone keeps them.

The same change moves every [user link](#user-links): the tokens stay valid, because only their
hashes are stored and the host is no part of them, but the links already sent say
`https://old-host/u#…`, which the new setup does not serve. Issue each user's link again
(`lotsman user link NAME`) and send it; that also ends the old one. Enrollment links are
one-time and short-lived, so print new ones after the change.

**Back up `web.db`** (in `state_dir`, `/var/lib/lotsman-web`; it is the panel's own state, the
daemon's is `lotsman.db`) along with `lotsman.db`. It holds the passkeys' public keys and the
sessions; it holds no private key, so a copy cannot sign anyone in, but losing it means
enrolling every operator again with `lotsman web enroll`. Keep it `0600 lotsman-web`.

The panel opens `web.db` in SQLite's WAL mode, so while it runs, recent changes live in
`web.db-wal` and copying `web.db` alone can lose them or give an inconsistent file. Either take
the copy with SQLite, which is safe while the panel runs (needs the `sqlite3` command line tool;
run it as `lotsman-web` so no root-owned `-wal` or `-shm` file is left behind):

```bash
sudo -u lotsman-web sqlite3 /var/lib/lotsman-web/web.db ".backup /var/lib/lotsman-web/web.db.bak"
```

then move `web.db.bak` out of the state directory, or stop the panel first and copy the file
with any `-wal` and `-shm` beside it:

```bash
sudo systemctl stop lotsman-web
sudo cp -p /var/lib/lotsman-web/web.db* /root/backup/    # a directory only root can read
sudo systemctl start lotsman-web
```

### Reverse proxy

The proxy terminates TLS for `public_url` and passes everything to `listen`. Live views are
server-streaming calls that stay open for as long as the page is, so the proxy must pass each
response chunk on as it arrives (no response buffering) and must not time out an open response
after a minute or two. It should set `X-Forwarded-For` and keep the browser's `Origin`.

Caddy gets and renews the certificate itself, provided `panel.example.com` resolves to this host
and ports 80 and 443 reach it. It sets `X-Forwarded-For` itself. `flush_interval -1` turns response
buffering off; without it the live views stall, because Caddy may hold back the chunks of a
streaming response:

```
panel.example.com {
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1
	}
}
```

If your distribution's `caddy` package is old or missing, Debian and Ubuntu hosts can use Caddy's
own apt repository (see Caddy's installation page for the current commands); it adds
`/etc/apt/sources.list.d/caddy-stable.list` and
`/usr/share/keyrings/caddy-stable-archive-keyring.gpg` and installs the `caddy` package, which
runs as its own service from `/etc/caddy/Caddyfile`. Record all three in the
[manifest](#removing-it) if you installed Caddy for the panel.

nginx, not tested on the test gateway (the directives are the standard ones for streaming
responses; the panel speaks HTTP/1.1, so the upstream needs nothing more):

```nginx
server {
    listen 443 ssl;
    server_name panel.example.com;
    # ssl_certificate ... as usual

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

`proxy_http_version 1.1` with an empty `Connection` keeps the upstream connection a plain
HTTP/1.1 one, `proxy_buffering off` passes each chunk on at once, and `proxy_read_timeout 1h`
keeps a quiet stream from being cut at nginx's default 60 seconds. The panel does not use
`X-Forwarded-Proto` (its origin comes from `public_url`); the header does no harm.

List the proxy's address in `trusted_proxies`: `[127.0.0.1, "::1"]` for a proxy on the same host
(either of the above), its own address or prefix otherwise. The panel believes
`X-Forwarded-For` only from those peers. If the proxy is not listed, every request counts as
coming from the proxy: the sign-in and link limits are then shared by all clients, so one client's
failed attempts throttle the others, and the logs show the proxy's address. List only proxies you
run; a client in a listed prefix can name any address it likes.

Add HSTS at the proxy if you want it; the panel sets its own security headers on every response (a
`default-src 'self'` CSP with nothing inline, `frame-ancestors 'none'`, `Referrer-Policy:
no-referrer`, `nosniff`) and `no-store` on API responses.

### Checking it

After the install, the proxy and the first enrollment:

```bash
systemctl is-active lotsman-web                                      # active
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/      # 200
journalctl -u lotsman-web -n 20 --no-pager                           # no "daemon unreachable"
```

- The console opens at `https://panel.example.com/admin` once you are signed in (a signed-out
  browser is sent to `/login`).
- A live view updates: on **Upstreams**, the rate and latency change every second or so. If the
  page loads but the numbers stay still, the proxy is buffering the stream; check
  `flush_interval -1` or `proxy_buffering off`.
- A user's link, from `lotsman user link NAME`, opens that user's page.

### What the panel forwards

The browser calls the same `lotsman.v1` API as the socket, under `/api/` (for example
`/api/lotsman.v1.StatusService/Watch`). Each RPC has a scope in the panel: operator (a signed-in
operator), user (a user's link), or internal (`UserSelfService.ResolveLink`, which only the panel
calls); an RPC without a scope is refused with `permission_denied`. Only POST is accepted, a
request body is at most 64 KiB, and only the RPC protocol headers reach the daemon: cookies,
credentials, forwarded addresses and every `Lotsman-*` header the browser sent are dropped. A
user's page sends its link's token as `Authorization: Bearer <token>`; the panel resolves it
through the daemon and, for self-service calls, sets `Lotsman-User` itself. The `Authorization`
header never reaches the daemon. When the daemon is down, or the panel's
user is not in the `lotsman-web` group, calls answer `unavailable` and the panel logs
`daemon unreachable` with the reason. The panel's own sign-in endpoints, under `/auth/`, are JSON
over POST with the same `Origin` check, and never reach the daemon.

## Monitoring

```yaml
metrics:
  listen: 10.77.0.1:9100     # or 127.0.0.1:9100 for a scraper on the host
```

`GET /metrics` serves the Prometheus text format. The endpoint is off unless `metrics.listen` is
set, and an unspecified address (`0.0.0.0`, `::`) is refused: the per-client series carry user,
device and profile names. On the gateway address every connected client can read it (Lotsman's
firewall lets clients reach only that address and port on the host); on loopback only a scraper on
the host can. Changing `metrics.listen` needs a restart.

| Metric | Labels | Meaning |
|---|---|---|
| `lotsman_upstream_info` | `upstream`, `kind`, `interface` | always 1; `kind` is `awg`, `outline` or `vless` |
| `lotsman_upstream_up` | `upstream` | 1 while probes pass |
| `lotsman_upstream_flows` | `upstream` | Outline and VLESS only: TCP and UDP flows open at the last health round |
| `lotsman_upstream_flows_refused_total` | `upstream` | Outline and VLESS only: flows refused at `max_flows` |
| `lotsman_proxy_restarts_total` | `upstream` | Outline and VLESS only: times its helper process exited on its own (crash, kill) and was restarted |
| `lotsman_upstream_probe_latency_seconds` | `upstream` | last successful probe |
| `lotsman_upstream_probes_total` | `upstream`, `result` | `ok` / `fail` |
| `lotsman_upstream_degraded` | `upstream` | 1 while it is reachable but fails its transfer probes; always 0 with `health.transfer` off |
| `lotsman_upstream_transfer_seconds` | `upstream` | duration of the last transfer probe, a failed one included (then about `timeout`) |
| `lotsman_upstream_transfer_probes_total` | `upstream`, `result` | transfer probes, `ok` / `fail` |
| `lotsman_upstream_last_handshake_timestamp_seconds` | `upstream` | AWG only; 0 if never |
| `lotsman_upstream_clients` | `upstream` | clients routed through it, connected or not |
| `lotsman_upstream_active_clients` | `upstream` | of those, with a handshake in the last 3 minutes |
| `lotsman_upstream_receive_bytes_total`, `…_transmit_bytes_total` | `upstream` | AWG only: provider tunnel counters |
| `lotsman_peer_receive_bytes_total`, `…_transmit_bytes_total` | `user`, `device`, `profile` | client tunnel counters |
| `lotsman_peer_last_handshake_timestamp_seconds` | `user`, `device`, `profile` | 0 if never |
| `lotsman_peer_upstream` | `user`, `device`, `profile`, `upstream` | 1 for the current upstream |
| `lotsman_reroutes_total` | `upstream` | routes to it, first assignment included |
| `lotsman_reloads_total` | `result` | `ok` / `error` |
| `lotsman_firewall_repairs_total` | | firewall reinstalls after another tool removed Lotsman's rules |
| `lotsman_client_mtu`, `lotsman_build_info{version}` | | |

Byte counters come from the WireGuard devices and reset when a peer is re-added; use `rate()`.
A Prometheus job:

```yaml
scrape_configs:
  - job_name: lotsman
    static_configs:
      - targets: ["10.77.0.1:9100"]
```

## History

The daemon records each upstream's availability and latency and each upstream's and peer's traffic
a minute at a time in `lotsman.db`, for the web panel's charts (`HistoryService`). Nothing to enable.

```yaml
history:
  minute_retention: 168h   # samples of a minute: 7 days (default; at least 1h)
  hour_retention: 2160h    # samples of an hour: 90 days (default; not shorter than the above)
```

Minute samples older than `minute_retention` are folded into hour samples (bytes added, availability
and latency averaged) and hour samples older than `hour_retention` are deleted; both run at start and
then every hour, and a reload with new values applies at the next run. The fold takes whole
hours, so minute samples reach back up to an hour further than `minute_retention`.
Shortening a retention drops history that no longer fits at the next run, without a way back
except the pre-migration backups; lengthening it does not bring back what was dropped.

- Traffic is bytes per period, down towards the client. An upstream's bytes are its `lm-up-<name>`
  interface's, the same counters `StatusService.Watch` reports; a peer's are its tunnel's, encrypted, with the
  AmneziaWG overhead.
- The availability of an upstream is the share of the minute it was up, read once a second. A
  degraded upstream (reachable, failing its transfer probes) counts as not up, as does one not yet
  probed; latency is the mean over the time it was up. This is not `lotsman_upstream_up`, which
  stays 1 while degraded.
- A peer has a sample for every minute the daemon ran, idle or not; a gap is a time the daemon
  was not running (a restart loses its last, unfinished minute). The current minute shows up in
  the charts after it ends.
- Removing a peer leaves its samples until they expire; they are no longer served. Removing an
  upstream from the config does the same. Peer ids are never reused, so a new peer never inherits
  a removed one's history.
- A query returns at most 2000 samples: it asks for a step, the daemon rounds it up to a
  multiple of one minute (ranges starting within `minute_retention`) or one hour (older), and
  further until the range fits.

**Disk.** 5 upstreams and 50 peers kept 7 days by the minute and 83 more by the hour take about
**15 MiB** (14.4 MiB measured; up to about 19 MiB including the write-ahead log) once the hour
samples are in, and the size is flat from then on: a peer costs about 0.24 MiB for its 90 days
(21 bytes × 10,080 minute rows plus 22 bytes × 1,992 hour rows) and an upstream about 0.33 MiB.
Scale linearly with `peers × (7 d × 1,440 + 83 d × 24)` rows for the defaults, which is the part that
`minute_retention` drives: a 30-day `minute_retention` is about four times the minute share
(roughly 50 MiB for the same gateway). The database lives in `state_dir`, so budget it there, and
note that the pre-migration backups (`lotsman.db.v<N>-…bak`) are copies of the whole file. The
first start of a version with history migrates the schema (to 5) and makes such a backup.
Writing costs one short transaction a minute; compaction an hour's worth of folding, about
0.1 s on a modern core (up to a second on a small VPS), once an hour.

## Live connections

The operator's live connections view shows the connections each client has open through the
gateway, with their throughput. Forwarding happens in the kernel, so the daemon reads them from the
kernel's connection tracking table (conntrack), in its own network namespace.

Byte counts need `net.netfilter.nf_conntrack_acct=1`. `serve` sets it once it is up, after its
firewall has loaded `nf_conntrack`; it does not reset it on exit, and
[uninstall](#removing-it) restores the value the manifest recorded. The setting is per network
namespace, and only connections opened after it was turned on are counted. If it cannot be set (no
`nf_conntrack`, a read-only `/proc/sys`), the daemon logs "live connections will show no traffic"
and the gateway works as before; the connections are listed with zero bytes.

Who sees what:

- Operators only. Every operator sees every client's connections; a user's own page never shows
  connections, not even their own.
- Per connection: the client (peer), protocol, the client's tunnel IP and port, the destination IP
  and port, the client's current upstream, when it started, and bytes and rate in each direction
  (down is towards the client). Destinations are numeric: no reverse DNS, no hostnames, no SNI, no
  payload. A client's DNS queries appear as UDP connections to its resolver.
- The upstream is the one the client's new connections take now. A connection that stayed on the
  previous upstream after a failover (see [How failover behaves](#how-failover-behaves)) shows the
  new one.
- The start time is the kernel's with `net.netfilter.nf_conntrack_timestamp=1`, which Lotsman does
  not set. Without it, a connection that opens while the view is open starts when it was first
  seen, and one that was already open has no start time.
- At most 500 connections per update, fastest first; the update says when more were left out.
- Updates come every second, or every `interval` of the watch if longer. Each viewer measures
  rates on their own, so the first update after opening the view has bytes but no rates.

Nothing is kept at rest. The table is read only while someone watches, on every update of that
view, and each reading is dropped once it is sent: connections never reach the database, the
status file, the metrics or the logs. The kernel's own table is short-lived and visible to root on
the host anyway (`conntrack -L`); Lotsman adds no record of it.

## How failover behaves

- An upstream is *down* after `down_after` consecutive failed probes and *up* again after `up_after`
  successes. A round starts `interval` after the previous one ends, and a probe that gets no
  answer waits its timeout (half the interval) first. Down therefore takes
  `(down_after − 1) × (interval + timeout) + timeout`, plus up to one `interval` until the next
  round: 53–68 s at the defaults (15 s, 3, 2) when the path drops packets, 30–45 s when it refuses
  connections at once. Up again takes up to `interval + timeout` until a round passes, plus
  `(up_after − 1) × interval`: 15–38 s at the defaults. With several `health.probe`
  targets a round tries all of them at once and passes if any answers, reporting the fastest, so
  one blocked or rate-limited destination does not take an upstream down. `probe` applies on reload.
- A client stays on its upstream while it is healthy, even if a sibling in the same tier becomes
  faster. It moves when its upstream goes down, or when an upstream in a *better* tier recovers.
- Moving changes the exit IP for *new* connections only. Connections a client already has open
  through an upstream that is still alive stay pinned to it (conntrack marks plus a per-client
  `fwmark` rule) until they close; only when the old upstream is dead do they fail. The tunnel to
  Lotsman itself never drops.
- No healthy upstream for a profile: nftables denies the client, then its routes and pins are
  removed. A client without an assignment is denied from the start, including destinations in an
  upstream's connected subnet. Nothing leaves through the server's own address.

On shutdown, Linux disables `lm0` before removing routes or firewall protection.
If disabling fails, those protections remain and the error is logged. The daemon
closes downstream and retries cleanup once before closing upstreams. The same
order applies after a failed start; repeated shutdown is safe. Cleanup errors
that persist are logged, and owned leftovers are reconciled at the next start.
Uninstall also disables surviving downstream ingress before removing protection,
even if stopping the service fails. If interface inspection or disabling fails,
it stops while preserving the firewall and routing state.

### Throttled upstreams (transfer probe)

Reachability probes catch an upstream that is cut off. They miss one that is throttled or whose
TCP flows freeze after the first 15–20 KB, a common way of blocking traffic to foreign hosting:
the handshake and the small DNS answer pass, real traffic does not. The transfer probe, off unless
configured, downloads through each upstream every few minutes:

```yaml
health:
  transfer:
    url: https://speed.cloudflare.com/__down?bytes=65536   # or a list, tried in random order until one passes
    min_bytes: 65536   # default
    timeout: 10s       # default; covers lookup, connect, TLS, request and body
    interval: 5m       # default; each upstream draws its own ±20% per probe
```

- A probe passes when at least `min_bytes` of the response body (status 2xx) arrive within
  `timeout`; it reads that much and closes. A short body, an error status or a timeout fail it.
  Redirects are followed through the same upstream. Lotsman ships no default URL: pick one that
  serves at least `min_bytes` without compression and does not mind the traffic, such as
  Cloudflare's speed test endpoint above.
- The connection is bound to the upstream's interface and source address, like the reachability
  probes, for AWG and Outline upstreams alike. The URL's host is resolved by the host's own
  resolver at probe time, outside the upstream.
- `down_after` consecutive failed transfer probes mark an upstream *degraded*; `up_after` passing
  ones clear it. Probes run only while the upstream is up by its reachability probes, at most one
  per upstream at a time, outside the health round. A result that arrives after the upstream went
  down is discarded.
- A degraded upstream is a last resort. A client moves to the best healthy, not degraded upstream
  of its profile in any tier, with the usual tier, latency and stickiness rules. Only when its
  profile has none does it stay on (or go to) a degraded one, chosen by the same rules: slow
  connectivity beats none. `upstream status` shows it as `degraded`, `lotsman_upstream_up` stays 1
  and `lotsman_upstream_degraded` is 1.
- Pinned connections: once no client is routed through a degraded upstream, its pins are released
  as for a dead one; connections that clients opened through it follow their new route and break,
  rather than hanging on a frozen path. While it is still some client's last resort it stays
  alive, so that client's connections are not cut, and the old connections of clients that moved
  away also stay pinned to it until they close.
- Timing: the full `interval` applies while probes pass. After a failure, and for as long as the
  upstream is degraded, the next probe comes after a fifth of it (±20%, at least
  `health.interval`): 48–72 s at the defaults. A freeze is caught by the first scheduled probe that
  hits it (within 4–6 minutes) and confirmed `down_after` − 1 retries later, about 2 more minutes:
  degraded within roughly 3 to 8.5 minutes of the freeze. Recovery takes `up_after` passing
  retries: about 1 to 2.5 minutes after the path recovers.
- Cost: about `min_bytes` × 86400 / `interval` per upstream per day plus TCP, TLS and HTTP overhead
  (a few KB per probe): 18 MiB a day per upstream at the defaults, 90 MiB with `interval: 1m`.
  Only while an upstream's probes fail or it is degraded does it probe five times as often; a
  failing probe moves only what the path lets through, so the extra cost is at most 64 KiB a
  minute (90 MiB a day at the defaults) for that upstream, and only while it lasts.
- Pattern and privacy: inside the tunnel a probe is a ~64 KiB download at an irregular interval,
  among the clients' own traffic. The URL's server sees each upstream's exit address every few
  minutes. The lookup of the URL's host leaves through the host's own uplink, so whoever watches
  that uplink sees the name every interval; an `http://` URL with an IP address, or a resolver you
  trust, avoids that.
- `health.transfer` applies on reload; removing it clears every degraded mark at once. A new
  `interval` takes effect after each upstream's next probe.

## Memory limit

The AmneziaWG runtime allocates its ~64 KiB message buffers from pools that are never capped, so
the daemon's heap follows the traffic and stays high after it. On a 1 vCPU / 454 MiB VPS, four 15 s
`iperf3` runs pushed the RSS to 240–280 MiB, which is close to what such a host has free.

`lotsman serve` therefore sets a soft Go memory limit at startup when `GOMEMLIMIT` is unset or
empty: 40% of the host's `MemTotal` (`/proc/meminfo`), or of the cgroup v2 `memory.max` of the
process when that is a number below it (a container or a unit with `MemoryMax=`). The log shows it
once: `memory limit set limit_mib=… percent_of_memory=40`. If the memory cannot be read, no limit is
set. A limit is soft: the GC works harder near it, but nothing is killed or refused. Helper
processes are not affected.

Measured on that host, a 200 MiB limit against none: upload and single-stream download as fast or
faster, four-stream download about 20% slower (336–352 against 424–458 Mbit/s), and half the peak
RSS (112–138 against 240–280 MiB). At 150 MiB the loss grows to up to 20% on most runs, and at
100 MiB or below the GC costs 35–40% of the throughput on one core, which is why the default is a
share of RAM and not a small constant.

To use another value, set `GOMEMLIMIT` (it always wins; `off` removes the limit) with a drop-in
(`systemctl edit lotsman`):

```ini
[Service]
Environment=GOMEMLIMIT=300MiB
```

then `systemctl restart lotsman`. The default does not follow later changes of RAM or `MemoryMax=`;
it is computed at start.

## Identity and secrets

`state_dir/identity.json` holds the server key, the S1–S4/H1–H4 parameters and the AWG 3 header
protection key every client config embeds. The file serves both `awg` levels; the level only
selects what is used. An identity written before AWG 3 support is upgraded on first load: the key
is derived from the server key, S1–S4 below 12 are raised to 12, and the file is replaced
atomically, so configs issued before must be re-shown. Deleting it invalidates every issued config. `state_dir/lotsman.db` holds client private
keys so configs can be re-shown; treat both files like the server key (mode 0600, root only).
`state_dir/ssconf/` holds the last fetched key of each dynamic Outline upstream (0600); a reload
that removes the upstream, or gives it a static key, deletes its file.

## Troubleshooting

**The provider changed its server IP.** Nothing to do if the provider config names a hostname:
when an upstream's probes fail, Lotsman re-resolves the name (bounded by the probe timeout, half
the health interval and at least 1 s) and, if the address changed, points the running tunnel at
it (`upstream endpoint changed` in the log). A config with a literal IP needs
editing and `systemctl reload lotsman`.

**Clients connect but nothing loads.** Check `upstream status`; if everything is `down`, the
probes cannot get out. Test an upstream config by hand with `awg-quick`, and confirm the probe
target is reachable through it.

**An Outline upstream stays down.** Its health needs a DNS answer over UDP *and* TCP from a
`health.dns` resolver through the server. A wrong key looks like an unreachable server (the server
answers nothing); a server without UDP relay, or a network that blocks its UDP port, fails the UDP
half. The probe error in the `upstream health changed` log line says which protocol failed. Check
the key with the Outline client on another machine. With a dynamic key, look for
`dynamic key fetch failed` (it names the HTTP status, a certificate problem or the provider's
error message) and run `lotsman config check`, which fetches the key once. If the log shows
`proxy helper exited` or `upstream device not up` instead, the helper itself is failing: see the
next item.

**A VLESS upstream stays down.** As for Outline, its probes need DNS answers over UDP and TCP
through the server, both carried over TCP. Run with `-log-level debug` and look at the
`component=xray` lines of that upstream: `connection refused` or `i/o timeout` while dialing is the
server or its port being unreachable. `received real certificate` means the server did not
recognize the client and passed it on to its `sni` site, as it does with strangers: a wrong `pbk`
or `sid` (or an `sni` the server does not serve), or something between the gateway and the server
answering in its place. A wrong user ID passes REALITY and then fails with the connection closed;
over gRPC, so does a wrong `serviceName` or `mode`.
Check the link with an Xray client on another machine.

**An Outline or VLESS upstream keeps restarting.** `lotsman_proxy_restarts_total` rising and `proxy helper
exited` in the log mean its helper process dies. With `panic=…` it crashed: keep the lines logged
as `proxy helper output` around it, they are the stack trace a bug report needs; the other
upstreams are unaffected. `signal: killed` without a panic means something killed it: the OOM
killer (`journalctl -k | grep -i oom`), another process of uid 65534, or the daemon itself after
the helper stopped answering for 2 s (logged just before). `operation not permitted` when starting
means the unit lacks `CAP_SETUID`/`CAP_SETGID` (see [Helper processes](#helper-processes)).

**An upstream shows `degraded`.** Its transfer probes fail while it stays reachable. The
`upstream degraded` log line carries the last error: `16384 of 65536 bytes in 10s` is a path that
freezes or crawls; an error status, `the body ended` or a lookup failure point at the URL, and
then every upstream degrades at once. Run with `-log-level debug` to see each failed probe.

**Sites open, large downloads or video hang.** MTU. `config check` prints the client MTU; make
sure clients actually imported the config with that `MTU` line. Try a lower value in a provider
config's `MTU` if the provider's path is narrower than it claims.

**Replies do not come back through an upstream.** `sysctl net.ipv4.conf.lm-up-*.rp_filter` must
be `2` (loose). Lotsman sets it, but a hardening tool may reset it; in Docker set
`net.ipv4.conf.default.rp_filter=2` at start.

**All clients stopped working after a firewall reload.** Tools that flush nftables (`nft flush
ruleset`, some ufw and firewalld reloads, `iptables -F`) remove Lotsman's `inet lotsman` table or
the accept rules it adds to other forward chains (Docker, ufw). Lotsman follows nftables change
notifications; about 100 ms after such a change it reinstalls what is missing and admits every
routed client again. Until then the host forwards without Lotsman's drop and NAT. Every health
round checks too, which catches a change the notifications missed. If watching fails (logged once
at error level as "watching the firewall for changes failed"), Lotsman restarts the watch with a
backoff of up to a minute and relies on the health round meanwhile, so service returns within one
`health.interval`. Each repair is logged at error level as "firewall rules were removed by another
tool and have been reinstalled" and counted in `lotsman_firewall_repairs_total`; a counter that
keeps rising means a tool on the host keeps removing them. A firewall reload made of several
steps may count more than one repair. Removing rules inside `inet lotsman` while leaving the table, or emptying
its sets by hand, is not detected; restart Lotsman after doing that.

**Another firewall on the host.** Lotsman owns `table inet lotsman`. Docker (`iptables -P FORWARD
DROP`) and ufw add their own forward chains whose drop would win over Lotsman's accept, so on start
Lotsman also inserts two `accept` rules tagged `comment "lotsman"` at the top of every other
forward chain and removes them on shutdown. Only nftables-backed chains are handled; on a host
still using `iptables-legacy` allow `lm0 → lm-up-*` and the replies by hand. The UDP listen port
must be open for input.

**Clients and the host itself.** Lotsman's `input` chain drops everything clients send to the host
itself except a ping to the gateway address, the `metrics.listen` endpoint when it is set, and
replies to connections the host opened toward a client. SSH, Docker's bridge address, a resolver or
anything else bound to `0.0.0.0` is unreachable through the tunnel, even if ufw or firewalld allows
it: a drop in any nftables chain is final. Lotsman's accepts do not override other firewalls either,
so on a host with ufw or firewalld the metrics port must still be opened for input on `lm0`.
Clients that need DNS should use a resolver on the internet, not one on the gateway.

**`another lotsman serve is running in this network namespace`** (or `is using state_dir`). Only
one daemon may run per network namespace, since all of them would share `lm0`, the routing rules
and `table inet lotsman`, and only one per state directory. A second `serve` checks both before it
reads the state directory or touches the network, so it exits with this error and leaves the
running daemon, its pid file and its clients alone. The locks are an abstract Unix socket
`@lotsman` and a flock on `state_dir/serve.lock`; the kernel releases both when the daemon exits,
crash included, so there is nothing to clean up. `ss -xap | grep @lotsman` shows which process
holds the first: if it is not Lotsman, that process blocks it from starting.

**Restart left rules behind.** Lotsman removes its rules, routes and nftables table on shutdown and
recreates them on start, so a crash is harmless. It only removes rules and routes of the exact
shape it installs (`from <addr>/32 [fwmark N] lookup N`, priorities 3000/4000/5000) in tables
1000-1099 that hold nothing else, and never uses a table that already has routes or rules.
