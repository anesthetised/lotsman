import { Code, ConnectError } from "@connectrpc/connect";
import { expect, test } from "vitest";
import { appendKeyed, appendRow, num, reopening } from "./live";

/** A stream opener that plays one script per opening: messages, then an error or a clean end. */
function scripted(...runs: { messages: number[]; error?: unknown }[]) {
  const signals: AbortSignal[] = [];
  const open = (signal: AbortSignal) => {
    signals.push(signal);
    const run = runs[signals.length - 1];
    return (async function* () {
      if (!run) return yield* never(signal);
      yield* run.messages;
      if (run.error) throw run.error;
    })();
  };
  return { open, signals };
}

/** A stream that stays open without a message until aborted, as a quiet Watch does. */
async function* never(signal: AbortSignal): AsyncGenerator<number> {
  await new Promise((resolve) => signal.addEventListener("abort", resolve));
  throw new ConnectError("aborted", Code.Canceled);
}

test("reopening carries on after a dropped stream and after one that ends", async () => {
  const { open, signals } = scripted({ messages: [1, 2], error: new TypeError("Load failed") }, { messages: [3] }, { messages: [4] });
  const it = reopening(open, [0])[Symbol.asyncIterator]();
  const got = [];
  for (let i = 0; i < 4; i++) got.push((await it.next()).value);
  expect(got).toEqual([1, 2, 3, 4]);
  expect(signals).toHaveLength(3);
  await it.return?.();
});

test("reopening gives up on an error that reopening cannot fix", async () => {
  const { open, signals } = scripted({ messages: [1], error: new ConnectError("link revoked", Code.Unauthenticated) });
  const it = reopening(open, [0])[Symbol.asyncIterator]();
  expect((await it.next()).value).toBe(1);
  await expect(it.next()).rejects.toMatchObject({ code: Code.Unauthenticated });
  expect(signals).toHaveLength(1);
});

test("return aborts the open stream at once, even while it waits for a message", async () => {
  const { open, signals } = scripted({ messages: [1] }, { messages: [] }); // the third opening never answers
  const it = reopening(open, [0])[Symbol.asyncIterator]();
  await it.next();
  const waiting = it.next();
  await expect.poll(() => signals.length).toBe(3);
  await it.return?.();
  expect(signals[2]!.aborted).toBe(true);
  expect(await waiting).toEqual({ done: true, value: undefined });
});

test("a page coming back to the foreground reopens without waiting out the backoff", async () => {
  const { open, signals } = scripted({ messages: [1], error: new TypeError("Load failed") }, { messages: [2] });
  const it = reopening(open, [60_000])[Symbol.asyncIterator]();
  await it.next();
  const next = it.next();
  await expect.poll(() => signals.length).toBe(1);
  document.dispatchEvent(new Event("visibilitychange"));
  expect((await next).value).toBe(2);
  await it.return?.();
});

test("appendRow adds a row to each column and keeps the last rows", () => {
  let columns = appendRow(undefined, [1, 10], 2);
  expect(columns).toEqual([[1], [10]]);
  columns = appendRow(columns, [2, 20], 2);
  const before = structuredClone(columns);
  const next = appendRow(columns, [3, 30], 2);
  expect(next).toEqual([
    [2, 3],
    [20, 30],
  ]);
  expect(columns).toEqual(before); // input untouched
});

test("appendKeyed keeps only the keys present now", () => {
  const a = appendKeyed(undefined, { x: 1, y: 2 }, 3);
  const b = appendKeyed(a, { x: 3 }, 3);
  expect(b).toEqual({ x: [1, 3] });
});

test("num", () => {
  expect(num(undefined)).toBe(0);
  expect(num(12n)).toBe(12);
});
