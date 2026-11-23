import { create } from "@bufbuild/protobuf";
import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, test } from "vitest";
import { RoutingBlock, UpstreamState, UpstreamStatusSchema } from "../gen/lotsman/v1/status_pb";
import { type Route, RouteSchema } from "../gen/lotsman/v1/user_pb";
import { en } from "../i18n/en";
import { formatters } from "../i18n/format";
import { ru } from "../i18n/ru";
import {
  changed,
  chosen,
  countryNow,
  DEFAULT_ROUTE,
  type Draft,
  draftOf,
  moveCountry,
  offerOf,
  providerAvailable,
  routeAvailability,
  routeOf,
  routeSummary,
  sameOffer,
  toggleCountry,
  toggleProvider,
} from "./route";

const exit = (name: string, geo: string, provider: string, state = UpstreamState.UP, latencyMs = 50) =>
  create(UpstreamStatusSchema, { name, geo, provider, state, latency: state === UpstreamState.DOWN ? undefined : durationFromMs(latencyMs) });

const exits = [
  exit("lv-1", "LV", "provider-a", UpstreamState.UP, 38),
  exit("lv-2", "LV", "provider-b", UpstreamState.UP, 52),
  exit("nl-1", "NL", "provider-c", UpstreamState.DEGRADED, 140),
  exit("al-1", "AL", "provider-b", UpstreamState.UP, 61),
  exit("de-1", "DE", "provider-a", UpstreamState.DOWN),
];
const country = formatters("en").country;
const offer = offerOf(exits, country);
const route = (r: Parameters<typeof create<typeof RouteSchema>>[1]): Route => create(RouteSchema, r);
/** The draft's rows as `GEO` (off) or `+GEO:providers` (on), in order. */
const rows = (d: Draft) => d.countries.map((c) => `${c.on ? "+" : ""}${c.geo}${c.on ? `:${c.providers.join("|")}` : ""}`);

describe("the offer", () => {
  test("lists each country once, by its name, with its providers sorted", () => {
    expect([...offer]).toEqual([
      ["AL", ["provider-b"]],
      ["DE", ["provider-a"]],
      ["LV", ["provider-a", "provider-b"]],
      ["NL", ["provider-c"]],
    ]);
    // Russian names sort differently: Албания, Германия, Латвия, Нидерланды.
    expect([...offerOf(exits, formatters("ru").country).keys()]).toEqual(["AL", "DE", "LV", "NL"]);
    expect([...offerOf([exit("x", "US", "a"), exit("y", "AT", "b")], country).keys()]).toEqual(["AT", "US"]);
  });

  test("has no providers when they are hidden, and compares by content", () => {
    expect(offerOf(exits, country, false).get("LV")).toEqual([]);
    expect(sameOffer(offerOf([...exits].reverse(), country), offer)).toBe(true);
    expect(sameOffer(offerOf(exits.slice(1), country), offer)).toBe(false);
  });
});

describe("drafts", () => {
  test("put the route's countries first, in order, then the rest", () => {
    const d = draftOf(route({ fastest: false, strict: true, countries: [{ geo: "NL" }, { geo: "LV", providers: ["provider-b"] }] }), offer);
    expect(rows(d)).toEqual(["+NL:provider-c", "+LV:provider-b", "AL", "DE"]);
    expect([d.fastest, d.strict]).toEqual([false, true]);
  });

  test("an empty route, any exit, has every country on", () => {
    expect(rows(draftOf(DEFAULT_ROUTE, offer))).toEqual(["+AL:provider-b", "+DE:provider-a", "+LV:provider-a|provider-b", "+NL:provider-c"]);
  });

  test("leave out what the offer lacks: a stale country or provider, or a duplicate", () => {
    const d = draftOf(route({ countries: [{ geo: "FI" }, { geo: "LV", providers: ["gone"] }, { geo: "LV" }] }), offer);
    expect(rows(d)).toEqual(["+LV:provider-a|provider-b", "AL", "DE", "NL"]);
  });

  test("round-trip to the same route; every provider chosen names none", () => {
    const stored = route({ fastest: true, strict: false, countries: [{ geo: "LV", providers: ["provider-a"] }, { geo: "AL" }] });
    const d = draftOf(stored, offer);
    expect(routeOf(d, offer)).toEqual(stored);
    expect(changed(d, stored, offer)).toBe(false);
    expect(changed({ ...d, strict: true }, stored, offer)).toBe(true);
    expect(routeOf(toggleProvider(d, "LV", "provider-b", offer), offer).countries[0]?.providers).toEqual([]);
  });
});

describe("editing", () => {
  const start = draftOf(route({ strict: true, countries: [{ geo: "LV" }, { geo: "NL" }] }), offer);

  test("a country turned on goes last among the chosen; turned off, back among the others in order", () => {
    const on = toggleCountry(start, "AL", offer);
    expect(rows(on)).toEqual(["+LV:provider-a|provider-b", "+NL:provider-c", "+AL:provider-b", "DE"]);
    expect(rows(toggleCountry(on, "LV", offer))).toEqual(["+NL:provider-c", "+AL:provider-b", "DE", "LV"]);
    expect(rows(toggleCountry(toggleCountry(on, "LV", offer), "LV", offer))).toEqual(["+NL:provider-c", "+AL:provider-b", "+LV:provider-a|provider-b", "DE"]);
  });

  test("the last country or provider can be turned off before choosing a replacement", () => {
    const one = toggleCountry(start, "NL", offer);
    expect(chosen(toggleCountry(one, "LV", offer))).toEqual([]);
    const narrowed = toggleProvider(start, "LV", "provider-a", offer);
    expect(chosen(narrowed)[0]?.providers).toEqual(["provider-b"]);
    const deselected = toggleProvider(narrowed, "LV", "provider-b", offer);
    expect(chosen(deselected).map((c) => c.geo)).toEqual(["NL"]);
    expect(chosen(toggleProvider(deselected, "LV", "provider-a", offer)).at(-1)?.providers).toEqual(["provider-a"]);
    expect(chosen(toggleProvider(narrowed, "LV", "provider-a", offer))[0]?.providers).toEqual(["provider-a", "provider-b"]); // the offer's order
    expect(chosen(toggleProvider(start, "AL", "provider-b", offer)).at(-1)).toEqual({ geo: "AL", on: true, providers: ["provider-b"] });
  });

  test("moving keeps the others in order and refuses what is out of range", () => {
    const three = toggleCountry(start, "AL", offer);
    expect(chosen(moveCountry(three, "AL", 0)).map((c) => c.geo)).toEqual(["AL", "LV", "NL"]);
    expect(chosen(moveCountry(three, "LV", 2)).map((c) => c.geo)).toEqual(["NL", "AL", "LV"]);
    expect(moveCountry(three, "LV", 0)).toBe(three);
    expect(moveCountry(three, "LV", 3)).toBe(three);
    expect(moveCountry(three, "DE", 0)).toBe(three); // not chosen
  });
});

describe("now", () => {
  test("a country is available while a chosen provider has an exit up or degraded, at its lowest latency", () => {
    expect(countryNow({ geo: "LV", providers: [] }, exits)).toEqual({ available: true, latencyMs: 38 });
    expect(countryNow({ geo: "LV", providers: ["provider-b"] }, exits)).toEqual({ available: true, latencyMs: 52 });
    expect(countryNow({ geo: "NL", providers: [] }, exits)).toEqual({ available: true, latencyMs: 140 });
    expect(countryNow({ geo: "DE", providers: [] }, exits)).toEqual({ available: false, latencyMs: undefined });
    expect(providerAvailable("DE", "provider-a", exits)).toBe(false);
    expect(providerAvailable("LV", "provider-a", exits)).toBe(true);
  });

  test("a route can use its selection, use a fallback or block traffic", () => {
    const germany = draftOf(route({ strict: true, countries: [{ geo: "DE" }] }), offer);
    expect(routeAvailability(germany, exits)).toBe("blocked");
    expect(routeAvailability({ ...germany, strict: false }, exits)).toBe("fallback");
    expect(routeAvailability(toggleCountry(germany, "NL", offer), exits)).toBe("selected");
    expect(routeAvailability({ ...germany, strict: false }, exits.filter((u) => u.geo === "DE"))).toBe("none");
  });

  test("blocked exits stay on offer but do not contribute to availability or latency", () => {
    const restricted = [{ ...exits[0]!, routingBlock: RoutingBlock.UNAVAILABLE }, ...exits.slice(1)];
    expect(offerOf(restricted, country, true)).toEqual(offer);
    expect(countryNow({ geo: "LV", providers: [] }, restricted)).toEqual({ available: true, latencyMs: 52 });
    expect(providerAvailable("LV", "provider-a", restricted)).toBe(false);
    const sameProvider = { ...exits[0]!, name: "lv-3", latency: durationFromMs(85) };
    expect(countryNow({ geo: "LV", providers: ["provider-a"] }, [...restricted, sameProvider])).toEqual({ available: true, latencyMs: 85 });
  });
});

describe("routeSummary", () => {
  test("reads a route in a line, in either language", () => {
    expect(routeSummary(DEFAULT_ROUTE, en, country)).toBe("Any, fastest");
    expect(routeSummary(route({ countries: [{ geo: "LV" }, { geo: "NL" }] }), en, country)).toBe("Latvia → Netherlands");
    const fastest = route({ fastest: true, strict: true, countries: [{ geo: "LV", providers: ["provider-b"] }, { geo: "NL" }] });
    expect(routeSummary(fastest, en, country)).toBe("Fastest: Latvia (provider-b), Netherlands · strict");
    expect(routeSummary(fastest, en, country, false)).toBe("Fastest: Latvia, Netherlands · strict");
    expect(routeSummary(fastest, ru, formatters("ru").country)).toBe("Самая быстрая: Латвия (provider-b), Нидерланды · строго");
  });
});

test("an unknown or malformed country code stays as it is", () => {
  expect(country("XK")).toBe("Kosovo");
  expect(country("ZZ")).toMatch(/^(ZZ|Unknown Region)$/);
  expect(country("not a code")).toBe("not a code");
});
