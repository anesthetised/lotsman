import { durationMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { StatusSnapshot } from "../gen/lotsman/v1/status_pb";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { createFakeTransport, FAKE_LINKS, withToken } from "./fake";

beforeEach(() => {
  vi.useFakeTimers({ now: new Date("2026-11-19T12:00:00Z") });
});
afterEach(() => {
  vi.useRealTimers();
});

const selfFor = (link: string, options?: Parameters<typeof createFakeTransport>[0]) =>
  createClient(UserSelfService, withToken(createFakeTransport({ seed: 1, ...options }), link));

const codeOf = async (call: Promise<unknown>) => ConnectError.from(await call.catch((e) => e)).code;

async function snapshotOf(self: ReturnType<typeof selfFor>): Promise<StatusSnapshot> {
  const stream = self.watch({})[Symbol.asyncIterator]();
  const snapshot: StatusSnapshot = (await stream.next()).value;
  await stream.return?.();
  return snapshot;
}

const range = (name: string) => {
  const to = Date.now();
  return { name, from: timestampFromMs(to - 3_600_000), to: timestampFromMs(to) };
};

test("devices, rights and configs of the ordinary user", async () => {
  const self = selfFor(FAKE_LINKS.ordinary);
  const user = await self.get({});
  expect(user.devices.map((d) => d.name)).toEqual(["phone", "laptop", "tv"]);
  expect(user.rights).toMatchObject({ editRoutes: true, showProviders: true, exits: [] });

  const peerId = user.devices[0]!.peerId;
  const config = (await self.getConfig({ peerId })).config;
  expect(config).toMatch(/^\[Interface\]\nPrivateKey = [A-Za-z0-9+/]{43}=\n/);
  expect(config.length).toBeGreaterThan(400);
  expect((await self.getConfig({ peerId })).config).toBe(config); // deterministic
});

test("another user's peer and a missing one are both NotFound", async () => {
  const self = selfFor(FAKE_LINKS.ordinary);
  expect(await codeOf(self.getConfig({ peerId: "peer-4" }))).toBe(Code.NotFound); // bob's
  expect(await codeOf(self.getConfig({ peerId: "peer-99" }))).toBe(Code.NotFound);
});

test("AddDevice returns the config; the device is routed by its route, or the default one", async () => {
  const self = selfFor(FAKE_LINKS.ordinary);
  expect(await codeOf(self.addDevice({ device: "x", route: { countries: [{ geo: "FI" }] } }))).toBe(Code.InvalidArgument);
  const added = await self.addDevice({ device: "tablet", route: { strict: true, countries: [{ geo: "AL" }] } });
  expect(added.config).toContain("[Interface]");
  expect((await self.getConfig({ peerId: added.peerId })).config).toBe(added.config);
  const plain = await self.addDevice({ device: "watch" });

  const snapshot = await snapshotOf(self);
  expect(snapshot.peers.find((p) => p.peerId === added.peerId)).toMatchObject({ device: "tablet", upstream: "al-1" });
  expect(snapshot.peers.find((p) => p.peerId === plain.peerId)?.upstream).toBe("lv-1");
  const tablet = (await self.get({})).devices.find((d) => d.name === "tablet");
  expect(tablet?.route).toMatchObject({ strict: true, countries: [{ geo: "AL" }] });

  // A new device has no history yet, but its owner may ask.
  const history = await self.peerHistory(range(added.peerId));
  expect(durationMs(history.step!)).toBe(60_000);

  expect(await codeOf(self.addDevice({ device: "tablet" }))).toBe(Code.AlreadyExists);
  expect(await codeOf(self.addDevice({ device: " x" }))).toBe(Code.InvalidArgument);
  expect(await codeOf(self.addDevice({ device: "" }))).toBe(Code.InvalidArgument);
});

test("SetRoute moves a device to the exit its new route picks", async () => {
  const self = selfFor(FAKE_LINKS.ordinary);
  const device = await self.setRoute({ device: "phone", route: { fastest: true, strict: true, countries: [{ geo: "AL" }, { geo: "NL" }] } });
  expect(device.route?.countries.map((c) => c.geo)).toEqual(["AL", "NL"]);
  expect((await snapshotOf(self)).peers.find((p) => p.device === "phone")?.upstream).toBe("al-1");
  expect(await codeOf(self.setRoute({ device: "nope", route: {} }))).toBe(Code.NotFound);
  expect(await codeOf(self.setRoute({ device: "phone", route: { countries: [{ geo: "LV", providers: ["nope"] }] } }))).toBe(Code.InvalidArgument);
});

test("without edit_routes a user keeps the operator's routes: SetRoute and a route on AddDevice are PermissionDenied", async () => {
  const bob = selfFor(FAKE_LINKS.fixedRoutes);
  expect((await bob.get({})).rights).toMatchObject({ editRoutes: false, exits: ["lv-2", "al-1"] });
  expect(await codeOf(bob.setRoute({ device: "phone", route: { countries: [{ geo: "LV" }] } }))).toBe(Code.PermissionDenied);
  expect(await codeOf(bob.addDevice({ device: "tablet", route: { countries: [{ geo: "LV" }] } }))).toBe(Code.PermissionDenied);
  await bob.addDevice({ device: "tablet" }); // the default route: the fastest of his exits
  const snapshot = await snapshotOf(bob);
  expect(snapshot.peers.find((p) => p.device === "tablet")?.upstream).toBe("lv-2");
});

test("only the allowed exits reach the user: in Watch, in history, in routes", async () => {
  const bob = selfFor(FAKE_LINKS.fixedRoutes);
  const snapshot = await snapshotOf(bob);
  expect(snapshot.upstreams.map((u) => [u.name, u.geo, u.provider])).toEqual([
    ["lv-2", "LV", "provider-b"],
    ["al-1", "AL", "provider-b"],
  ]);
  await bob.upstreamHistory(range("al-1"));
  expect(await codeOf(bob.upstreamHistory(range("nl-1")))).toBe(Code.NotFound);
});

test("without show_providers the snapshot names no provider and a route may not name one", async () => {
  const carol = selfFor(FAKE_LINKS.countriesOnly);
  const snapshot = await snapshotOf(carol);
  expect(snapshot.upstreams.map((u) => u.geo)).toEqual(["LV", "LV", "AL", "NL", "DE"]);
  expect(snapshot.upstreams.every((u) => u.provider === "")).toBe(true);
  // Her only device is strict to Germany, which is down: blocked.
  expect(snapshot.peers.map((p) => [p.device, p.upstream])).toEqual([["phone", ""]]);

  expect(await codeOf(carol.setRoute({ device: "phone", route: { countries: [{ geo: "LV", providers: ["provider-a"] }] } }))).toBe(Code.InvalidArgument);
  await carol.setRoute({ device: "phone", route: { strict: true, countries: [{ geo: "DE" }, { geo: "NL" }] } });
  expect((await snapshotOf(carol)).peers[0]?.upstream).toBe("nl-1");
});

test("RemoveDevice removes the device", async () => {
  const self = selfFor(FAKE_LINKS.ordinary);
  await self.removeDevice({ device: "phone" });
  expect((await self.get({})).devices.map((d) => d.name)).toEqual(["laptop", "tv"]);
  expect(await codeOf(self.removeDevice({ device: "phone" }))).toBe(Code.NotFound);
  expect(await codeOf(self.removeDevice({ device: "" }))).toBe(Code.InvalidArgument);
});

test("max_devices: the limit counts devices, and removing one lifts it", async () => {
  const self = selfFor(FAKE_LINKS.ordinary, { self: { maxDevices: 4 } });
  await self.addDevice({ device: "d4" });
  const refused = await self.addDevice({ device: "d5" }).catch((e) => ConnectError.from(e));
  expect(refused).toMatchObject({ code: Code.ResourceExhausted, rawMessage: "at most 4 devices; remove one first or ask the operator" });
  await self.removeDevice({ device: "d4" });
  await self.addDevice({ device: "d5" });
});

test("max_devices 0 refuses every addition with permission_denied, but removing still works", async () => {
  const self = selfFor(FAKE_LINKS.noAdding);
  expect(await codeOf(self.addDevice({ device: "new" }))).toBe(Code.PermissionDenied);
  await self.removeDevice({ device: "tv" });
});

test("the rate limit allows a burst, then one addition every 6 minutes", async () => {
  const self = selfFor(FAKE_LINKS.ordinary, { self: { maxDevices: 100, addBurst: 3 } });
  for (const device of ["a", "b", "c"]) await self.addDevice({ device });
  const refused = await self.addDevice({ device: "d" }).catch((e) => ConnectError.from(e));
  expect(refused).toMatchObject({ code: Code.ResourceExhausted, rawMessage: "too many devices added lately; try again in a few minutes" });
  vi.advanceTimersByTime(6 * 60_000);
  await self.addDevice({ device: "d" });
  expect(await codeOf(self.addDevice({ device: "e" }))).toBe(Code.ResourceExhausted);
});

test("scenario links: revoked is Unauthenticated everywhere, unavailable is Unavailable, empty has no devices", async () => {
  const revoked = selfFor(FAKE_LINKS.revoked);
  expect(await codeOf(revoked.get({}))).toBe(Code.Unauthenticated);
  expect(await codeOf(revoked.addDevice({ device: "x" }))).toBe(Code.Unauthenticated);
  expect(await codeOf(revoked.setRoute({ device: "phone", route: {} }))).toBe(Code.Unauthenticated);
  expect(await codeOf(revoked.watch({})[Symbol.asyncIterator]().next())).toBe(Code.Unauthenticated);
  expect(await codeOf(selfFor(FAKE_LINKS.unavailable).get({}))).toBe(Code.Unavailable);

  const empty = selfFor(FAKE_LINKS.empty);
  expect(await empty.get({})).toMatchObject({ name: "dave", devices: [] });
  const snapshot = await snapshotOf(empty);
  expect(snapshot.peers).toEqual([]);
  expect(snapshot.upstreams).toHaveLength(5); // what a first device may use
});
