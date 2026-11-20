// History ranges of the upstream page and turning samples into chart series.

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { num } from "../../api/live";
import type { Sample } from "../../gen/lotsman/v1/history_pb";

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * Each range asks for a step that gives a readable chart of a few hundred
 * points at most; the daemon may round it up (1 min samples for the last 7
 * days, else 1 h, and at most 2000 samples).
 */
export const historyRanges = [
  { key: "1h", span: HOUR, step: MINUTE },
  { key: "24h", span: DAY, step: 5 * MINUTE },
  { key: "7d", span: 7 * DAY, step: HOUR },
  { key: "90d", span: 90 * DAY, step: 6 * HOUR },
] as const;

export type RangeKey = (typeof historyRanges)[number]["key"];

/**
 * uPlot columns of the speed tests' down and up rates (bytes per second),
 * with a gap (null) for a step in which none ran.
 */
export function speedSeries(samples: readonly Sample[]): (number | null)[][] {
  const rate = (v: bigint) => (v > 0n ? Number(v) : null);
  return [samples.map((s) => (s.time ? timestampMs(s.time) / 1000 : 0)), samples.map((s) => rate(s.downRate)), samples.map((s) => rate(s.upRate))];
}

/** The mean of the values present, or undefined when there are none. */
export function meanOf(values: readonly (number | null)[]): number | undefined {
  const present = values.filter((v): v is number => v !== null);
  return present.length > 0 ? present.reduce((a, b) => a + b, 0) / present.length : undefined;
}

/** uPlot columns of the average down and up rates (bytes per second) over each step. */
export function rateSeries(samples: readonly Sample[], stepMs: number): number[][] {
  const seconds = stepMs / 1000;
  return [
    samples.map((s) => (s.time ? timestampMs(s.time) / 1000 : 0)),
    samples.map((s) => num(s.downBytes) / seconds),
    samples.map((s) => num(s.upBytes) / seconds),
  ];
}
