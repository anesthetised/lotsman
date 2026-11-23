import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "vitest";
import { HistoryResponseSchema, SampleSchema } from "../../gen/lotsman/v1/history_pb";
import { SelfServiceLimit_Reason, SelfServiceLimitSchema } from "../../gen/lotsman/v1/user_self_pb";
import { en } from "../../i18n/en";
import { ru } from "../../i18n/ru";
import { confFilename } from "./config";
import { failureOf, failureText } from "./errors";
import { chartPaths, mergeHistory } from "./history";

const failure = (code: Code, message = "") => failureOf(new ConnectError(message, code));

describe("failureOf", () => {
  test("a revoked link is Unauthenticated", () => {
    expect(failure(Code.Unauthenticated, "unknown link")).toEqual({ kind: "revoked" });
  });

  test("the daemon's two limits are told apart by their detail, not their wording", () => {
    const limited = (value: MessageInitShape<typeof SelfServiceLimitSchema>) =>
      failureOf(new ConnectError("anything", Code.ResourceExhausted, undefined, [{ desc: SelfServiceLimitSchema, value }]));
    expect(limited({ reason: SelfServiceLimit_Reason.MAX_DEVICES, maxDevices: 5 })).toEqual({ kind: "maxDevices", limit: 5 });
    expect(limited({ reason: SelfServiceLimit_Reason.RATE_LIMITED, retryAfter: { seconds: 90n } })).toEqual({ kind: "rateLimited", retryAfterSeconds: 90 });
    expect(limited({ reason: SelfServiceLimit_Reason.RATE_LIMITED })).toEqual({ kind: "rateLimited" });
    // The panel's own limit, or no detail: still a limit, not a failure.
    expect(failure(Code.ResourceExhausted, "at most 5 devices; remove one first")).toEqual({ kind: "exhausted" });
    expect(failure(Code.ResourceExhausted, "too many unknown links from this address; try again later")).toEqual({ kind: "exhausted" });
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
    expect(failure(Code.InvalidArgument, "route: no exit in country FI")).toEqual({ kind: "invalid", message: "route: no exit in country FI" });
    expect(failure(Code.Internal, "users: internal error")).toEqual({ kind: "other", message: "users: internal error" });
    expect(failure(Code.Unknown, "Failed to fetch")).toEqual({ kind: "other", message: "Failed to fetch" });
    expect(failureOf(new TypeError("Unexpected result"))).toEqual({ kind: "other", message: "Unexpected result" });
  });

  test.each(["Failed to fetch", "Load failed", "NetworkError when attempting to fetch resource."])(
    "a browser connection failure gets a localized retry message (%s)",
    (message) => {
      const browserError = new TypeError(message);
      const result = failureOf(ConnectError.from(browserError));
      expect(failureOf(browserError)).toEqual(result);
      expect(failureText(ru, result)).toBe("Не удалось связаться с сайтом. Проверьте подключение к интернету и попробуйте ещё раз.");
      expect(failureText(en, result)).toBe("Couldn’t reach the site. Check your internet connection and try again.");
    },
  );

  test("every failure has words in both languages", () => {
    const kinds = [
      { kind: "revoked" },
      { kind: "unavailable" },
      { kind: "denied" },
      { kind: "maxDevices", limit: 5 },
      { kind: "rateLimited" },
      { kind: "rateLimited", retryAfterSeconds: 90 },
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
    expect(failureText(en, { kind: "rateLimited", retryAfterSeconds: 90 })).toContain("2 min");
    expect(failureText(en, { kind: "invalid", message: "why" })).toContain("why");
  });
});

describe("confFilename", () => {
  test.each([
    ["phone", "phone.conf"],
    ["my phone / work", "my-phone-work.conf"],
    ["the-very-long-laptop", "the-very-long-l.conf"], // tunnel names are at most 15 characters
    ["café", "cafe.conf"],
    ["телефон 2", "2.conf"],
    ["телефон", "lotsman.conf"],
  ])("%s → %s", (device, want) => {
    expect(confFilename(device)).toBe(want);
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
