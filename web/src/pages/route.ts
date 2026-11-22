// A device's route as the route editor holds it: every country on offer,
// the chosen ones first in their order, each with its chosen providers.
// Pure functions, shared by the user page and the console, tested without a browser.

import { create, equals } from "@bufbuild/protobuf";
import { durationMs } from "@bufbuild/protobuf/wkt";
import { type UpstreamStatus, UpstreamState } from "../gen/lotsman/v1/status_pb";
import { type Route, RouteSchema } from "../gen/lotsman/v1/user_pb";
import type { Dict } from "../i18n/en";

/** What a new device gets without a route: the fastest of any allowed exit. */
export const DEFAULT_ROUTE: Route = /* @__PURE__ */ create(RouteSchema, { fastest: true, strict: true });

/** The countries a route may name, in display order, each with its providers (sorted; none when they are hidden). */
export type Offer = ReadonlyMap<string, readonly string[]>;

/** One row of the editor. `providers` are the chosen ones, in the offer's order. */
export interface CountryChoice {
  geo: string;
  on: boolean;
  providers: readonly string[];
}

export interface Draft {
  fastest: boolean;
  strict: boolean;
  /** The chosen countries first, in route order; then the others, in the offer's order. */
  countries: readonly CountryChoice[];
}

/**
 * The countries and providers of `exits`, ordered by `name` (a country's
 * display name). With `providers` false every country has none: the user may not see them.
 */
export function offerOf(exits: readonly Pick<UpstreamStatus, "geo" | "provider">[], name: (geo: string) => string, providers = true): Offer {
  const byGeo = new Map<string, Set<string>>();
  for (const u of exits) {
    if (!u.geo) continue;
    const set = byGeo.get(u.geo) ?? new Set();
    if (providers && u.provider) set.add(u.provider);
    byGeo.set(u.geo, set);
  }
  return new Map(
    [...byGeo]
      .sort(([a], [b]) => name(a).localeCompare(name(b)))
      .map(([geo, set]) => [geo, [...set].sort()]),
  );
}

/** Whether two offers name the same countries and providers in the same order. */
export function sameOffer(a: Offer, b: Offer): boolean {
  const key = (o: Offer) => [...o].map(([geo, providers]) => `${geo}:${providers.join(",")}`).join(";");
  return key(a) === key(b);
}

/**
 * The draft of a stored route. Countries and providers the offer lacks (a
 * stale route, or an exit the user may no longer use) are left out; a country
 * with no provider left, or with none named, has every provider. An empty
 * route (any exit) has every country on.
 */
export function draftOf(route: Route, offer: Offer): Draft {
  const listed: CountryChoice[] = [];
  for (const c of route.countries) {
    const offered = offer.get(c.geo);
    if (!offered || listed.some((l) => l.geo === c.geo)) continue;
    const chosen = offered.filter((p) => c.providers.includes(p));
    listed.push({ geo: c.geo, on: true, providers: chosen.length > 0 ? chosen : offered });
  }
  const anyExit = route.countries.length === 0;
  const rest = [...offer]
    .filter(([geo]) => !listed.some((l) => l.geo === geo))
    .map(([geo, providers]) => ({ geo, on: anyExit, providers }));
  return { fastest: route.fastest, strict: route.strict, countries: [...listed, ...rest] };
}

/** The route to store: the chosen countries in order; a country with every provider chosen names none. */
export function routeOf(draft: Draft, offer: Offer): Route {
  return create(RouteSchema, {
    fastest: draft.fastest,
    strict: draft.strict,
    countries: draft.countries
      .filter((c) => c.on)
      .map((c) => ({ geo: c.geo, providers: c.providers.length === (offer.get(c.geo)?.length ?? 0) ? [] : [...c.providers] })),
  });
}

/** Whether the draft stores something other than `saved`. */
export function changed(draft: Draft, saved: Route, offer: Offer): boolean {
  return !equals(RouteSchema, routeOf(draft, offer), routeOf(draftOf(saved, offer), offer));
}

export function chosen(draft: Draft): CountryChoice[] {
  return draft.countries.filter((c) => c.on);
}

/**
 * Turns a country on (it goes last among the chosen, with every provider) or
 * off (it goes back among the others, in the offer's order). The last chosen
 * country stays on: a route of none would mean any exit.
 */
export function toggleCountry(draft: Draft, geo: string, offer: Offer): Draft {
  const row = draft.countries.find((c) => c.geo === geo);
  if (!row || (row.on && chosen(draft).length === 1)) return draft;
  const on = chosen(draft).filter((c) => c.geo !== geo);
  if (!row.on) on.push({ geo, on: true, providers: offer.get(geo) ?? [] });
  const off = [...offer].filter(([g]) => !on.some((c) => c.geo === g)).map(([g, providers]) => ({ geo: g, on: false, providers }));
  return { ...draft, countries: [...on, ...off] };
}

/** Chooses a provider of a chosen country, or drops it; the last one stays. */
export function toggleProvider(draft: Draft, geo: string, provider: string, offer: Offer): Draft {
  const row = draft.countries.find((c) => c.geo === geo);
  if (!row?.on) return draft;
  const has = row.providers.includes(provider);
  if (has && row.providers.length === 1) return draft;
  const providers = (offer.get(geo) ?? []).filter((p) => (p === provider ? !has : row.providers.includes(p)));
  return { ...draft, countries: draft.countries.map((c) => (c.geo === geo ? { ...c, providers } : c)) };
}

/** Moves a chosen country to position `to` among the chosen; the same draft when it is there already or `to` is out of range. */
export function moveCountry(draft: Draft, geo: string, to: number): Draft {
  const on = chosen(draft);
  const from = on.findIndex((c) => c.geo === geo);
  if (from < 0 || from === to || to < 0 || to >= on.length) return draft;
  const [row] = on.splice(from, 1);
  on.splice(to, 0, row!);
  return { ...draft, countries: [...on, ...draft.countries.filter((c) => !c.on)] };
}

/** An exit carries traffic while it is up, and as a last resort while degraded. */
function usable(u: UpstreamStatus): boolean {
  return u.state === UpstreamState.UP || u.state === UpstreamState.DEGRADED;
}

export function matchesCountry(c: Pick<CountryChoice, "geo" | "providers">, u: Pick<UpstreamStatus, "geo" | "provider">): boolean {
  return u.geo === c.geo && (c.providers.length === 0 || c.providers.includes(u.provider));
}

/** A country of the draft now: whether a chosen provider has a usable exit, and the lowest latency among those. */
export function countryNow(c: Pick<CountryChoice, "geo" | "providers">, exits: readonly UpstreamStatus[]): { available: boolean; latencyMs?: number } {
  const live = exits.filter((u) => matchesCountry(c, u) && usable(u));
  const latencies = live.flatMap((u) => (u.latency ? [durationMs(u.latency)] : []));
  return { available: live.length > 0, latencyMs: latencies.length > 0 ? Math.min(...latencies) : undefined };
}

/** Whether a provider of a country has a usable exit now. */
export function providerAvailable(geo: string, provider: string, exits: readonly UpstreamStatus[]): boolean {
  return exits.some((u) => u.geo === geo && u.provider === provider && usable(u));
}

/** A strict draft whose chosen countries have no usable exit: the device's traffic is blocked. */
export function blocked(draft: Draft, exits: readonly UpstreamStatus[]): boolean {
  return draft.strict && !chosen(draft).some((c) => countryNow(c, exits).available);
}

/**
 * A route in a line: "Latvia → Netherlands", "Fastest: Latvia (provider-a),
 * Netherlands · strict", or "Any, fastest". Providers show when `providers` is true.
 */
export function routeSummary(route: Route, t: Dict, country: (geo: string) => string, providers = true): string {
  if (route.countries.length === 0) return t.route.summary.any;
  const names = route.countries.map((c) => (providers && c.providers.length > 0 ? `${country(c.geo)} (${c.providers.join(", ")})` : country(c.geo)));
  const list = route.fastest ? t.route.summary.fastest(names.join(", ")) : names.join(" → ");
  return route.strict ? `${list} · ${t.route.summary.strict}` : list;
}
