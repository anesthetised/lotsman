// A small simulation behind the fake API: a fixed set of upstreams, peers
// whose rates take a random walk, the connections they open, and the users,
// their rights, devices, routes and links the operator changes. It is
// deterministic for a given seed, clock and sequence of calls, so tests can assert on it.

import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { type Connection, ConnectionSchema, type ConnectionSnapshot, ConnectionSnapshotSchema, Protocol } from "../gen/lotsman/v1/connection_pb";
import { type HistoryResponse, HistoryResponseSchema, SampleSchema } from "../gen/lotsman/v1/history_pb";
import { type Incident, IncidentKind, IncidentSchema } from "../gen/lotsman/v1/incident_pb";
import {
  type PeerStatus,
  PeerStatusSchema,
  type Speed,
  SpeedSchema,
  type StatusSnapshot,
  StatusSnapshotSchema,
  type Traffic,
  TrafficSchema,
  UpstreamKind,
  UpstreamState,
  UpstreamStatusSchema,
} from "../gen/lotsman/v1/status_pb";
import { type Device, DeviceSchema, type Route, type User, UserSchema } from "../gen/lotsman/v1/user_pb";

const MINUTE = 60_000;
const HOUR = 3_600_000;
const DAY = 86_400_000;
const MAX_SAMPLES = 2000;
const MAX_RATE = 6_000_000; // bytes per second, about 48 Mbit/s
/** Speed tests run every half hour, on the half hour (the daemon: health.speed.interval with jitter). */
const SPEED_EVERY = 30 * MINUTE;
/** How long de-1 has been down; its availability, speed tests and incident agree. */
const DOWN_FOR = 6 * HOUR;
/** The fake's cap on connections per update; the daemon's is 500. Low, so the full list shows `truncated`. */
export const CONNECTION_CAP = 25;

interface Upstream {
  name: string;
  kind: UpstreamKind;
  geo: string;
  provider: string;
  state: UpstreamState;
  latencyMs: number;
  /** What its speed tests measure, about: bytes per second each way. */
  speed: { down: number; up: number };
}

/** An incident as the daemon's store keeps it (see Incident in incident.proto). */
interface IncidentRecord {
  id: string;
  upstream: string;
  kind: IncidentKind;
  started: number; // ms since the epoch
  ended: number; // ms since the epoch, 0 = open
  detail: string;
  devices: number;
}

interface Peer {
  id: string;
  user: string;
  device: string;
  route: RouteValue;
  address: string;
  /** Where its route takes it now, among the user's allowed exits; "" = blocked. */
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

/** A route as the store keeps it (see Route in user.proto). */
export interface RouteValue {
  fastest: boolean;
  strict: boolean;
  countries: { geo: string; providers: string[] }[];
}

/** A user's rights (see Rights in user.proto). */
export interface RightsValue {
  editRoutes: boolean;
  showProviders: boolean;
  /** Upstream names; empty = all. */
  exits: string[];
}

interface UserRecord {
  name: string;
  created: number;
  hasLink: boolean;
  rights: RightsValue;
}

const defaultRoute = (): RouteValue => ({ fastest: true, strict: true, countries: [] });
const defaultRights = (): RightsValue => ({ editRoutes: true, showProviders: true, exits: [] });

// The tables below are pure IIFEs: a production build drops the fake, and
// without the annotation it would keep their enum reads and calls.
const upstreams: readonly Upstream[] = /* @__PURE__ */ (() => [
  { name: "lv-1", kind: UpstreamKind.AWG, geo: "LV", provider: "provider-a", state: UpstreamState.UP, latencyMs: 38, speed: { down: 11e6, up: 5e6 } },
  { name: "lv-2", kind: UpstreamKind.OUTLINE, geo: "LV", provider: "provider-b", state: UpstreamState.UP, latencyMs: 52, speed: { down: 8e6, up: 3e6 } },
  { name: "al-1", kind: UpstreamKind.OUTLINE, geo: "AL", provider: "provider-b", state: UpstreamState.UP, latencyMs: 61, speed: { down: 6e6, up: 2.5e6 } },
  // Degraded by an open upload incident: its uploads crawl.
  { name: "nl-1", kind: UpstreamKind.VLESS, geo: "NL", provider: "provider-c", state: UpstreamState.DEGRADED, latencyMs: 140, speed: { down: 3e6, up: 0.1e6 } },
  { name: "de-1", kind: UpstreamKind.AWG, geo: "DE", provider: "provider-a", state: UpstreamState.DOWN, latencyMs: 45, speed: { down: 9e6, up: 4e6 } },
])();

/**
 * The incident log, relative to the simulation's start: nl-1's upload and
 * de-1's outage are open (they are why one is degraded and the other down),
 * and a few closed ones lie in the past.
 */
const incidentSeeds: readonly Omit<IncidentRecord, "id">[] = /* @__PURE__ */ (() => [
  {
    upstream: "nl-1",
    kind: IncidentKind.UPLOAD,
    started: -(2 * HOUR + 14 * MINUTE),
    ended: 0,
    detail: 'upload probe: Post "https://speed.cloudflare.com/__up": context deadline exceeded',
    devices: 1,
  },
  { upstream: "de-1", kind: IncidentKind.DOWN, started: -DOWN_FOR, ended: 0, detail: "probe 192.0.2.1:443: dial tcp 192.0.2.1:443: i/o timeout", devices: 1 },
  {
    upstream: "lv-1",
    kind: IncidentKind.SEND_ERRORS,
    started: -(DAY + 3 * HOUR),
    ended: -(DAY + 3 * HOUR - 7 * MINUTE),
    detail: "Failed to send data packets: write udp4 0.0.0.0:51820->203.0.113.7:51820: sendmmsg: message too long",
    devices: 2,
  },
  {
    upstream: "lv-2",
    kind: IncidentKind.DOWN,
    started: -(2 * DAY + HOUR),
    ended: -(2 * DAY + HOUR - 4 * MINUTE),
    detail: "probe 192.0.2.1:443: dial tcp 192.0.2.1:443: connect: network is unreachable",
    devices: 2,
  },
  {
    upstream: "nl-1",
    kind: IncidentKind.DOWNLOAD,
    started: -(3 * DAY + 5 * HOUR),
    ended: -(3 * DAY + 4 * HOUR + 18 * MINUTE),
    detail: 'download probe: Get "https://speed.cloudflare.com/__down?bytes=65536": unexpected EOF',
    devices: 1,
  },
  {
    upstream: "de-1",
    kind: IncidentKind.UPLOAD,
    started: -5 * DAY,
    ended: -(5 * DAY - HOUR - 20 * MINUTE),
    detail: 'upload probe: Post "https://speed.cloudflare.com/__up": read: connection reset by peer',
    devices: 1,
  },
])();

// alice may do everything; the operator sets bob's routes and lets him use
// two exits; carol sees countries only.
const userSeeds: readonly (readonly [string, RightsValue])[] = [
  ["alice", defaultRights()],
  ["bob", { editRoutes: false, showProviders: true, exits: ["lv-2", "al-1"] }],
  ["carol", { editRoutes: true, showProviders: false, exits: [] }],
];

// Each seeded device with its route and, from it, its exit now.
const peerSeeds: readonly (readonly [string, string, RouteValue])[] = [
  // Latvia, else the Netherlands, else any exit: lv-1.
  ["alice", "phone", { fastest: false, strict: false, countries: [{ geo: "LV", providers: [] }, { geo: "NL", providers: [] }] }],
  // The default: the fastest of any exit, lv-1.
  ["alice", "laptop", defaultRoute()],
  // The faster of provider-b in Latvia (lv-2) and the Netherlands (nl-1, degraded): lv-2.
  ["alice", "tv", { fastest: true, strict: true, countries: [{ geo: "LV", providers: ["provider-b"] }, { geo: "NL", providers: [] }] }],
  ["bob", "phone", { fastest: false, strict: true, countries: [{ geo: "AL", providers: [] }] }], // al-1
  ["bob", "laptop", defaultRoute()], // lv-2, the faster of his two exits
  ["carol", "phone", { fastest: false, strict: true, countries: [{ geo: "DE", providers: [] }] }], // blocked: de-1 is down
];

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
  private flows: Flow[] = [];
  private readonly incidents: IncidentRecord[];
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
    this.incidents = incidentSeeds.map((s, i) => ({
      ...s,
      id: `inc-${i + 1}`,
      started: this.time + s.started,
      ended: s.ended ? this.time + s.ended : 0,
    }));
    for (const [i, [name, rights]] of userSeeds.entries()) {
      const first = peerSeeds.findIndex(([user]) => user === name);
      this.users.set(name, { name, created: this.time - (41 - first * 5) * DAY, hasLink: i === 0, rights: copyRights(rights) });
    }
    this.peers = peerSeeds.map(([user, device, route], i) => {
      const upstream = this.exitFor(user, route);
      return {
        id: `peer-${i + 1}`,
        user,
        device,
        route: copyRoute(route),
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
      };
    });
    this.nextPeer = this.peers.length + 1;
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
   * user's own peers and their allowed exits, without the routed and active
   * counts or the traffic of those exits, which would show other users, and
   * without providers unless the user may see them.
   */
  snapshot(user?: string): StatusSnapshot {
    this.advance();
    const all = user === undefined;
    const peers = all ? this.peers : this.peers.filter((p) => p.user === user);
    const used = this.upstreamsOf(user);
    const providers = all || this.rights(user).showProviders;
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
            provider: providers ? u.provider : "",
            state: u.state,
            latency: u.state === UpstreamState.DOWN ? undefined : durationFromMs(Math.round(u.latencyMs * jitter)),
            lastHandshake: u.kind === UpstreamKind.AWG && u.state !== UpstreamState.DOWN ? timestampFromMs(this.time - 20_000) : undefined,
            routed: all ? routed.length : 0,
            active: all ? routed.filter((p) => this.isActive(p)).length : 0,
            traffic: all ? traffic(t.down, t.up, t.downRate, t.upRate) : undefined,
            // Operator views only, like the daemon's incidents; the user page does not show speed.
            speed: all ? this.latestSpeed(u) : undefined,
            incidents: all ? this.incidents.filter((i) => i.upstream === u.name && !i.ended).map((i) => i.kind) : [],
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

  /**
   * The incident log, of one upstream or (empty) of all: open ones first,
   * then newest, at most `limit` (0: 200), as IncidentService.List answers.
   */
  listIncidents(upstream = "", limit = 0): Incident[] {
    return this.incidents
      .filter((i) => !upstream || i.upstream === upstream)
      .sort((a, b) => Number(!b.ended) - Number(!a.ended) || b.started - a.started)
      .slice(0, limit || 200)
      .map((i) =>
        create(IncidentSchema, {
          id: i.id,
          upstream: i.upstream,
          kind: i.kind,
          started: timestampFromMs(i.started),
          ended: i.ended ? timestampFromMs(i.ended) : undefined,
          detail: i.detail,
          devices: i.devices,
        }),
      );
  }

  /** The upstream's latest speed test that got through, if any. */
  private latestSpeed(u: Upstream): Speed | undefined {
    for (let t = Math.floor(this.time / SPEED_EVERY) * SPEED_EVERY; t > this.time - 7 * DAY; t -= SPEED_EVERY) {
      const s = speedTest(u, t, this.time);
      if (s) return create(SpeedSchema, { downRate: BigInt(s.down), upRate: BigInt(s.up), time: timestampFromMs(t) });
    }
    return undefined;
  }

  /** The user with their rights and devices. */
  user(name: string): User {
    const record = this.users.get(name);
    return create(UserSchema, {
      name,
      hasLink: record?.hasLink ?? false,
      created: record ? timestampFromMs(record.created) : undefined,
      rights: this.rights(name),
      devices: this.peers.filter((p) => p.user === name).map((p) => device(p)),
    });
  }

  /** Every user, in the order they were created. */
  listUsers(): User[] {
    return [...this.users.keys()].map((name) => this.user(name));
  }

  /** Adds a user without devices, with the default rights, as `user add` does. */
  addUser(name: string): User {
    checkNames("user", name);
    if (this.users.has(name)) throw new ConnectError(`user ${name} already exists`, Code.AlreadyExists);
    this.advance();
    this.users.set(name, { name, created: this.time, hasLink: false, rights: defaultRights() });
    return this.user(name);
  }

  /** The user's rights; the defaults for a user the store does not know. */
  rights(user: string): RightsValue {
    return copyRights(this.users.get(user)?.rights ?? defaultRights());
  }

  /** Replaces a user's rights, as `user rights` does, and routes their devices again within the exits now allowed. */
  setRights(user: string, rights: RightsValue): User {
    const record = this.users.get(user);
    if (!record) throw new ConnectError(`no such user: ${user}`, Code.NotFound);
    for (const name of rights.exits) {
      if (!upstreams.some((u) => u.name === name)) throw new ConnectError(`rights: no upstream named ${name}`, Code.InvalidArgument);
    }
    record.rights = { ...copyRights(rights), exits: [...new Set(rights.exits)] };
    for (const p of this.peers.filter((p) => p.user === user)) p.upstream = this.exitFor(user, p.route);
    return this.user(user);
  }

  /**
   * Adds a device with its route (the default when unset), creating the user
   * if needed, as `device add` does. The device is routed at once and never connects.
   */
  addDevice(user: string, device: string, route: RouteValue = defaultRoute()): { peerId: string; config: string } {
    checkNames("user", user, "device", device);
    if (this.hasDevice(user, device)) throw new ConnectError(`device ${user}/${device} already exists`, Code.AlreadyExists);
    this.checkRoute(user, route);
    this.advance();
    if (!this.users.has(user)) this.users.set(user, { name: user, created: this.time, hasLink: false, rights: defaultRights() });
    const peer: Peer = {
      id: `peer-${this.nextPeer++}`,
      user,
      device,
      route: copyRoute(route),
      address: this.freeAddress(),
      upstream: this.exitFor(user, route),
      created: this.time,
      connected: false,
      lastHandshake: 0,
      downRate: 0,
      upRate: 0,
      down: 0,
      up: 0,
    };
    this.peers.push(peer);
    return { peerId: peer.id, config: this.config(peer.id) };
  }

  /** Replaces a device's route, as `device route` does: the device moves to the exit it now picks; its config stays. */
  setRoute(user: string, deviceName: string, route: RouteValue): Device {
    const peer = this.peers.find((p) => p.user === user && p.device === deviceName);
    if (!peer) throw new ConnectError(`no such device: ${user}/${deviceName}`, Code.NotFound);
    this.checkRoute(user, route);
    peer.route = copyRoute(route);
    peer.upstream = this.exitFor(user, route);
    return device(peer);
  }

  /** Removes a device: its config stops working. */
  removeDevice(user: string, device: string): void {
    const gone = this.peers.filter((p) => p.user === user && p.device === device);
    if (gone.length === 0) throw new ConnectError(`no such device: ${user}/${device}`, Code.NotFound);
    this.dropPeers(gone);
  }

  /** Removes the user with every device and the link. */
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

  /** The upstreams `user` may use (their rights' exits; stale names are ignored); every upstream when `user` is unset. */
  upstreamsOf(user?: string): Set<string> {
    return new Set(this.allowedExits(user).map((u) => u.name));
  }

  private allowedExits(user?: string): Upstream[] {
    const exits = user === undefined ? [] : this.rights(user).exits;
    return upstreams.filter((u) => exits.length === 0 || exits.includes(u.name));
  }

  /** Where `route` takes a device of `user` now: see selectExit. */
  private exitFor(user: string, route: RouteValue): string {
    return selectExit(route, this.allowedExits(user));
  }

  /**
   * The daemon's checks of a route being written: every country has an exit
   * the user may use, every provider one in that country, and nothing is listed twice.
   */
  private checkRoute(user: string, route: RouteValue): void {
    const exits = this.allowedExits(user);
    const seen = new Set<string>();
    for (const c of route.countries) {
      const invalid = (why: string) => new ConnectError(`route: ${why}`, Code.InvalidArgument);
      if (seen.has(c.geo)) throw invalid(`country ${c.geo} is listed twice`);
      seen.add(c.geo);
      const inCountry = exits.filter((u) => u.geo === c.geo);
      if (inCountry.length === 0) throw invalid(`no exit in country ${c.geo}`);
      if (new Set(c.providers).size !== c.providers.length) throw invalid(`a provider is listed twice in ${c.geo}`);
      const missing = c.providers.find((p) => !inCountry.some((u) => u.provider === p));
      if (missing !== undefined) throw invalid(`no exit of provider ${missing} in ${c.geo}`);
    }
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

    const now = this.clock();
    const step = historyStep(from, to, now, wanted);
    const routed = upstream ? this.peers.filter((p) => p.upstream === upstream.name).length : 1;
    const samples = [];
    for (let t = Math.ceil(from / step) * step; t <= to; t += step) {
      const h = random(Math.floor(t / MINUTE) * 17 + name.charCodeAt(0))();
      const ratio = upstream ? availability(upstream, t, to) : 0;
      // Bytes moved during the step: about a fifth of the peak rate per peer while up.
      const down = peer && !peer.connected ? 0 : (h * MAX_RATE * 0.4 * routed * (upstream ? ratio : 1) * step) / 1000;
      const speed = upstream ? speedInStep(upstream, t, step, now) : { down: 0, up: 0 };
      samples.push(
        create(SampleSchema, {
          time: timestampFromMs(t),
          upRatio: ratio,
          latency: upstream && ratio > 0 ? durationFromMs(Math.round(upstream.latencyMs * (0.9 + h * 0.2))) : undefined,
          downBytes: BigInt(Math.round(down)),
          upBytes: BigInt(Math.round(down * 0.15)),
          downRate: BigInt(Math.round(speed.down)),
          upRate: BigInt(Math.round(speed.up)),
        }),
      );
    }
    return create(HistoryResponseSchema, { step: durationFromMs(step), samples });
  }

  /** The device names of `user`, in the order they were added. */
  devicesOf(user: string): string[] {
    return this.peers.filter((p) => p.user === user).map((p) => p.device);
  }

  hasDevice(user: string, device: string): boolean {
    return this.peers.some((p) => p.user === user && p.device === device);
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

function device(p: Peer): Device {
  return create(DeviceSchema, { peerId: p.id, name: p.device, address: p.address, created: timestampFromMs(p.created), route: p.route });
}

/** A route as the store keeps it, from the API's message; unset is the default route. */
export function routeValue(route: Route | undefined): RouteValue {
  if (!route) return defaultRoute();
  return { fastest: route.fastest, strict: route.strict, countries: route.countries.map((c) => ({ geo: c.geo, providers: [...c.providers] })) };
}

function copyRoute(r: RouteValue): RouteValue {
  return { fastest: r.fastest, strict: r.strict, countries: r.countries.map((c) => ({ geo: c.geo, providers: [...c.providers] })) };
}

function copyRights(r: RightsValue): RightsValue {
  return { editRoutes: r.editRoutes, showProviders: r.showProviders, exits: [...r.exits] };
}

/** What a tier matches: an upstream with this geo and provider; an empty field matches any. */
interface Match {
  geo: string;
  provider: string;
}

/**
 * The daemon's choice, simplified: the route becomes tiers (each country one
 * in order, or all of them one when fastest; any exit for an empty route, and
 * as a last tier unless strict), and the first tier with an exit up wins,
 * lowest latency first. A degraded exit is the last resort. "" when nothing fits: blocked.
 */
function selectExit(route: RouteValue, candidates: readonly Upstream[]): string {
  const any: Match[] = [{ geo: "", provider: "" }];
  const matches = (c: RouteValue["countries"][number]): Match[] =>
    c.providers.length > 0 ? c.providers.map((provider) => ({ geo: c.geo, provider })) : [{ geo: c.geo, provider: "" }];
  const tiers = route.countries.length === 0 ? [any] : route.fastest ? [route.countries.flatMap(matches)] : route.countries.map(matches);
  if (!route.strict && route.countries.length > 0) tiers.push(any);
  for (const state of [UpstreamState.UP, UpstreamState.DEGRADED]) {
    for (const tier of tiers) {
      const fits = (u: Upstream) => u.state === state && tier.some((m) => (!m.geo || m.geo === u.geo) && (!m.provider || m.provider === u.provider));
      const best = candidates.filter(fits).sort((a, b) => a.latencyMs - b.latencyMs)[0];
      if (best) return best.name;
    }
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

/**
 * The speed test that ran through `u` at `t` (on the half hour), as of `now`:
 * none in the future or while the upstream was down; else near its usual speed.
 */
function speedTest(u: Upstream, t: number, now: number): { down: number; up: number } | undefined {
  if (t % SPEED_EVERY !== 0 || t > now) return undefined;
  if (u.state === UpstreamState.DOWN && now - t < DOWN_FOR) return undefined;
  const h = random(Math.floor(t / MINUTE) * 13 + u.name.charCodeAt(0) + u.name.charCodeAt(3))();
  return { down: Math.round(u.speed.down * (0.8 + h * 0.4)), up: Math.round(u.speed.up * (0.75 + h * 0.5)) };
}

/** The average of the speed tests in the step that starts at `t`, as the daemon samples them; 0 for none. */
function speedInStep(u: Upstream, t: number, step: number, now: number): { down: number; up: number } {
  const tests: { down: number; up: number }[] = [];
  for (let s = Math.ceil(t / SPEED_EVERY) * SPEED_EVERY; s < t + step; s += SPEED_EVERY) {
    const test = speedTest(u, s, now);
    if (test) tests.push(test);
  }
  const mean = (pick: (x: { down: number; up: number }) => number) => tests.reduce((sum, x) => sum + pick(x), 0) / Math.max(tests.length, 1);
  return { down: mean((x) => x.down), up: mean((x) => x.up) };
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
