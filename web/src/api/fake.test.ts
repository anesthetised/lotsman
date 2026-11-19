import { durationFromMs, durationMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { HistoryService } from "../gen/lotsman/v1/history_pb";
import { ProfileService } from "../gen/lotsman/v1/profile_pb";
import { StatusService } from "../gen/lotsman/v1/status_pb";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { createFakeTransport, FAKE_USER } from "./fake";
import { historyStep } from "./simulation";

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

async function next<T>(stream: AsyncIterator<T>): Promise<T> {
  const result = await stream.next();
  if (result.done) throw new Error("the stream ended");
  return result.value;
}

test("StatusService.Watch sends a snapshot now and every second, until cancelled", async () => {
  const status = createClient(StatusService, createFakeTransport({ seed: 1 }));
  const stream = status.watch({})[Symbol.asyncIterator]();

  const first = await next(stream);
  expect(first.upstreams.map((u) => u.name)).toEqual(["lv-1", "al-1", "nl-1", "de-1"]);
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
    expect(s.upstreams.map((u) => u.name).sort()).toEqual(["lv-1", "nl-1"]);
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

test("UserSelfService history covers only the user's upstreams and peers, without upstream bytes", async () => {
  const self = createClient(UserSelfService, createFakeTransport());
  const upstream = await self.upstreamHistory(range("lv-1", 7, HOUR));
  expect(upstream.samples).toHaveLength(7 * 24 + 1);
  expect(upstream.samples.every((s) => s.downBytes === 0n && s.upBytes === 0n)).toBe(true);
  expect(upstream.samples.some((s) => s.latency !== undefined && s.upRatio > 0)).toBe(true);
  await expect(self.upstreamHistory(range("al-1", 7, HOUR))).rejects.toSatisfy(notFound); // only bob uses it

  const peer = await self.peerHistory(range("peer-1", 1, HOUR));
  expect(peer.samples[0]?.downBytes).toBeGreaterThan(0n);
  await expect(self.peerHistory(range("peer-4", 1, HOUR))).rejects.toSatisfy(notFound); // bob's
});

test("services without a fake answer Unimplemented", async () => {
  const profiles = createClient(ProfileService, createFakeTransport());
  await expect(profiles.list({})).rejects.toSatisfy((e) => ConnectError.from(e).code === Code.Unimplemented);
});
