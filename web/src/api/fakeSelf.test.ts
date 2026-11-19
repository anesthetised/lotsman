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

test("devices, profiles and configs of the ordinary user", async () => {
  const self = selfFor(FAKE_LINKS.ordinary);
  const user = await self.get({});
  expect(user.devices.map((d) => [d.name, d.peers.map((p) => p.profile)])).toEqual([
    ["phone", ["default"]],
    ["laptop", ["default"]],
    ["tv", ["streaming"]],
  ]);
  expect((await self.listProfiles({})).profiles.map((p) => [p.name, p.prefer.length])).toEqual([
    ["default", 0],
    ["streaming", 0],
    ["backup", 0],
    ["fi", 0],
  ]);

  const peerId = user.devices[0]!.peers[0]!.peerId;
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

test("AddDevice returns the config and the device appears; a second profile is the same device", async () => {
  const self = selfFor(FAKE_LINKS.ordinary);
  const added = await self.addDevice({ device: "tablet", profile: "streaming" });
  expect(added.config).toContain("[Interface]");
  expect((await self.getConfig({ peerId: added.peerId })).config).toBe(added.config);

  await self.addDevice({ device: "tablet", profile: "default" });
  const tablet = (await self.get({})).devices.find((d) => d.name === "tablet");
  expect(tablet?.peers.map((p) => p.profile)).toEqual(["streaming", "default"]);

  expect(await codeOf(self.addDevice({ device: "tablet", profile: "default" }))).toBe(Code.AlreadyExists);
  expect(await codeOf(self.addDevice({ device: "x", profile: "nope" }))).toBe(Code.InvalidArgument);
  expect(await codeOf(self.addDevice({ device: " x", profile: "default" }))).toBe(Code.InvalidArgument);
  expect(await codeOf(self.addDevice({ device: "", profile: "default" }))).toBe(Code.InvalidArgument);
});

test("a new device is routed through its profile's exit and has no history yet but is visible to its owner", async () => {
  const self = selfFor(FAKE_LINKS.ordinary);
  const added = await self.addDevice({ device: "tablet", profile: "backup" });
  const stream = self.watch({})[Symbol.asyncIterator]();
  const snapshot: StatusSnapshot = (await stream.next()).value;
  expect(snapshot.peers.find((p) => p.peerId === added.peerId)).toMatchObject({ device: "tablet", profile: "backup", upstream: "al-1" });
  const to = Date.now();
  const history = await self.peerHistory({ name: added.peerId, from: timestampFromMs(to - 3_600_000), to: timestampFromMs(to) });
  expect(durationMs(history.step!)).toBe(60_000);
  await stream.return?.();
});

test("RemoveDevice removes one profile or the whole device", async () => {
  const self = selfFor(FAKE_LINKS.ordinary);
  await self.addDevice({ device: "phone", profile: "streaming" });
  await self.removeDevice({ device: "phone", profile: "default" });
  expect((await self.get({})).devices.find((d) => d.name === "phone")?.peers.map((p) => p.profile)).toEqual(["streaming"]);
  await self.removeDevice({ device: "phone", profile: "" });
  expect((await self.get({})).devices.map((d) => d.name)).toEqual(["laptop", "tv"]);
  expect(await codeOf(self.removeDevice({ device: "phone", profile: "" }))).toBe(Code.NotFound);
  expect(await codeOf(self.removeDevice({ device: "", profile: "" }))).toBe(Code.InvalidArgument);
});

test("max_devices: the limit counts devices, not profiles, and removing one lifts it", async () => {
  const self = selfFor(FAKE_LINKS.ordinary, { self: { maxDevices: 4 } });
  await self.addDevice({ device: "d4", profile: "default" });
  const refused = await self.addDevice({ device: "d5", profile: "default" }).catch((e) => ConnectError.from(e));
  expect(refused).toMatchObject({ code: Code.ResourceExhausted, rawMessage: "at most 4 devices; remove one first or ask the operator" });
  await self.addDevice({ device: "phone", profile: "streaming" }); // another profile of a device they have
  await self.removeDevice({ device: "d4", profile: "" });
  await self.addDevice({ device: "d5", profile: "default" });
});

test("max_devices 0 refuses every addition with permission_denied, but removing still works", async () => {
  const self = selfFor(FAKE_LINKS.noAdding);
  expect(await codeOf(self.addDevice({ device: "new", profile: "default" }))).toBe(Code.PermissionDenied);
  expect(await codeOf(self.addDevice({ device: "phone", profile: "streaming" }))).toBe(Code.PermissionDenied);
  await self.removeDevice({ device: "tv", profile: "" });
});

test("the rate limit allows a burst, then one addition every 6 minutes", async () => {
  const self = selfFor(FAKE_LINKS.ordinary, { self: { maxDevices: 100, addBurst: 3 } });
  for (const device of ["a", "b", "c"]) await self.addDevice({ device, profile: "default" });
  const refused = await self.addDevice({ device: "d", profile: "default" }).catch((e) => ConnectError.from(e));
  expect(refused).toMatchObject({ code: Code.ResourceExhausted, rawMessage: "too many devices added lately; try again in a few minutes" });
  vi.advanceTimersByTime(6 * 60_000);
  await self.addDevice({ device: "d", profile: "default" });
  expect(await codeOf(self.addDevice({ device: "e", profile: "default" }))).toBe(Code.ResourceExhausted);
});

test("scenario links: revoked is Unauthenticated everywhere, unavailable is Unavailable, empty has no devices", async () => {
  const revoked = selfFor(FAKE_LINKS.revoked);
  expect(await codeOf(revoked.get({}))).toBe(Code.Unauthenticated);
  expect(await codeOf(revoked.listProfiles({}))).toBe(Code.Unauthenticated);
  expect(await codeOf(revoked.addDevice({ device: "x", profile: "default" }))).toBe(Code.Unauthenticated);
  expect(await codeOf(revoked.watch({})[Symbol.asyncIterator]().next())).toBe(Code.Unauthenticated);
  expect(await codeOf(selfFor(FAKE_LINKS.unavailable).get({}))).toBe(Code.Unavailable);

  const empty = selfFor(FAKE_LINKS.empty);
  expect(await empty.get({})).toMatchObject({ name: "dave", devices: [] });
  const snapshot: StatusSnapshot = (await empty.watch({})[Symbol.asyncIterator]().next()).value;
  expect(snapshot.peers).toEqual([]);
  expect(snapshot.upstreams).toEqual([]);
});
