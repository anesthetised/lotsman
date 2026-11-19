// Profile tiers as the CLI writes them, and the daemon's profile warnings
// matched back to the profile and tier they are about.

/** One tier: an upstream must match every field that is set. */
export interface Tier {
  geo: string;
  provider: string;
  tag: string;
}

/** A tier in the CLI's syntax: `geo=NL,tag=eu`, or `any` when no field is set. */
export function tierString(tier: Tier): string {
  const parts = (["geo", "provider", "tag"] as const).filter((key) => tier[key] !== "").map((key) => `${key}=${tier[key]}`);
  return parts.length > 0 ? parts.join(",") : "any";
}

/** What a tier matches against: an upstream's labels. */
export interface UpstreamLabels {
  geo: string;
  provider: string;
  tags: readonly string[];
}

/** Whether the upstream has every field the tier sets. */
export function tierMatches(tier: Tier, u: UpstreamLabels): boolean {
  return (!tier.geo || tier.geo === u.geo) && (!tier.provider || tier.provider === u.provider) && (!tier.tag || u.tags.includes(tier.tag));
}

/** A geo or provider the CLI can write back in a tier (internal/config/profile.go). */
export const MATCH_VALUE = /^[^\s,=]{1,64}$/;
/** A tag, as upstream tags are written. */
export const TAG = /^[a-z0-9][a-z0-9-]{0,31}$/;

/** Why the daemon refuses to store a tier: a value it cannot write, or no upstream to match. */
export type TierProblem = { kind: "value"; field: keyof Tier } | { kind: "unmatched" };

/** The daemon's check of a tier being written (ValidateProfileWrite), value format first. */
export function tierProblem(tier: Tier, upstreams: readonly UpstreamLabels[]): TierProblem | undefined {
  for (const field of ["geo", "provider"] as const) {
    if (tier[field] !== "" && !MATCH_VALUE.test(tier[field])) return { kind: "value", field };
  }
  if (tier.tag !== "" && !TAG.test(tier.tag)) return { kind: "value", field: "tag" };
  if (!upstreams.some((u) => tierMatches(tier, u))) return { kind: "unmatched" };
  return undefined;
}

/**
 * The values `field` can take so that the tier still matches an upstream
 * given its other fields, sorted: a tier with geo=NL suggests only the
 * providers of NL upstreams.
 */
export function suggestions(tier: Tier, field: keyof Tier, upstreams: readonly UpstreamLabels[]): string[] {
  const rest = { ...tier, [field]: "" };
  const values = upstreams.filter((u) => tierMatches(rest, u)).flatMap((u) => (field === "tag" ? u.tags : [u[field]]));
  return [...new Set(values.filter((v) => v !== ""))].sort();
}

export type ProfileWarning =
  /** A tier that matches no upstream. */
  | { kind: "unmatched"; tier: number; match: string; text: string }
  /** A tier whose upstreams earlier tiers (`by`, in CLI syntax) all take. */
  | { kind: "shadowed"; tier: number; match: string; by: string; text: string }
  /** Any other warning about the profile; only its text is known. */
  | { kind: "other"; tier?: undefined; text: string };

export interface Warnings {
  byProfile: Map<string, ProfileWarning[]>;
  /** Warnings that name no profile. */
  other: string[];
}

const PROFILE = /^profile "([^"]*)": (.*)$/;
const UNMATCHED = /^tier (\d+) (\S+) matches no upstream and is ignored$/;
const SHADOWED = /^tier (\d+) (\S+) never applies: every upstream it matches is taken by (.+)$/;

/**
 * Groups the daemon's warnings by profile and recognises the two it gives
 * about tiers (internal/config/profile.go), such as
 * `profile "eu": tier 2 geo=FI matches no upstream and is ignored`, so they
 * can be shown translated. Anything else keeps its text.
 */
export function groupWarnings(warnings: readonly string[]): Warnings {
  const byProfile = new Map<string, ProfileWarning[]>();
  const other: string[] = [];
  for (const text of warnings) {
    const [, name, rest = ""] = PROFILE.exec(text) ?? [];
    if (name === undefined) {
      other.push(text);
      continue;
    }
    const unmatched = UNMATCHED.exec(rest);
    const shadowed = SHADOWED.exec(rest);
    const warning: ProfileWarning = unmatched
      ? { kind: "unmatched", tier: Number(unmatched[1]), match: unmatched[2]!, text }
      : shadowed
        ? { kind: "shadowed", tier: Number(shadowed[1]), match: shadowed[2]!, by: shadowed[3]!, text }
        : { kind: "other", text };
    byProfile.set(name, [...(byProfile.get(name) ?? []), warning]);
  }
  return { byProfile, other };
}
