import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test, vi } from "vitest";
import { HistoryResponseSchema, SampleSchema } from "../../gen/lotsman/v1/history_pb";
import { DeviceConfigSchema } from "../../gen/lotsman/v1/user_pb";
import { en } from "../../i18n/en";
import { ru } from "../../i18n/ru";
import { confFilename } from "./config";
import { failureOf, failureText } from "./errors";
import { chartPaths, mergeHistory } from "./history";
import { changeProfile, ProfileChangeError } from "./profileChange";

const failure = (code: Code, message = "") => failureOf(new ConnectError(message, code));

describe("failureOf", () => {
  test("a revoked link is Unauthenticated", () => {
    expect(failure(Code.Unauthenticated, "unknown link")).toEqual({ kind: "revoked" });
  });

  test("the daemon's two limits are told apart by their wording", () => {
    expect(failure(Code.ResourceExhausted, "at most 5 devices; remove one first or ask the operator")).toEqual({ kind: "maxDevices", limit: 5 });
    expect(failure(Code.ResourceExhausted, "at most 1 device; remove one first")).toEqual({ kind: "maxDevices", limit: 1 });
    expect(failure(Code.ResourceExhausted, "too many devices added lately; try again in a few minutes")).toEqual({ kind: "rateLimited" });
    // The panel's own limit, or wording we do not know: still a limit, not a failure.
    expect(failure(Code.ResourceExhausted, "too many unknown links from this address; try again later")).toEqual({ kind: "exhausted" });
    expect(failure(Code.ResourceExhausted, "the gateway's subnet has no free address")).toEqual({ kind: "exhausted" });
  });

  test.each([
    [Code.PermissionDenied, "denied"],
    [Code.Unavailable, "unavailable"],
    [Code.DeadlineExceeded, "unavailable"],
    [Code.AlreadyExists, "exists"],
    [Code.NotFound, "notFound"],
  ] as const)("code %s is %s", (code, kind) => {
    expect(failure(code).kind).toBe(kind);
  });

  test("a refused name carries the daemon's reason; anything else its message", () => {
    expect(failure(Code.InvalidArgument, "no such profile: x")).toEqual({ kind: "invalid", message: "no such profile: x" });
    expect(failure(Code.Internal, "users: internal error")).toEqual({ kind: "other", message: "users: internal error" });
    expect(failureOf(new TypeError("Failed to fetch")).kind).toBe("other");
  });

  test("a profile change fails for the reason of the call that failed", () => {
    expect(failureOf(new ProfileChangeError("add", new ConnectError("", Code.PermissionDenied)))).toEqual({ kind: "denied" });
    expect(failureOf(new ProfileChangeError("remove", new ConnectError("", Code.Unauthenticated)))).toEqual({ kind: "revoked" });
  });

  test("every failure has words in both languages", () => {
    const kinds = [
      { kind: "revoked" },
      { kind: "unavailable" },
      { kind: "denied" },
      { kind: "maxDevices", limit: 5 },
      { kind: "maxDevices" },
      { kind: "rateLimited" },
      { kind: "exhausted" },
      { kind: "exists" },
      { kind: "notFound" },
      { kind: "invalid", message: "why" },
      { kind: "other", message: "why" },
    ] as const;
    for (const f of kinds) {
      expect(failureText(en, f)).not.toBe("");
      expect(failureText(ru, f)).not.toBe("");
    }
    expect(failureText(en, { kind: "maxDevices", limit: 5 })).toContain("(5)");
    expect(failureText(ru, { kind: "maxDevices", limit: 5 })).toContain("(5)");
    expect(failureText(en, { kind: "invalid", message: "why" })).toContain("why");
  });
});

describe("changeProfile", () => {
  const config = create(DeviceConfigSchema, { peerId: "peer-9", config: "[Interface]" });

  test("adds the new profile first, then removes the old one, and returns the new config", async () => {
    const calls: string[] = [];
    const got = await changeProfile(
      {
        addDevice: async (req) => (calls.push(`add ${req.device}/${req.profile}`), config),
        removeDevice: async (req) => void calls.push(`remove ${req.device}/${req.profile}`),
      },
      "phone",
      "default",
      "streaming",
    );
    expect(calls).toEqual(["add phone/streaming", "remove phone/default"]);
    expect(got).toBe(config);
  });

  test("removes nothing when the add fails", async () => {
    const removeDevice = vi.fn();
    const denied = new ConnectError("adding is off", Code.PermissionDenied);
    const error = await changeProfile({ addDevice: () => Promise.reject(denied), removeDevice }, "phone", "default", "streaming").catch((e) => e);
    expect(error).toBeInstanceOf(ProfileChangeError);
    expect(error).toMatchObject({ stage: "add", cause: denied, config: undefined });
    expect(removeDevice).not.toHaveBeenCalled();
  });

  test("keeps the new config when the removal fails: the device has both profiles", async () => {
    const gone = new ConnectError("no such device or profile", Code.NotFound);
    const error = await changeProfile({ addDevice: async () => config, removeDevice: () => Promise.reject(gone) }, "phone", "default", "streaming").catch(
      (e) => e,
    );
    expect(error).toMatchObject({ stage: "remove", cause: gone, config });
  });
});

describe("confFilename", () => {
  test.each([
    ["phone", "default", "phone-default.conf"],
    ["laptop", "streaming", "laptop-streamin.conf"],
    ["tablet", "streaming", "tablet-streamin.conf"],
    ["my phone", "a/b", "my-phone-a-b.conf"],
    ["телефон", "default", "default.conf"],
    ["café", "eu", "cafe-eu.conf"],
    ["телефон", "профиль", "lotsman.conf"],
  ])("%s / %s → %s", (device, profile, want) => {
    expect(confFilename(device, profile)).toBe(want);
  });
});

describe("mergeHistory", () => {
  const sample = (t: number, down: number, up: number) =>
    create(SampleSchema, { time: timestampFromMs(t), downBytes: BigInt(down), upBytes: BigInt(up) });
  const response = (stepMs: number, ...samples: ReturnType<typeof sample>[]) =>
    create(HistoryResponseSchema, { step: durationFromMs(stepMs), samples });

  test("sums the peers' samples of the same step into rates", () => {
    const history = mergeHistory(
      new Map([
        ["a", response(60_000, sample(0, 6000, 600), sample(60_000, 0, 0))],
        ["b", response(60_000, sample(0, 3000, 300), sample(60_000, 12_000, 0))],
      ]),
    );
    expect(history.step).toBe(60_000);
    expect(history.points).toEqual([
      { time: 0, down: 150, up: 15 }, // 9000 B over 60 s
      { time: 60_000, down: 200, up: 0 },
    ]);
    expect(history.total).toEqual({ down: 21_000, up: 900 });
    expect(history.byPeer.get("a")).toEqual({ down: 6000, up: 600 });
    expect(history.byPeer.get("b")).toEqual({ down: 15_000, up: 300 });
  });

  test("orders the points by time even when the peers disagree on the range", () => {
    const history = mergeHistory(new Map([["a", response(1000, sample(2000, 1000, 0))], ["b", response(1000, sample(1000, 1000, 0))]]));
    expect(history.points.map((p) => p.time)).toEqual([1000, 2000]);
  });

  test("is empty without peers", () => {
    expect(mergeHistory(new Map())).toEqual({ step: 0, points: [], total: { down: 0, up: 0 }, byPeer: new Map() });
  });
});

describe("chartPaths", () => {
  test("scales both directions to the largest rate", () => {
    const paths = chartPaths(
      [
        { time: 0, down: 100, up: 25 },
        { time: 10, down: 50, up: 0 },
        { time: 20, down: 0, up: 100 },
      ],
      200,
      40,
    );
    expect(paths.max).toBe(100);
    expect(paths.down).toBe("M0.0,0.0L100.0,20.0L200.0,40.0");
    expect(paths.up).toBe("M0.0,30.0L100.0,40.0L200.0,0.0");
  });

  test("a flat zero series lies on the bottom, and nothing draws nothing", () => {
    expect(chartPaths([{ time: 0, down: 0, up: 0 }], 10, 10).down).toBe("M5.0,10.0");
    expect(chartPaths([], 10, 10)).toEqual({ down: "", up: "", max: 0 });
  });
});
