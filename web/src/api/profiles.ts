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
