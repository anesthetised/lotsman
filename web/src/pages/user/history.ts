// The user's own traffic history: PeerHistory of each of their peers, summed.

import { durationMs, timestampMs } from "@bufbuild/protobuf/wkt";
import type { HistoryResponse } from "../../gen/lotsman/v1/history_pb";

export interface TrafficPoint {
  /** Start of the step, ms since the epoch. */
  time: number;
  /** Mean rates over the step, bytes per second. */
  down: number;
  up: number;
}

export interface Volume {
  down: number;
  up: number;
}

export interface TrafficHistory {
  /** The step the daemon used, ms. */
  step: number;
  /** All peers together, oldest first. */
  points: TrafficPoint[];
  /** Bytes moved in the whole range, in total and per peer. */
  total: Volume;
  byPeer: Map<string, Volume>;
}

/** The ranges the page offers, with the step it asks for (the daemon may round it up). */
export const RANGES = {
  day: { span: 24 * 3_600_000, step: 15 * 60_000 },
  week: { span: 7 * 24 * 3_600_000, step: 3_600_000 },
} as const;

export type Range = keyof typeof RANGES;

/** Sums the peers' samples that fall on the same step. */
export function mergeHistory(responses: ReadonlyMap<string, HistoryResponse>): TrafficHistory {
  const byTime = new Map<number, { down: number; up: number }>();
  const byPeer = new Map<string, Volume>();
  const total = { down: 0, up: 0 };
  let step = 0;
  for (const [peerId, res] of responses) {
    step = Math.max(step, res.step ? durationMs(res.step) : 0);
    const volume = { down: 0, up: 0 };
    for (const s of res.samples) {
      const time = s.time ? timestampMs(s.time) : 0;
      const at = byTime.get(time) ?? { down: 0, up: 0 };
      at.down += Number(s.downBytes);
      at.up += Number(s.upBytes);
      byTime.set(time, at);
      volume.down += Number(s.downBytes);
      volume.up += Number(s.upBytes);
    }
    byPeer.set(peerId, volume);
    total.down += volume.down;
    total.up += volume.up;
  }
  const seconds = Math.max(step, 1) / 1000;
  const points = [...byTime]
    .sort(([a], [b]) => a - b)
    .map(([time, v]) => ({ time, down: v.down / seconds, up: v.up / seconds }));
  return { step, points, total, byPeer };
}

export interface ChartPaths {
  down: string;
  up: string;
  /** The largest rate, bytes per second: the chart's top. */
  max: number;
}

/** SVG paths of both directions on one scale, in a `width` by `height` box, the largest rate at the top. */
export function chartPaths(points: readonly TrafficPoint[], width: number, height: number): ChartPaths {
  const max = Math.max(...points.flatMap((p) => [p.down, p.up]), 0);
  const first = points[0]?.time ?? 0;
  const span = (points.at(-1)?.time ?? 0) - first;
  const path = (value: (p: TrafficPoint) => number) =>
    points
      .map((p, i) => {
        const x = span > 0 ? ((p.time - first) / span) * width : width / 2;
        const y = max > 0 ? height - (value(p) / max) * height : height;
        return `${i === 0 ? "M" : "L"}${x.toFixed(1)},${y.toFixed(1)}`;
      })
      .join("");
  return { down: path((p) => p.down), up: path((p) => p.up), max };
}
