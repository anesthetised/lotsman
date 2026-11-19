// A small traffic simulation behind the fake API: a fixed set of upstreams and
// peers whose rates take a random walk. It is deterministic for a given seed
// and clock, so tests can assert on it.

import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import {
  type DevicePeer,
  DevicePeerSchema,
  DeviceSchema,
  type HistoryResponse,
  HistoryResponseSchema,
  type PeerStatus,
  PeerStatusSchema,
  SampleSchema,
  type StatusSnapshot,
  StatusSnapshotSchema,
  type Traffic,
  TrafficSchema,
  UpstreamKind,
  UpstreamState,
  UpstreamStatusSchema,
  type User,
  UserSchema,
} from "../gen/lotsman/v1/lotsman_pb";

const HOUR = 3_600_000;
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
  { name: "de-1", kind: UpstreamKind.AWG, geo: "DE", provider: "provider-a", state: UpstreamState.DOWN, latencyMs: 0 },
];

const peerSeeds = [
  ["alice", "phone", "default", "lv-1"],
  ["alice", "laptop", "default", "lv-1"],
  ["alice", "tv", "streaming", "nl-1"],
  ["bob", "phone", "default", "al-1"],
  ["bob", "laptop", "streaming", "nl-1"],
  ["carol", "phone", "default", ""], // blocked: no upstream fits her profile
] as const;

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

  /** The current state, optionally limited to the peers `include` accepts and their upstreams. */
  snapshot(include: (peer: { user: string }) => boolean = () => true): StatusSnapshot {
    this.advance();
    const peers = this.peers.filter(include);
    const used = new Set(peers.map((p) => p.upstream));
    const all = peers.length === this.peers.length;
    const sum = (list: Peer[]) =>
      list.reduce(
        (t, p) => ({ down: t.down + p.down, up: t.up + p.up, downRate: t.downRate + p.downRate, upRate: t.upRate + p.upRate }),
        { down: 0, up: 0, downRate: 0, upRate: 0 },
      );

    return create(StatusSnapshotSchema, {
      time: timestampFromMs(this.time),
      clientMtu: 1376,
      upstreams: upstreams
        .filter((u) => all || used.has(u.name))
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
            // A user's view carries no counts or traffic of other users.
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
      const peer = create(DevicePeerSchema, { peerId: p.id, profile: p.profile, address: p.address });
      devices.set(p.device, [...(devices.get(p.device) ?? []), peer]);
    }
    return create(UserSchema, {
      name,
      hasLink: true,
      devices: [...devices].map(([device, peers]) => create(DeviceSchema, { name: device, peers })),
    });
  }

  /** Hourly availability of an upstream between two instants, oldest first. */
  history(name: string, from: number, to: number): HistoryResponse {
    const upstream = upstreams.find((u) => u.name === name);
    const samples = [];
    for (let t = Math.ceil(from / HOUR) * HOUR; t <= to; t += HOUR) {
      samples.push(
        create(SampleSchema, {
          time: timestampFromMs(t),
          upRatio: upstream ? availability(upstream, t, to) : 0,
        }),
      );
    }
    return create(HistoryResponseSchema, { step: durationFromMs(HOUR), samples });
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
