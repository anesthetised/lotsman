// A small simulation behind the fake API: a fixed set of upstreams, peers
// whose rates take a random walk, the connections they open, and the
// profiles, users and links the operator changes. It is deterministic for a
// given seed, clock and sequence of calls, so tests can assert on it.

import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { type Connection, ConnectionSchema, type ConnectionSnapshot, ConnectionSnapshotSchema, Protocol } from "../gen/lotsman/v1/connection_pb";
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
import { type Tier, tierString } from "./profiles";

const MINUTE = 60_000;
const HOUR = 3_600_000;
const DAY = 86_400_000;
const MAX_SAMPLES = 2000;
const MAX_RATE = 6_000_000; // bytes per second, about 48 Mbit/s
/** The fake's cap on connections per update; the daemon's is 500. Low, so the full list shows `truncated`. */
export const CONNECTION_CAP = 25;

interface Upstream {
  name: string;
  kind: UpstreamKind;
  geo: string;
  provider: string;
  tags: readonly string[];
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
  created: number; // ms since the epoch
  /** Whether the device ever connected; an added one never does. */
  connected: boolean;
  lastHandshake: number; // ms since the epoch, 0 = never
  downRate: number;
  upRate: number;
  down: number;
  up: number;
}

interface Flow {
  peer: Peer;
  protocol: Protocol;
  source: string;
  destination: string;
  started: number; // ms since the epoch, 0 = open before anyone watched
  /** Its share of the peer's traffic, relative to the peer's other flows. */
  weight: number;
  down: number;
  up: number;
  downRate: number;
  upRate: number;
}

interface UserRecord {
  name: string;
  created: number;
  hasLink: boolean;
}

export interface Profile {
  name: string;
  prefer: Tier[];
}

// The tables below are pure IIFEs: a production build drops the fake, and
// without the annotation it would keep their enum reads and calls.
const upstreams: readonly Upstream[] = /* @__PURE__ */ (() => [
  { name: "lv-1", kind: UpstreamKind.AWG, geo: "LV", provider: "provider-a", tags: ["near", "eu"], state: UpstreamState.UP, latencyMs: 38 },
  { name: "al-1", kind: UpstreamKind.OUTLINE, geo: "AL", provider: "provider-b", tags: [], state: UpstreamState.UP, latencyMs: 61 },
  { name: "nl-1", kind: UpstreamKind.VLESS, geo: "NL", provider: "provider-c", tags: ["eu"], state: UpstreamState.DEGRADED, latencyMs: 140 },
  { name: "de-1", kind: UpstreamKind.AWG, geo: "DE", provider: "provider-a", tags: ["near", "eu"], state: UpstreamState.DOWN, latencyMs: 45 },
])();

const tier = (t: Partial<Tier>): Tier => ({ geo: "", provider: "", tag: "", ...t });

// Stored profiles, as if `profile add` had made them. `default` has a tier
// that never applies; `backup` routes new devices through al-1; `fi`'s only
// tier matches no upstream, so its users are blocked (the daemon keeps such a
// profile with a warning: its upstream was removed after it was stored).
const profileSeeds: readonly Profile[] = /* @__PURE__ */ (() => [
  { name: "default", prefer: [tier({ tag: "near" }), tier({ geo: "LV" }), tier({})] },
  { name: "streaming", prefer: [tier({ geo: "NL" }), tier({ tag: "eu" })] },
  { name: "backup", prefer: [tier({ geo: "AL" })] },
  { name: "fi", prefer: [tier({ geo: "FI" })] },
])();

// The seeded peers keep these upstreams; peers added later are routed by their profile.
const peerSeeds = [
  ["alice", "phone", "default", "lv-1"],
  ["alice", "laptop", "default", "lv-1"],
  ["alice", "tv", "streaming", "nl-1"],
  ["bob", "phone", "default", "al-1"],
  ["bob", "laptop", "streaming", "nl-1"],
  ["carol", "phone", "fi", ""], // blocked: no upstream fits her profile
] as const;

/**
 * Where the simulated connections go (documentation addresses only, and the
 * gateway's resolver), with how heavy such a flow is: pings and DNS barely move bytes.
 */
const destinations: readonly (readonly [Protocol, string, number, number])[] = /* @__PURE__ */ (() => [
  [Protocol.TCP, "198.51.100.10", 443, 1],
  [Protocol.TCP, "198.51.100.23", 443, 1],
  [Protocol.UDP, "203.0.113.5", 443, 1],
  [Protocol.TCP, "203.0.113.80", 80, 0.5],
  [Protocol.TCP, "198.51.100.77", 993, 0.1],
  [Protocol.UDP, "192.0.2.44", 3478, 0.5],
  [Protocol.UDP, "10.8.0.1", 53, 0.001],
  [Protocol.ICMP, "192.0.2.1", 0, 0.0001],
] as const)();

const NAME_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;

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
  /** Connections and new secrets draw from their own sequences, so the traffic stays as it was. */
  private readonly flowRand: () => number;
  private readonly secretRand: () => number;
  private readonly peers: Peer[];
  private readonly users = new Map<string, UserRecord>();
  private profiles: Profile[] = profileSeeds.map((p) => ({ name: p.name, prefer: p.prefer.map((t) => ({ ...t })) }));
  private flows: Flow[] = [];
  private nextPeer: number;
  private time: number;

  constructor(
    seed = 1,
    private readonly clock: () => number = Date.now,
  ) {
    this.rand = random(seed);
    this.flowRand = random(seed ^ 0x5bd1e995);
    this.secretRand = random(seed ^ 0x27d4eb2f);
    this.time = clock();
    this.peers = peerSeeds.map(([user, device, profile, upstream], i) => ({
      id: `peer-${i + 1}`,
      user,
      device,
      profile,
      address: `10.8.0.${i + 2}`,
      upstream,
      created: this.time - (40 - i * 5) * DAY,
      connected: upstream !== "",
      // carol never connected; everyone else did recently.
      lastHandshake: upstream ? this.time - Math.round(this.rand() * 100_000) : 0,
      downRate: upstream ? this.rand() * MAX_RATE : 0,
      upRate: upstream ? this.rand() * MAX_RATE * 0.2 : 0,
      down: upstream ? this.rand() * 4e9 : 0,
      up: upstream ? this.rand() * 4e8 : 0,
    }));
    this.nextPeer = this.peers.length + 1;
    for (const [i, p] of this.peers.entries()) {
      if (!this.users.has(p.user)) this.users.set(p.user, { name: p.user, created: p.created - DAY, hasLink: i === 0 });
    }
    for (const p of this.peers.filter((p) => p.connected)) {
      const count = 4 + Math.floor(this.flowRand() * 5);
      for (let i = 0; i < count; i++) this.flows.push(this.newFlow(p, 0));
    }
    this.shareTraffic();
  }

  /** Advances the simulation in one-second steps up to the clock. */
  private advance(): void {
    const now = this.clock();
    while (this.time + 1000 <= now) {
      this.time += 1000;
      for (const p of this.peers) {
        if (!p.connected) continue;
        p.downRate = walk(p.downRate, MAX_RATE, this.rand());
        p.upRate = walk(p.upRate, MAX_RATE * 0.2, this.rand());
        p.down += p.downRate;
        p.up += p.upRate;
        if (this.rand() < 0.05) p.lastHandshake = this.time;
      }
      // Now and then a connection closes and another opens.
      if (this.flows.length > 0 && this.flowRand() < 0.3) {
        const i = Math.floor(this.flowRand() * this.flows.length);
        this.flows[i] = this.newFlow(this.flows[i]!.peer, this.time);
      }
      this.shareTraffic();
      for (const f of this.flows) {
        f.down += f.downRate;
        f.up += f.upRate;
      }
    }
  }

  /** Splits each peer's rates over its connections by their weights. */
  private shareTraffic(): void {
    const weights = new Map<Peer, number>();
    for (const f of this.flows) weights.set(f.peer, (weights.get(f.peer) ?? 0) + f.weight);
    for (const f of this.flows) {
      const share = f.weight / weights.get(f.peer)!;
      f.downRate = f.peer.downRate * share;
      f.upRate = f.peer.upRate * share;
    }
  }

  private newFlow(peer: Peer, started: number): Flow {
    const [protocol, ip, port, heft] = destinations[Math.floor(this.flowRand() * destinations.length)]!;
    const sourcePort = 32768 + Math.floor(this.flowRand() * 28000);
    return {
      peer,
      protocol,
      source: protocol === Protocol.ICMP ? peer.address : `${peer.address}:${sourcePort}`,
      destination: port ? `${ip}:${port}` : ip,
      started,
      weight: heft * (this.flowRand() ** 2 + 0.01),
      down: heft * this.flowRand() * 5e7,
      up: heft * this.flowRand() * 5e6,
      downRate: 0,
      upRate: 0,
    };
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
            tags: [...u.tags],
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

  /**
   * The open connections of one peer, or of all, fastest first and at most
   * CONNECTION_CAP of them. NotFound for an unknown peer, as the daemon answers.
   */
  connections(peerId = ""): ConnectionSnapshot {
    this.advance();
    if (peerId && !this.peers.some((p) => p.id === peerId)) throw new ConnectError(`peer ${peerId} not found`, Code.NotFound);
    const flows = this.flows
      .filter((f) => !peerId || f.peer.id === peerId)
      .sort((a, b) => b.downRate + b.upRate - (a.downRate + a.upRate));
    return create(ConnectionSnapshotSchema, {
      time: timestampFromMs(this.time),
      connections: flows.slice(0, CONNECTION_CAP).map((f) => connection(f)),
      truncated: flows.length > CONNECTION_CAP,
    });
  }

  /** The user with their devices, one peer per profile. */
  user(name: string): User {
    const record = this.users.get(name);
    const devices = new Map<string, DevicePeer[]>();
    for (const p of this.peers.filter((p) => p.user === name)) {
      const peer = create(DevicePeerSchema, { peerId: p.id, profile: p.profile, address: p.address, created: timestampFromMs(p.created) });
      devices.set(p.device, [...(devices.get(p.device) ?? []), peer]);
    }
    return create(UserSchema, {
      name,
      hasLink: record?.hasLink ?? false,
      created: record ? timestampFromMs(record.created) : undefined,
      devices: [...devices].map(([device, peers]) => create(DeviceSchema, { name: device, peers })),
    });
  }

  /** Every user, in the order they were created. */
  listUsers(): User[] {
    return [...this.users.keys()].map((name) => this.user(name));
  }

  /**
   * Adds a peer for the user's device with the profile, creating the user if
   * needed, as `user add` does. An empty device is the default one.
   */
  addDevice(user: string, device: string, profile: string): { peerId: string; config: string } {
    device ||= "default";
    checkNames("user", user, "device", device, "profile", profile);
    const p = this.profiles.find((p) => p.name === profile);
    if (!p) throw new ConnectError(`no such profile: ${profile}`, Code.InvalidArgument);
    if (this.peers.some((x) => x.user === user && x.device === device && x.profile === profile)) {
      throw new ConnectError(`${user}/${device} already has profile ${profile}`, Code.AlreadyExists);
    }
    const peerId = this.addPeer(user, device, profile).peerId;
    return { peerId, config: this.config(peerId) };
  }

  /**
   * Adds a peer for a known profile without the daemon's checks (the callers
   * make them), creating the user if needed. It is routed by its profile and
   * never connects.
   */
  addPeer(user: string, device: string, profile: string): DevicePeer {
    this.advance();
    if (!this.users.has(user)) this.users.set(user, { name: user, created: this.time, hasLink: false });
    const p = this.profiles.find((p) => p.name === profile);
    const peer: Peer = {
      id: `peer-${this.nextPeer++}`,
      user,
      device,
      profile,
      address: this.freeAddress(),
      upstream: p ? selectUpstream(p) : "",
      created: this.time,
      connected: false,
      lastHandshake: 0,
      downRate: 0,
      upRate: 0,
      down: 0,
      up: 0,
    };
    this.peers.push(peer);
    return create(DevicePeerSchema, { peerId: peer.id, profile, address: peer.address, created: timestampFromMs(peer.created) });
  }

  /** Removes the device's peer of that profile, or every peer of the device when profile is empty. */
  removeDevice(user: string, device: string, profile: string): void {
    device ||= "default";
    if (this.removePeers(user, device, profile) === 0) throw new ConnectError("no such device or profile", Code.NotFound);
  }

  /** Removes one profile of a device, or all of them when `profile` is empty; how many peers went. */
  removePeers(user: string, device: string, profile: string): number {
    const gone = this.peers.filter((p) => p.user === user && p.device === device && (!profile || p.profile === profile));
    this.dropPeers(gone);
    return gone.length;
  }

  /** Removes the user with every peer and the link. */
  removeUser(user: string): void {
    if (!this.users.delete(user)) throw new ConnectError("no such user", Code.NotFound);
    this.dropPeers(this.peers.filter((p) => p.user === user));
  }

  /** A new link token for the user: 256 bits, base64url. Their previous one dies. */
  issueLink(user: string): string {
    const record = this.users.get(user);
    if (!record) throw new ConnectError("no such user", Code.NotFound);
    record.hasLink = true;
    return base64url(Array.from({ length: 32 }, () => Math.floor(this.secretRand() * 256)));
  }

  revokeLink(user: string): void {
    const record = this.users.get(user);
    if (!record) throw new ConnectError("no such user", Code.NotFound);
    record.hasLink = false;
  }

  /** The client config of a peer, shaped like the daemon's. NotFound for an unknown peer. */
  config(peerId: string): string {
    const p = this.peers.find((p) => p.id === peerId);
    if (!p) throw new ConnectError("no such peer", Code.NotFound);
    return clientConfig(p.id, p.address);
  }

  /** The stored profiles and their warnings against the upstreams. */
  listProfiles(): { profiles: Profile[]; warnings: string[] } {
    return { profiles: this.profiles, warnings: profileWarnings(this.profiles) };
  }

  /** Creates or replaces a profile, as `profile add` and `profile set` do; returns every profile's warnings. */
  putProfile(profile: Profile): string[] {
    if (profile.name === "") throw new ConnectError("profile: name is required", Code.InvalidArgument);
    if (!NAME_RE.test(profile.name)) {
      throw new ConnectError(`invalid profile: profile name "${profile.name}" must match ${NAME_RE.source}`, Code.InvalidArgument);
    }
    if (profile.prefer.length > 0 && !tierSizes(profile).some((t) => t.own > 0)) {
      throw new ConnectError(`invalid profile: profile "${profile.name}": prefer matches no upstream`, Code.InvalidArgument);
    }
    const stored = { name: profile.name, prefer: profile.prefer.map((t) => ({ geo: t.geo, provider: t.provider, tag: t.tag })) };
    const i = this.profiles.findIndex((p) => p.name === profile.name);
    if (i < 0) this.profiles.push(stored);
    else this.profiles[i] = stored;
    return profileWarnings(this.profiles);
  }

  /** Deletes a profile no peer uses. */
  deleteProfile(name: string): void {
    if (!this.profiles.some((p) => p.name === name)) throw new ConnectError(`profile "${name}" not found`, Code.NotFound);
    if (this.peers.some((p) => p.profile === name)) {
      throw new ConnectError(`profile "${name}" is in use: remove its peers first`, Code.FailedPrecondition);
    }
    this.profiles = this.profiles.filter((p) => p.name !== name);
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
      const down = peer && !peer.connected ? 0 : (h * MAX_RATE * 0.4 * routed * (upstream ? ratio : 1) * step) / 1000;
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
    return this.profiles.map((p) => p.name);
  }

  /** The device names of `user`, in the order they were added. */
  devicesOf(user: string): string[] {
    return [...new Set(this.peers.filter((p) => p.user === user).map((p) => p.device))];
  }

  /** Whether `user` has the device with this profile. */
  hasPeer(user: string, device: string, profile: string): boolean {
    return this.peers.some((p) => p.user === user && p.device === device && p.profile === profile);
  }

  private dropPeers(gone: readonly Peer[]): void {
    this.peers.splice(0, this.peers.length, ...this.peers.filter((p) => !gone.includes(p)));
    this.flows = this.flows.filter((f) => !gone.includes(f.peer));
  }

  private freeAddress(): string {
    const used = new Set(this.peers.map((p) => p.address));
    for (let i = 2; i < 255; i++) if (!used.has(`10.8.0.${i}`)) return `10.8.0.${i}`;
    throw new ConnectError("the gateway's subnet has no free address", Code.ResourceExhausted);
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

function connection(f: Flow): Connection {
  return create(ConnectionSchema, {
    peerId: f.peer.id,
    protocol: f.protocol,
    source: f.source,
    destination: f.destination,
    upstream: f.peer.upstream,
    started: f.started ? timestampFromMs(f.started) : undefined,
    traffic: traffic(f.down, f.up, f.downRate, f.upRate),
  });
}

function matches(t: Tier, u: Upstream): boolean {
  return (!t.geo || t.geo === u.geo) && (!t.provider || t.provider === u.provider) && (!t.tag || u.tags.includes(t.tag));
}

/**
 * Per tier, as the daemon counts them: the upstreams it matches, those for
 * which it is the first match (its own), and the earlier tiers that took the rest.
 */
function tierSizes(p: Profile): { matched: number; own: number; shadowers: number[] }[] {
  const tiers = p.prefer.map(() => ({ matched: 0, own: 0, shadowers: [] as number[] }));
  for (const u of upstreams) {
    let first = -1;
    for (const [j, t] of p.prefer.entries()) {
      if (!matches(t, u)) continue;
      const size = tiers[j]!;
      size.matched++;
      if (first < 0) {
        first = j;
        size.own++;
      } else if (!size.shadowers.includes(first)) size.shadowers.push(first);
    }
  }
  for (const t of tiers) t.shadowers.sort((a, b) => a - b);
  return tiers;
}

/** The daemon's profile warnings, word for word: tiers that match nothing, or only what earlier tiers took. */
export function profileWarnings(profiles: readonly Profile[]): string[] {
  const out: string[] = [];
  for (const p of profiles) {
    for (const [j, t] of tierSizes(p).entries()) {
      const m = tierString(p.prefer[j]!);
      if (t.matched === 0) out.push(`profile "${p.name}": tier ${j + 1} ${m} matches no upstream and is ignored`);
      else if (t.own === 0) {
        const by = t.shadowers.map((k) => `tier ${k + 1} ${tierString(p.prefer[k]!)}`).join(", ");
        out.push(`profile "${p.name}": tier ${j + 1} ${m} never applies: every upstream it matches is taken by ${by}`);
      }
    }
  }
  return out;
}

/** The upstream a new peer of the profile gets: the first tier with one up, lowest latency first; "" if none. */
function selectUpstream(p: Profile): string {
  for (const t of p.prefer.length > 0 ? p.prefer : [{ geo: "", provider: "", tag: "" }]) {
    const up = upstreams.filter((u) => u.state === UpstreamState.UP && matches(t, u)).sort((a, b) => a.latencyMs - b.latencyMs);
    if (up[0]) return up[0].name;
  }
  return "";
}

/** The daemon's checks of names that come through the API. */
function checkNames(...pairs: string[]): void {
  for (let i = 0; i + 1 < pairs.length; i += 2) {
    const [what, name] = [pairs[i]!, pairs[i + 1]!];
    if (name === "") throw new ConnectError(`${what} is required`, Code.InvalidArgument);
    if (new TextEncoder().encode(name).length > 64 || /[\u0000-\u001f\u007f-\u009f]/.test(name) || name.trim() !== name) {
      throw new ConnectError(`${what} must be printable UTF-8 of at most 64 bytes, without spaces around it`, Code.InvalidArgument);
    }
  }
}

function base64url(bytes: number[]): string {
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
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
