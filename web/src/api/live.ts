// Helpers that turn the server-streaming Watch RPCs into reactive values.

import { createMemo, from } from "solid-js";

/**
 * The latest message of a server stream. Reading it is pending (shows the
 * nearest <Loading> fallback) until the first message arrives; a stream error
 * goes to the nearest <Errored>. Disposing the owner cancels the stream.
 */
export function createLive<T>(open: () => AsyncIterable<T>) {
  return createMemo(() => from(open()));
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
