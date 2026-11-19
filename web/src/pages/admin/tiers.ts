// The profile form's state: a name and ordered tiers, edited as rows. Pure
// functions, so the editing rules are tested without a browser.

import type { Tier } from "../../api/profiles";

/** A tier row; `key` names the row for the edits below while rows move. */
export interface TierRow extends Tier {
  key: number;
}

let nextKey = 1;

export function tierRow(tier: Partial<Tier> = {}): TierRow {
  return { key: nextKey++, geo: tier.geo ?? "", provider: tier.provider ?? "", tag: tier.tag ?? "" };
}

export function removeTier(rows: readonly TierRow[], key: number): TierRow[] {
  return rows.filter((r) => r.key !== key);
}

/** Moves a row `by` places (-1 up, +1 down); a move past either end changes nothing. */
export function moveTier(rows: readonly TierRow[], key: number, by: number): TierRow[] {
  const from = rows.findIndex((r) => r.key === key);
  const to = from + by;
  if (from < 0 || to < 0 || to >= rows.length) return [...rows];
  const out = [...rows];
  const [row] = out.splice(from, 1);
  out.splice(to, 0, row!);
  return out;
}

export function updateTier(rows: readonly TierRow[], key: number, field: keyof Tier, value: string): TierRow[] {
  return rows.map((r) => (r.key === key ? { ...r, [field]: value } : r));
}

/** The tiers to send: values trimmed, row keys dropped. */
export function toTiers(rows: readonly TierRow[]): Tier[] {
  return rows.map((r) => ({ geo: r.geo.trim(), provider: r.provider.trim(), tag: r.tag.trim() }));
}

export const PROFILE_NAME = /^[A-Za-z0-9][A-Za-z0-9-]{0,31}$/;

export type NameProblem = "required" | "pattern" | "taken";

/**
 * What is wrong with the name before the daemon sees it: the daemon checks
 * the pattern too, but a new profile must not silently replace an existing
 * one, since Put creates or replaces. Names are unique ignoring case, as
 * the store keeps them.
 */
export function nameProblem(name: string, existing: readonly string[], editing: boolean): NameProblem | undefined {
  if (name === "") return "required";
  if (!PROFILE_NAME.test(name)) return "pattern";
  if (!editing && existing.some((e) => e.toLowerCase() === name.toLowerCase())) return "taken";
  return undefined;
}
