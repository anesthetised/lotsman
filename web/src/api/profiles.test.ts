import { expect, test } from "vitest";
import { groupWarnings, tierString } from "./profiles";
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
