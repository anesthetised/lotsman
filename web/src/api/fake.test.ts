import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { HistoryService, ProfileService, StatusService, UserSelfService } from "../gen/lotsman/v1/lotsman_pb";
import { createFakeTransport, FAKE_USER } from "./fake";

beforeEach(() => {
  vi.useFakeTimers({ now: new Date("2026-11-19T12:00:00Z") });
});
afterEach(() => {
  vi.useRealTimers();
});

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
    expect(s.upstreams.every((u) => u.traffic === undefined && u.routed === 0)).toBe(true);
    break;
  }
  const user = await self.get({});
  expect(user.devices.map((d) => d.name)).toEqual(["phone", "laptop", "tv"]);
});

test("HistoryService.Upstream returns hourly samples for the range", async () => {
  const history = createClient(HistoryService, createFakeTransport());
  const to = Date.now();
  const res = await history.upstream({
    name: "de-1",
    from: { seconds: BigInt((to - 7 * 86_400_000) / 1000) },
    to: { seconds: BigInt(to / 1000) },
  });
  expect(res.samples).toHaveLength(7 * 24 + 1);
  expect(res.samples.at(-1)?.upRatio).toBe(0); // de-1 is down now
});

test("services without a fake answer Unimplemented", async () => {
  const profiles = createClient(ProfileService, createFakeTransport());
  await expect(profiles.list({})).rejects.toSatisfy((e) => ConnectError.from(e).code === Code.Unimplemented);
});
