import { expect, test } from "vitest";
import { groupWarnings, suggestions, tierProblem, tierString } from "./profiles";
import { profileWarnings } from "./simulation";

test("tierString writes a tier as the CLI does", () => {
  expect(tierString({ geo: "NL", provider: "", tag: "" })).toBe("geo=NL");
  expect(tierString({ geo: "DE", provider: "acme", tag: "eu" })).toBe("geo=DE,provider=acme,tag=eu");
  expect(tierString({ geo: "", provider: "", tag: "" })).toBe("any");
});

test("groupWarnings recognises the daemon's tier warnings and keeps the rest", () => {
  const { byProfile, other } = groupWarnings([
    'profile "eu": tier 2 geo=FI matches no upstream and is ignored',
    'profile "eu": tier 3 geo=LV never applies: every upstream it matches is taken by tier 1 tag=near, tier 2 geo=FI',
    'profile "odd": something new',
    "a warning about nothing in particular",
  ]);
  expect(byProfile.get("eu")).toEqual([
    { kind: "unmatched", tier: 2, match: "geo=FI", text: 'profile "eu": tier 2 geo=FI matches no upstream and is ignored' },
    {
      kind: "shadowed",
      tier: 3,
      match: "geo=LV",
      by: "tier 1 tag=near, tier 2 geo=FI",
      text: 'profile "eu": tier 3 geo=LV never applies: every upstream it matches is taken by tier 1 tag=near, tier 2 geo=FI',
    },
  ]);
  expect(byProfile.get("odd")).toEqual([{ kind: "other", text: 'profile "odd": something new' }]);
  expect(other).toEqual(["a warning about nothing in particular"]);
});

test("the fake's warnings parse back", () => {
  const warnings = profileWarnings([{ name: "x", prefer: [{ geo: "", provider: "", tag: "near" }, { geo: "DE", provider: "", tag: "" }, { geo: "FI", provider: "", tag: "" }] }]);
  expect(groupWarnings(warnings).byProfile.get("x")?.map((w) => [w.kind, w.tier])).toEqual([
    ["shadowed", 2],
    ["unmatched", 3],
  ]);
});

const upstreams = [
  { geo: "NL", provider: "acme", tags: ["eu", "fast"] },
  { geo: "NL", provider: "other", tags: ["eu"] },
  { geo: "DE", provider: "acme", tags: [] },
];
const tier = (t: { geo?: string; provider?: string; tag?: string }) => ({ geo: "", provider: "", tag: "", ...t });

test("suggestions offer the values that keep the tier matching an upstream", () => {
  expect(suggestions(tier({}), "geo", upstreams)).toEqual(["DE", "NL"]);
  expect(suggestions(tier({ geo: "NL" }), "provider", upstreams)).toEqual(["acme", "other"]);
  expect(suggestions(tier({ geo: "DE" }), "provider", upstreams)).toEqual(["acme"]);
  expect(suggestions(tier({ provider: "acme" }), "tag", upstreams)).toEqual(["eu", "fast"]);
  // The field's own value does not narrow its suggestions.
  expect(suggestions(tier({ geo: "N" }), "geo", upstreams)).toEqual(["DE", "NL"]);
  expect(suggestions(tier({ geo: "FI" }), "provider", upstreams)).toEqual([]);
});

test("tierProblem refuses what the daemon refuses to write", () => {
  expect(tierProblem(tier({}), upstreams)).toBeUndefined();
  expect(tierProblem(tier({ geo: "NL", tag: "fast" }), upstreams)).toBeUndefined();
  expect(tierProblem(tier({ geo: "DE", tag: "eu" }), upstreams)).toEqual({ kind: "unmatched" });
  expect(tierProblem(tier({ geo: "N L" }), upstreams)).toEqual({ kind: "value", field: "geo" });
  expect(tierProblem(tier({ provider: "a=b" }), upstreams)).toEqual({ kind: "value", field: "provider" });
  expect(tierProblem(tier({ tag: "EU" }), upstreams)).toEqual({ kind: "value", field: "tag" });
  // Nothing matches without upstreams, not even any.
  expect(tierProblem(tier({}), [])).toEqual({ kind: "unmatched" });
});
