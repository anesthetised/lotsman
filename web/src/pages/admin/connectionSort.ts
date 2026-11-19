// Sorting of the live connections table.

import type { Connection } from "../../gen/lotsman/v1/connection_pb";
import { num } from "../../api/live";

export const sortKeys = ["downRate", "upRate", "downBytes", "upBytes"] as const;
export type SortKey = (typeof sortKeys)[number];

/**
 * Connections by one traffic column, largest first or (`ascending`) last.
 * Ties keep a fixed order by peer, source and destination, so rows with equal
 * values do not swap places on every update. The input is untouched.
 */
export function sortConnections(list: readonly Connection[], key: SortKey, ascending = false): Connection[] {
  const sign = ascending ? 1 : -1;
  return [...list].sort((a, b) => {
    const by = num(a.traffic?.[key]) - num(b.traffic?.[key]);
    return by !== 0 ? sign * by : connectionKey(a).localeCompare(connectionKey(b));
  });
}

/** What identifies a connection from one snapshot to the next. */
export function connectionKey(c: Connection): string {
  return `${c.peerId} ${c.protocol} ${c.source} ${c.destination}`;
}
