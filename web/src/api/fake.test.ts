import { durationFromMs, durationMs, timestampFromMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ConnectionService } from "../gen/lotsman/v1/connection_pb";
import { HistoryService } from "../gen/lotsman/v1/history_pb";
import { IncidentKind, IncidentService } from "../gen/lotsman/v1/incident_pb";
import { StatusService } from "../gen/lotsman/v1/status_pb";
import { UserService } from "../gen/lotsman/v1/user_pb";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { createFakeTransport, FAKE_USER } from "./fake";
import { CONNECTION_CAP, historyStep } from "./simulation";

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

beforeEach(() => {
  vi.useFakeTimers({ now: new Date("2026-11-19T12:00:00Z") });
});
afterEach(() => {
  vi.useRealTimers();
});

/** A history request for the `days` before now. */
function range(name: string, days: number, stepMs?: number) {
  const to = Date.now();
  return {
    name,
    from: timestampFromMs(to - days * DAY),
    to: timestampFromMs(to),
    step: stepMs === undefined ? undefined : durationFromMs(stepMs),
  };
}

const notFound = (e: unknown) => ConnectError.from(e).code === Code.NotFound;
const code = (c: Code) => (e: unknown) => ConnectError.from(e).code === c;

async function next<T>(stream: AsyncIterator<T>): Promise<T> {
  const result = await stream.next();
  if (result.done) throw new Error("the stream ended");
  return result.value;
}

test("StatusService.Watch sends a snapshot now and every second, until cancelled", async () => {
  const status = createClient(StatusService, createFakeTransport({ seed: 1 }));
  const stream = status.watch({})[Symbol.asyncIterator]();

  const first = await next(stream);
  expect(first.upstreams.map((u) => u.name)).toEqual(["lv-1", "lv-2", "al-1", "nl-1", "de-1"]);
  expect(first.peers).toHaveLength(6);

  const pending = next(stream);
  await vi.advanceTimersByTimeAsync(1000);
  const second = await pending;
  expect(Number((second.time?.seconds ?? 0n) - (first.time?.seconds ?? 0n))).toBe(1);
  expect(second.total?.downBytes).toBeGreaterThan(first.total?.downBytes ?? 0n);

  await stream.return?.();
});

test("the same seed and clock give the same data", async () => {
  const first = async () => {
    for await (const s of createClient(StatusService, createFakeTransport({ seed: 3 })).watch({})) return s;
  };
  expect(await first()).toEqual(await first());
});

test("UserSelfService.Watch carries only the user's peers and no other users' traffic", async () => {
  const self = createClient(UserSelfService, createFakeTransport());
  for await (const s of self.watch({})) {
    expect(new Set(s.peers.map((p) => p.user))).toEqual(new Set([FAKE_USER]));
    // Every exit alice may use, whether her devices use it or not.
    expect(s.upstreams.map((u) => u.name)).toEqual(["lv-1", "lv-2", "al-1", "nl-1", "de-1"]);
    expect(s.upstreams.every((u) => u.traffic === undefined && u.routed === 0 && u.active === 0)).toBe(true);
    break;
  }
  const user = await self.get({});
  expect(user.devices.map((d) => d.name)).toEqual(["phone", "laptop", "tv"]);
});

test("HistoryService.Upstream returns samples at the step asked for", async () => {
  const history = createClient(HistoryService, createFakeTransport());
  const res = await history.upstream(range("de-1", 7, HOUR));
  expect(durationMs(res.step!)).toBe(HOUR);
  expect(res.samples).toHaveLength(7 * 24 + 1);
  expect(res.samples.at(-1)?.upRatio).toBe(0); // de-1 is down now
  expect(res.samples.at(-1)?.latency).toBeUndefined();
  expect(res.samples.every((s) => s.downBytes === 0n)).toBe(true); // nobody is routed through it

  const lv = await history.upstream(range("lv-1", 7, HOUR));
  expect(lv.samples.some((s) => s.downBytes > 0n && s.latency !== undefined)).toBe(true);
});

test("the history step is rounded up to what is kept and to at most 2000 samples", async () => {
  const now = Date.now();
  const step = (days: number, wanted?: number) => historyStep(now - days * DAY, now, now, wanted);
  expect(step(1)).toBe(MINUTE); // unset: the finest kept
  expect(step(1, 90_000)).toBe(2 * MINUTE);
  expect(step(7)).toBe(6 * MINUTE); // 10080 minutes in at most 2000 samples
  expect(step(30)).toBe(HOUR); // older than 7 days: hourly samples only
  expect(step(30, MINUTE)).toBe(HOUR);
  expect(step(90, HOUR)).toBe(2 * HOUR); // 2161 hourly samples would be too many

  const history = createClient(HistoryService, createFakeTransport());
  const res = await history.upstream(range("lv-1", 90));
  expect(durationMs(res.step!)).toBe(2 * HOUR);
  expect(res.samples.length).toBeLessThanOrEqual(2000);
});

test("HistoryService answers NotFound for an unknown upstream or peer", async () => {
  const history = createClient(HistoryService, createFakeTransport());
  await expect(history.upstream(range("xx-1", 1))).rejects.toSatisfy(notFound);
  await expect(history.peer(range("peer-99", 1))).rejects.toSatisfy(notFound);
  const peer = await history.peer(range("peer-4", 1, HOUR));
  expect(peer.samples.every((s) => s.upRatio === 0 && s.latency === undefined)).toBe(true);
  expect(peer.samples[0]?.downBytes).toBeGreaterThan(0n);
});

test("UserSelfService history covers only the user's exits and peers, without upstream bytes", async () => {
  const self = createClient(UserSelfService, createFakeTransport());
  const upstream = await self.upstreamHistory(range("lv-1", 7, HOUR));
  expect(upstream.samples).toHaveLength(7 * 24 + 1);
  expect(upstream.samples.every((s) => s.downBytes === 0n && s.upBytes === 0n)).toBe(true);
  expect(upstream.samples.some((s) => s.latency !== undefined && s.upRatio > 0)).toBe(true);
  await expect(self.upstreamHistory(range("xx-1", 7, HOUR))).rejects.toSatisfy(notFound);

  const peer = await self.peerHistory(range("peer-1", 1, HOUR));
  expect(peer.samples[0]?.downBytes).toBeGreaterThan(0n);
  await expect(self.peerHistory(range("peer-4", 1, HOUR))).rejects.toSatisfy(notFound); // bob's
});

test("services without a fake answer Unimplemented", async () => {
  const self = createClient(UserSelfService, createFakeTransport());
  await expect(self.resolveLink({ token: "x" })).rejects.toSatisfy(code(Code.Unimplemented));
});

test("operator services answer Unauthenticated while signed out", async () => {
  const transport = createFakeTransport({ signedIn: () => false });
  await expect(createClient(UserService, transport).addUser({ user: "eve" })).rejects.toSatisfy(code(Code.Unauthenticated));
  await expect(createClient(UserService, transport).list({})).rejects.toSatisfy(code(Code.Unauthenticated));
  await expect(createClient(IncidentService, transport).list({})).rejects.toSatisfy(code(Code.Unauthenticated));
  const stream = createClient(ConnectionService, transport).watch({})[Symbol.asyncIterator]();
  await expect(stream.next()).rejects.toSatisfy(code(Code.Unauthenticated));
});

/** Where each of the user's devices goes out now. */
async function exits(transport: ReturnType<typeof createFakeTransport>, user: string): Promise<Record<string, string>> {
  for await (const s of createClient(StatusService, transport).watch({})) {
    return Object.fromEntries(s.peers.filter((p) => p.user === user).map((p) => [p.device, p.upstream]));
  }
  throw new Error("no snapshot");
}

test("UserService lists users with their rights, devices and routes", async () => {
  const transport = createFakeTransport();
  const { users } = await createClient(UserService, transport).list({});
  expect(users.map((u) => [u.name, u.hasLink, u.devices.map((d) => d.name)])).toEqual([
    ["alice", true, ["phone", "laptop", "tv"]],
    ["bob", false, ["phone", "laptop"]],
    ["carol", false, ["phone"]],
  ]);
  expect(users.map((u) => [u.rights?.editRoutes, u.rights?.showProviders, u.rights?.exits])).toEqual([
    [true, true, []],
    [false, true, ["lv-2", "al-1"]],
    [true, false, []],
  ]);
  const phone = users[0]!.devices[0]!;
  expect([phone.peerId, phone.address]).toEqual(["peer-1", "10.8.0.2"]);
  expect(phone.route).toMatchObject({ fastest: false, strict: false, countries: [{ geo: "LV" }, { geo: "NL" }] });
  expect(users[0]!.devices[1]!.route).toMatchObject({ fastest: true, strict: true, countries: [] });
  expect(users.every((u) => u.created !== undefined)).toBe(true);

  // Each route picks among the user's allowed exits: the first country up, or the fastest.
  expect(await exits(transport, "alice")).toEqual({ phone: "lv-1", laptop: "lv-1", tv: "lv-2" });
  expect(await exits(transport, "bob")).toEqual({ phone: "al-1", laptop: "lv-2" });
  expect(await exits(transport, "carol")).toEqual({ phone: "" }); // strict, and Germany is down
});

test("UserService.AddUser adds a user with the default rights, once", async () => {
  const users = createClient(UserService, createFakeTransport());
  const eve = await users.addUser({ user: "eve" });
  expect(eve).toMatchObject({ name: "eve", devices: [], hasLink: false, rights: { editRoutes: true, showProviders: true, exits: [] } });
  await expect(users.addUser({ user: "eve" })).rejects.toSatisfy(code(Code.AlreadyExists));
  await expect(users.addUser({ user: "" })).rejects.toSatisfy(code(Code.InvalidArgument));
  expect((await users.list({})).users.map((u) => u.name)).toEqual(["alice", "bob", "carol", "eve"]);
});

test("UserService.SetRights narrows the exits and routes the user's devices again", async () => {
  const transport = createFakeTransport();
  const users = createClient(UserService, transport);
  const alice = await users.setRights({ user: "alice", rights: { editRoutes: false, showProviders: true, exits: ["nl-1", "al-1"] } });
  expect(alice.rights).toMatchObject({ editRoutes: false, exits: ["nl-1", "al-1"] });
  // phone: Latvia is not allowed now, the Netherlands is degraded, and it may fall back: al-1 is up.
  // tv: strict to Latvia's provider-b and the Netherlands: only the degraded nl-1 is left.
  expect(await exits(transport, "alice")).toEqual({ phone: "al-1", laptop: "al-1", tv: "nl-1" });

  await expect(users.setRights({ user: "alice", rights: { exits: ["xx-1"] } })).rejects.toSatisfy(code(Code.InvalidArgument));
  await expect(users.setRights({ user: "nobody", rights: {} })).rejects.toSatisfy(notFound);
});

test("UserService.AddDevice creates the user, routes the device and returns its config", async () => {
  const transport = createFakeTransport();
  const users = createClient(UserService, transport);
  const added = await users.addDevice({ user: "dave", device: "phone" });
  expect(added.peerId).toBe("peer-7");
  expect(added.config).toMatch(/^\[Interface\]\nPrivateKey = [A-Za-z0-9+/]{43}=\nAddress = 10\.8\.0\.8\/32\n/);
  expect(added.config).toContain("Endpoint = vpn.example.com:51820");
  expect((await users.getConfig({ peerId: "peer-7" })).config).toBe(added.config);

  const dave = (await users.list({})).users.find((u) => u.name === "dave");
  expect(dave?.devices.map((d) => [d.name, d.route?.fastest, d.route?.strict, d.route?.countries])).toEqual([["phone", true, true, []]]);
  await users.addDevice({ user: "dave", device: "tv", route: { strict: true, countries: [{ geo: "NL" }] } });
  expect(await exits(transport, "dave")).toEqual({ phone: "lv-1", tv: "nl-1" }); // the fastest, and the Netherlands
});

test("UserService refuses bad names, duplicates and routes that name what does not exist", async () => {
  const users = createClient(UserService, createFakeTransport());
  await expect(users.addDevice({ user: "", device: "phone" })).rejects.toSatisfy(code(Code.InvalidArgument));
  await expect(users.addDevice({ user: " dave", device: "phone" })).rejects.toSatisfy(code(Code.InvalidArgument));
  await expect(users.addDevice({ user: "dave", device: "" })).rejects.toSatisfy(code(Code.InvalidArgument));
  await expect(users.addDevice({ user: "alice", device: "phone" })).rejects.toSatisfy(code(Code.AlreadyExists));
  const route = (...countries: { geo: string; providers?: string[] }[]) => ({ user: "alice", device: "phone", route: { countries } });
  await expect(users.setRoute(route({ geo: "FI" }))).rejects.toThrow(/no exit in country FI/);
  await expect(users.setRoute(route({ geo: "LV", providers: ["provider-c"] }))).rejects.toThrow(/no exit of provider provider-c in LV/);
  await expect(users.setRoute(route({ geo: "LV" }, { geo: "LV" }))).rejects.toThrow(/country LV is listed twice/);
  await expect(users.setRoute(route({ geo: "LV", providers: ["provider-a", "provider-a"] }))).rejects.toThrow(/listed twice in LV/);
  // bob may use lv-2 and al-1 only: provider-a in Latvia is not his to name.
  await expect(users.setRoute({ user: "bob", device: "phone", route: { countries: [{ geo: "LV", providers: ["provider-a"] }] } })).rejects.toSatisfy(
    code(Code.InvalidArgument),
  );
  await expect(users.setRoute({ user: "bob", device: "nope", route: {} })).rejects.toSatisfy(notFound);
  expect((await users.list({})).users.map((u) => u.name)).toEqual(["alice", "bob", "carol"]);
});

test("UserService.SetRoute moves the device and keeps its config", async () => {
  const transport = createFakeTransport();
  const users = createClient(UserService, transport);
  const before = (await users.getConfig({ peerId: "peer-1" })).config;
  const device = await users.setRoute({ user: "alice", device: "phone", route: { fastest: false, strict: true, countries: [{ geo: "AL" }] } });
  expect(device).toMatchObject({ peerId: "peer-1", name: "phone", route: { strict: true, countries: [{ geo: "AL", providers: [] }] } });
  expect((await exits(transport, "alice")).phone).toBe("al-1");
  expect((await users.getConfig({ peerId: "peer-1" })).config).toBe(before);
});

test("UserService removes devices and users", async () => {
  const users = createClient(UserService, createFakeTransport());
  const bob = async () => (await users.list({})).users.find((u) => u.name === "bob");
  await users.removeDevice({ user: "bob", device: "laptop" });
  expect((await bob())?.devices.map((d) => d.name)).toEqual(["phone"]);
  await expect(users.removeDevice({ user: "bob", device: "laptop" })).rejects.toSatisfy(notFound);

  await users.removeUser({ user: "bob" });
  expect(await bob()).toBeUndefined();
  await expect(users.removeUser({ user: "bob" })).rejects.toSatisfy(notFound);
  await expect(users.getConfig({ peerId: "peer-4" })).rejects.toSatisfy(notFound);
});

test("UserService issues, rotates and revokes links", async () => {
  const users = createClient(UserService, createFakeTransport());
  const first = await users.issueLink({ user: "bob" });
  expect(first.token).toMatch(/^[\w-]{43}$/);
  const second = await users.issueLink({ user: "bob" });
  expect(second.token).not.toBe(first.token);
  const hasLink = async () => (await users.list({})).users.find((u) => u.name === "bob")?.hasLink;
  expect(await hasLink()).toBe(true);
  await users.revokeLink({ user: "bob" });
  expect(await hasLink()).toBe(false);
  await expect(users.issueLink({ user: "nobody" })).rejects.toSatisfy(notFound);
  await expect(users.revokeLink({ user: "nobody" })).rejects.toSatisfy(notFound);
});

test("ConnectionService.Watch streams the fastest connections, rates from the second snapshot", async () => {
  const connections = createClient(ConnectionService, createFakeTransport());
  const stream = connections.watch({})[Symbol.asyncIterator]();
  const first = await next(stream);
  expect(first.connections).toHaveLength(CONNECTION_CAP);
  expect(first.truncated).toBe(true);
  expect(first.connections.every((c) => c.traffic?.downRate === 0n && c.traffic.upRate === 0n)).toBe(true);
  expect(first.connections.every((c) => c.lastTraffic === undefined)).toBe(true);
  expect(first.connections.some((c) => (c.traffic?.downBytes ?? 0n) > 0n)).toBe(true);

  const pending = next(stream);
  await vi.advanceTimersByTimeAsync(1000);
  const second = await pending;
  const rates = second.connections.map((c) => Number((c.traffic?.downRate ?? 0n) + (c.traffic?.upRate ?? 0n)));
  expect(rates.some((r) => r > 0)).toBe(true);
  expect(second.connections.some((c) => c.lastTraffic !== undefined)).toBe(true);
  expect(rates).toEqual([...rates].sort((a, b) => b - a));
  expect(new Set(second.connections.map((c) => c.upstream))).not.toContain("");
  await stream.return?.();
});

test("ConnectionService.Watch filters by peer and refuses an unknown one", async () => {
  const connections = createClient(ConnectionService, createFakeTransport());
  for await (const s of connections.watch({ peerId: "peer-1" })) {
    expect(s.connections.length).toBeGreaterThan(0);
    expect(s.truncated).toBe(false);
    expect(s.connections.every((c) => c.peerId === "peer-1" && c.source.startsWith("10.8.0.2") && c.upstream === "lv-1")).toBe(true);
    break;
  }
  const stream = connections.watch({ peerId: "peer-99" })[Symbol.asyncIterator]();
  await expect(stream.next()).rejects.toSatisfy(notFound);
});

test("IncidentService.List: open ones first, then newest; by upstream and limit", async () => {
  const incidents = createClient(IncidentService, createFakeTransport());
  const all = (await incidents.list({})).incidents;
  const now = Date.now();
  expect(all.map((i) => [i.upstream, i.kind, i.ended === undefined])).toEqual([
    ["nl-1", IncidentKind.UPLOAD, true],
    ["de-1", IncidentKind.DOWN, true],
    ["lv-1", IncidentKind.SEND_ERRORS, false],
    ["lv-2", IncidentKind.DOWN, false],
    ["nl-1", IncidentKind.DOWNLOAD, false],
    ["de-1", IncidentKind.UPLOAD, false],
  ]);
  expect(timestampMs(all[0]!.started!)).toBe(now - 2 * HOUR - 14 * MINUTE);
  expect(new Set(all.map((i) => i.id)).size).toBe(all.length);
  for (const i of all.filter((i) => i.ended)) expect(timestampMs(i.ended!)).toBeGreaterThan(timestampMs(i.started!));

  const nl = (await incidents.list({ upstream: "nl-1" })).incidents;
  expect(nl.map((i) => i.kind)).toEqual([IncidentKind.UPLOAD, IncidentKind.DOWNLOAD]);
  expect((await incidents.list({ limit: 3 })).incidents).toHaveLength(3);
  expect((await incidents.list({ upstream: "xx-1" })).incidents).toEqual([]);
});

test("StatusService.Watch carries the open incident kinds and the latest speed; the user's Watch neither", async () => {
  const transport = createFakeTransport();
  for await (const s of createClient(StatusService, transport).watch({})) {
    expect(Object.fromEntries(s.upstreams.map((u) => [u.name, u.incidents]))).toEqual({
      "lv-1": [],
      "lv-2": [],
      "al-1": [],
      "nl-1": [IncidentKind.UPLOAD],
      "de-1": [IncidentKind.DOWN],
    });
    // On the half hour; de-1's last one got through before it went down 6 hours ago.
    const lv = s.upstreams[0]!.speed!;
    expect(lv.downRate).toBeGreaterThan(0n);
    expect(lv.upRate).toBeGreaterThan(0n);
    expect(timestampMs(lv.time!)).toBe(Math.floor(Date.now() / (30 * MINUTE)) * 30 * MINUTE);
    expect(timestampMs(s.upstreams[4]!.speed!.time!)).toBeLessThanOrEqual(Date.now() - 6 * HOUR);
    break;
  }
  for await (const s of createClient(UserSelfService, transport).watch({})) {
    expect(s.upstreams.every((u) => u.incidents.length === 0 && u.speed === undefined)).toBe(true);
    break;
  }
});

test("HistoryService.Upstream averages the speed tests per step, 0 where none ran", async () => {
  const history = createClient(HistoryService, createFakeTransport());
  // Tests run on the half hour: one in six 5-minute steps, every hourly one.
  const fine = await history.upstream(range("lv-1", 1, 5 * MINUTE));
  const tested = fine.samples.filter((s) => s.downRate > 0n);
  expect(tested.length).toBe(49); // 12:00 the day before to 12:00 now, both included
  expect(tested.every((s) => timestampMs(s.time!) % (30 * MINUTE) === 0 && s.upRate > 0n)).toBe(true);
  const hourly = await history.upstream(range("lv-1", 7, HOUR));
  expect(hourly.samples.slice(0, -1).every((s) => s.downRate > 0n)).toBe(true);
  // de-1 has been down for 6 hours: no test got through.
  const de = await history.upstream(range("de-1", 1, HOUR));
  expect(de.samples.slice(-6).every((s) => s.downRate === 0n)).toBe(true);
  // A peer's history and the user's carry no speed.
  expect((await history.peer(range("peer-1", 1, HOUR))).samples.every((s) => s.downRate === 0n)).toBe(true);
  const self = await createClient(UserSelfService, createFakeTransport()).upstreamHistory(range("lv-1", 1, HOUR));
  expect(self.samples.every((s) => s.downRate === 0n && s.upRate === 0n)).toBe(true);
});
