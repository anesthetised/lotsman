// Helpers that turn the server-streaming Watch RPCs into reactive values.

import { Code, ConnectError } from "@connectrpc/connect";
import { createMemo, from } from "solid-js";

/**
 * The latest message of a server stream. Reading it is pending (shows the
 * nearest <Loading> fallback) until the first message arrives. The stream
 * reopens after it ends or drops (see reopening), so only an error that
 * reopening cannot fix goes to the nearest <Errored>. Disposing the owner
 * cancels the stream.
 */
export function createLive<T>(open: (signal: AbortSignal) => AsyncIterable<T>) {
  return createMemo(() => from(reopening(open)));
}

/** Errors after which reopening the stream cannot help: the page must say so. */
const FINAL = new Set([Code.Unauthenticated, Code.PermissionDenied, Code.NotFound, Code.InvalidArgument, Code.FailedPrecondition, Code.Unimplemented]);

/** Waits before reopening, by failures in a row; the last one repeats. */
export const REOPEN_DELAYS_MS = [1000, 2000, 5000, 10000, 30000];

/**
 * A stream that reopens itself. A phone suspends a page in the background
 * (iOS Safari answers with "Load failed"), networks switch and the panel
 * restarts: each drops the stream, and the page should carry on with the
 * last message until a new stream answers, not fail. Reopening backs off
 * while it keeps failing, and goes at once when the page comes back to the
 * foreground or the device back online.
 *
 * Solid closes an iterable it no longer needs with return(); an async
 * generator would only see that once its pending await settles, so return()
 * first aborts the open stream and any wait through the signal.
 */
export function reopening<T>(open: (signal: AbortSignal) => AsyncIterable<T>, delays = REOPEN_DELAYS_MS): AsyncIterable<T> {
  return {
    [Symbol.asyncIterator]() {
      const abort = new AbortController();
      const it = pump(open, abort.signal, delays);
      return {
        next: () => it.next(),
        return: (value?: T) => {
          abort.abort();
          return it.return(value as T);
        },
      };
    },
  };
}

async function* pump<T>(open: (signal: AbortSignal) => AsyncIterable<T>, signal: AbortSignal, delays: readonly number[]): AsyncGenerator<T> {
  let failures = 0;
  while (!signal.aborted) {
    try {
      for await (const message of open(signal)) {
        failures = 0;
        yield message;
      }
    } catch (e) {
      if (signal.aborted) return;
      if (e instanceof ConnectError && FINAL.has(e.code)) throw e;
    }
    await pause(delays[Math.min(failures++, delays.length - 1)] ?? 0, signal);
  }
}

/** Resolves after ms, or sooner when the page becomes visible, the device goes online or signal aborts. */
function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const onVisible = () => {
      if (document.visibilityState === "visible") done();
    };
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      document.removeEventListener("visibilitychange", onVisible);
      window.removeEventListener("online", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done);
    document.addEventListener("visibilitychange", onVisible);
    window.addEventListener("online", done);
  });
}

/**
 * Appends a row to column-oriented series (uPlot's layout: `[xs, ys1, ys2…]`),
 * keeping the last `max` rows. Returns new arrays; the input is untouched.
 */
export function appendRow(columns: readonly number[][] | undefined, row: readonly number[], max: number): number[][] {
  return row.map((value, i) => [...(columns?.[i] ?? []), value].slice(-max));
}

/** Appends one value per key, keeping the last `max` values of each key present in `values`. */
export function appendKeyed(
  series: Readonly<Record<string, readonly number[]>> | undefined,
  values: Readonly<Record<string, number>>,
  max: number,
): Record<string, number[]> {
  return Object.fromEntries(Object.entries(values).map(([key, v]) => [key, [...(series?.[key] ?? []), v].slice(-max)]));
}

/** uint64 counters arrive as bigint; rates and byte counts fit a double. */
export function num(value: bigint | undefined): number {
  return value === undefined ? 0 : Number(value);
}
