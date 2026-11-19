// A small traffic simulation behind the fake API: a fixed set of upstreams and
// peers whose rates take a random walk. It is deterministic for a given seed
// and clock, so tests can assert on it.

import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { type HistoryResponse, HistoryResponseSchema, SampleSchema } from "../gen/lotsman/v1/history_pb";
import {
  type PeerStatus,
  PeerStatusSchema,
  type StatusSnapshot,
  StatusSnapshotSchema,
  type Traffic,
  TrafficSchema,
  UpstreamKind,
  UpstreamState,
  UpstreamStatusSchema,
} from "../gen/lotsman/v1/status_pb";
import { type DevicePeer, DevicePeerSchema, DeviceSchema, type User, UserSchema } from "../gen/lotsman/v1/user_pb";

const MINUTE = 60_000;
const HOUR = 3_600_000;
const DAY = 86_400_000;
const MAX_SAMPLES = 2000;
const MAX_RATE = 6_000_000; // bytes per second, about 48 Mbit/s

interface Upstream {
  name: string;
  kind: UpstreamKind;
  geo: string;
  provider: string;
  state: UpstreamState;
  latencyMs: number;
}

interface Peer {
  id: string;
  user: string;
  device: string;
  profile: string;
  address: string;
  upstream: string;
  created?: number; // ms since the epoch; the seeded peers predate the simulation
  lastHandshake: number; // ms since the epoch, 0 = never
  downRate: number;
  upRate: number;
  down: number;
  up: number;
}

const upstreams: readonly Upstream[] = [
  { name: "lv-1", kind: UpstreamKind.AWG, geo: "LV", provider: "provider-a", state: UpstreamState.UP, latencyMs: 38 },
  { name: "al-1", kind: UpstreamKind.OUTLINE, geo: "AL", provider: "provider-b", state: UpstreamState.UP, latencyMs: 61 },
  { name: "nl-1", kind: UpstreamKind.VLESS, geo: "NL", provider: "provider-c", state: UpstreamState.DEGRADED, latencyMs: 140 },
  { name: "de-1", kind: UpstreamKind.AWG, geo: "DE", provider: "provider-a", state: UpstreamState.DOWN, latencyMs: 45 },
];

const peerSeeds = [
  ["alice", "phone", "default", "lv-1"],
  ["alice", "laptop", "default", "lv-1"],
  ["alice", "tv", "streaming", "nl-1"],
  ["bob", "phone", "default", "al-1"],
  ["bob", "laptop", "streaming", "nl-1"],
  ["carol", "phone", "default", ""], // blocked: no upstream fits her profile
] as const;

/** The upstream each profile resolves to for peers added at runtime. */
const profileUpstream: Readonly<Record<string, string>> = { default: "lv-1", streaming: "nl-1", backup: "al-1" };

/** mulberry32: a tiny seeded PRNG returning floats in [0, 1). */
function random(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function traffic(down: number, up: number, downRate: number, upRate: number): Traffic {
  return create(TrafficSchema, {
    downBytes: BigInt(Math.round(down)),
    upBytes: BigInt(Math.round(up)),
    downRate: BigInt(Math.round(downRate)),
    upRate: BigInt(Math.round(upRate)),
  });
}

export class Simulation {
  private readonly rand: () => number;
  private readonly peers: Peer[];
  private time: number;
  private lastId = peerSeeds.length;

  constructor(
    seed = 1,
    private readonly clock: () => number = Date.now,
  ) {
    this.rand = random(seed);
    this.time = clock();
    this.peers = peerSeeds.map(([user, device, profile, upstream], i) => ({
      id: `peer-${i + 1}`,
      user,
      device,
      profile,
      address: `10.8.0.${i + 2}`,
      upstream,
      // carol never connected; everyone else did recently.
      lastHandshake: upstream ? this.time - Math.round(this.rand() * 100_000) : 0,
      downRate: upstream ? this.rand() * MAX_RATE : 0,
      upRate: upstream ? this.rand() * MAX_RATE * 0.2 : 0,
      down: upstream ? this.rand() * 4e9 : 0,
      up: upstream ? this.rand() * 4e8 : 0,
    }));
  }

  /** Advances the simulation in one-second steps up to the clock. */
  private advance(): void {
    const now = this.clock();
    while (this.time + 1000 <= now) {
      this.time += 1000;
      for (const p of this.peers) {
        if (!p.upstream) continue;
        p.downRate = walk(p.downRate, MAX_RATE, this.rand());
        p.upRate = walk(p.upRate, MAX_RATE * 0.2, this.rand());
        p.down += p.downRate;
        p.up += p.upRate;
        if (this.rand() < 0.05) p.lastHandshake = this.time;
      }
    }
  }

  /**
   * The current state: everything, or the view of one user. That view has the
   * user's own peers and the upstreams they may use (here: the ones their peers
   * are routed through), without the routed and active counts or the traffic
   * of those upstreams, which would show other users.
   */
  snapshot(user?: string): StatusSnapshot {
    this.advance();
    const all = user === undefined;
    const peers = all ? this.peers : this.peers.filter((p) => p.user === user);
    const used = this.upstreamsOf(user);
    const sum = (list: Peer[]) =>
      list.reduce(
        (t, p) => ({ down: t.down + p.down, up: t.up + p.up, downRate: t.downRate + p.downRate, upRate: t.upRate + p.upRate }),
        { down: 0, up: 0, downRate: 0, upRate: 0 },
      );

    return create(StatusSnapshotSchema, {
      time: timestampFromMs(this.time),
      clientMtu: 1376,
      upstreams: upstreams
        .filter((u) => used.has(u.name))
        .map((u) => {
          const routed = this.peers.filter((p) => p.upstream === u.name);
          const t = sum(routed);
          const jitter = 1 + (this.rand() - 0.5) * 0.2;
          return create(UpstreamStatusSchema, {
            name: u.name,
            kind: u.kind,
            geo: u.geo,
            provider: u.provider,
            state: u.state,
            latency: u.state === UpstreamState.DOWN ? undefined : durationFromMs(Math.round(u.latencyMs * jitter)),
            lastHandshake: u.kind === UpstreamKind.AWG && u.state !== UpstreamState.DOWN ? timestampFromMs(this.time - 20_000) : undefined,
            routed: all ? routed.length : 0,
            active: all ? routed.filter((p) => this.isActive(p)).length : 0,
            traffic: all ? traffic(t.down, t.up, t.downRate, t.upRate) : undefined,
          });
        }),
      peers: peers.map((p) => this.peerStatus(p)),
      total: (({ down, up, downRate, upRate }) => traffic(down, up, downRate, upRate))(sum(peers)),
    });
  }

  /** The user with their devices, one peer per profile. */
  user(name: string): User {
    const devices = new Map<string, DevicePeer[]>();
    for (const p of this.peers.filter((p) => p.user === name)) {
      const peer = create(DevicePeerSchema, {
        peerId: p.id,
        profile: p.profile,
        address: p.address,
        created: p.created ? timestampFromMs(p.created) : undefined,
      });
      devices.set(p.device, [...(devices.get(p.device) ?? []), peer]);
    }
    return create(UserSchema, {
      name,
      hasLink: true,
      devices: [...devices].map(([device, peers]) => create(DeviceSchema, { name: device, peers })),
    });
  }

  /** The upstreams `user` may use; every upstream when `user` is unset. */
  upstreamsOf(user?: string): Set<string> {
    if (user === undefined) return new Set(upstreams.map((u) => u.name));
    return new Set(this.peers.filter((p) => p.user === user && p.upstream).map((p) => p.upstream));
  }

  /** The peer's user, or undefined for an unknown peer. */
  ownerOf(peerId: string): string | undefined {
    return this.peers.find((p) => p.id === peerId)?.user;
  }

  /**
   * Samples of an upstream (by name) or a peer (by peer_id) between two instants,
   * oldest first, at the step the daemon would use for `wanted` (see historyStep).
   * Undefined when there is no such upstream or peer.
   */
  history(kind: "upstream" | "peer", name: string, from: number, to: number, wanted = 0): HistoryResponse | undefined {
    const upstream = kind === "upstream" ? upstreams.find((u) => u.name === name) : undefined;
    const peer = kind === "peer" ? this.peers.find((p) => p.id === name) : undefined;
    if (!upstream && !peer) return undefined;

    const step = historyStep(from, to, this.clock(), wanted);
    const routed = upstream ? this.peers.filter((p) => p.upstream === upstream.name).length : 1;
    const samples = [];
    for (let t = Math.ceil(from / step) * step; t <= to; t += step) {
      const h = random(Math.floor(t / MINUTE) * 17 + name.charCodeAt(0))();
      const ratio = upstream ? availability(upstream, t, to) : 0;
      // Bytes moved during the step: about a fifth of the peak rate per peer while up.
      const down = peer && !peer.upstream ? 0 : (h * MAX_RATE * 0.4 * routed * (upstream ? ratio : 1) * step) / 1000;
      samples.push(
        create(SampleSchema, {
          time: timestampFromMs(t),
          upRatio: ratio,
          latency: upstream && ratio > 0 ? durationFromMs(Math.round(upstream.latencyMs * (0.9 + h * 0.2))) : undefined,
          downBytes: BigInt(Math.round(down)),
          upBytes: BigInt(Math.round(down * 0.15)),
        }),
      );
    }
    return create(HistoryResponseSchema, { step: durationFromMs(step), samples });
  }

  /** The profiles users may choose from, in the order the daemon lists them. */
  profileNames(): string[] {
    return Object.keys(profileUpstream);
  }

  /** The device names of `user`, in the order they were added. */
  devicesOf(user: string): string[] {
    return [...new Set(this.peers.filter((p) => p.user === user).map((p) => p.device))];
  }

  /** Whether `user` has the device with this profile. */
  hasPeer(user: string, device: string, profile: string): boolean {
    return this.peers.some((p) => p.user === user && p.device === device && p.profile === profile);
  }

  /** Adds a peer for a known profile: routed through that profile's upstream, never connected yet. */
  addPeer(user: string, device: string, profile: string, created = this.clock()): DevicePeer {
    const id = `peer-${++this.lastId}`;
    this.peers.push({
      id,
      user,
      device,
      profile,
      address: `10.8.0.${this.lastId + 1}`,
      upstream: profileUpstream[profile] ?? "",
      created,
      lastHandshake: 0,
      downRate: 0,
      upRate: 0,
      down: 0,
      up: 0,
    });
    return create(DevicePeerSchema, { peerId: id, profile, address: `10.8.0.${this.lastId + 1}`, created: timestampFromMs(created) });
  }

  /** Removes one profile of a device, or all of them when `profile` is empty; how many peers went. */
  removePeers(user: string, device: string, profile: string): number {
    const gone = this.peers.filter((p) => p.user === user && p.device === device && (!profile || p.profile === profile));
    for (const p of gone) this.peers.splice(this.peers.indexOf(p), 1);
    return gone.length;
  }

  /** The client config of a peer, or undefined for an unknown peer. Made-up keys, deterministic. */
  config(peerId: string): string | undefined {
    const p = this.peers.find((p) => p.id === peerId);
    return p && clientConfig(p.id, p.address);
  }

  private isActive(p: Peer): boolean {
    return p.lastHandshake > 0 && this.time - p.lastHandshake < 180_000;
  }

  private peerStatus(p: Peer): PeerStatus {
    return create(PeerStatusSchema, {
      peerId: p.id,
      user: p.user,
      device: p.device,
      profile: p.profile,
      address: p.address,
      upstream: p.upstream,
      lastHandshake: p.lastHandshake ? timestampFromMs(p.lastHandshake) : undefined,
      traffic: traffic(p.down, p.up, p.downRate, p.upRate),
    });
  }
}

/**
 * The step the daemon uses for a history range: the wanted step rounded up to
 * a multiple of what it keeps for the range (1 min samples for the last 7 days,
 * else 1 h), and further so that at most 2000 samples come back.
 */
export function historyStep(from: number, to: number, now: number, wanted = 0): number {
  const kept = now - from <= 7 * DAY ? MINUTE : HOUR;
  let step = Math.max(1, Math.ceil(wanted / kept), Math.ceil((to - from) / MAX_SAMPLES / kept)) * kept;
  while (Math.floor(to / step) - Math.ceil(from / step) + 1 > MAX_SAMPLES) step += kept;
  return step;
}

/** One random-walk step, kept inside [0, max]. */
function walk(value: number, max: number, r: number): number {
  return Math.min(max, Math.max(0, value + (r - 0.5) * max * 0.3));
}

/** A stable, made-up availability for the hour that starts at `t`. */
function availability(u: Upstream, t: number, now: number): number {
  if (u.state === UpstreamState.DOWN && now - t < 6 * HOUR) return 0;
  const h = random(Math.floor(t / HOUR) * 31 + u.name.charCodeAt(0))();
  if (u.state === UpstreamState.DEGRADED) return h < 0.3 ? 0.7 + h : 1;
  return h < 0.04 ? 0.5 + h * 10 : 1;
}

/** A made-up 32-byte key in base64, stable for a seed. Not a real key. */
function fakeKey(seed: string): string {
  const rand = random([...seed].reduce((h, c) => Math.imul(h ^ c.charCodeAt(0), 16777619), 2166136261));
  return btoa(String.fromCharCode(...Array.from({ length: 32 }, () => Math.floor(rand() * 256))));
}

/** What the daemon's ClientConfig renders for a peer: an AmneziaWG client config of about 600 bytes. */
function clientConfig(peerId: string, address: string): string {
  return [
    "[Interface]",
    `PrivateKey = ${fakeKey(`private-${peerId}`)}`,
    `Address = ${address}/32`,
    "DNS = 1.1.1.1, 9.9.9.9",
    "MTU = 1376",
    "Jc = 4",
    "Jmin = 40",
    "Jmax = 70",
    "S1 = 52",
    "S2 = 97",
    "S3 = 21",
    "S4 = 9",
    "H1 = 1823591044-1823591099",
    "H2 = 2047392011-2047392077",
    "H3 = 3174402331-3174402388",
    "H4 = 4012345678-4012345710",
    "I1 = <b 0xc000000001><r 64><t>",
    "",
    "[Peer]",
    `PublicKey = ${fakeKey("gateway")}`,
    "Endpoint = vpn.example.com:51820",
    "AllowedIPs = 0.0.0.0/0",
    "PersistentKeepalive = 25",
    "",
  ].join("\n");
}
