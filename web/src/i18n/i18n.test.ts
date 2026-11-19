import { describe, expect, test } from "vitest";
import { en } from "./en";
import { formatters } from "./format";
import { i18n, locales, pickLocale } from "./index";

describe("pickLocale", () => {
  test.each([
    [["ru-RU", "en-US"], "ru"],
    [["en-GB", "ru"], "en"],
    [["de-DE", "RU"], "ru"],
    [["de-DE", "fr"], "en"],
    [[], "en"],
  ])("%j → %s", (languages, want) => {
    expect(pickLocale(languages)).toBe(want);
  });
});

/** The dictionary's shape: key paths, with functions marked by their arity. */
function shape(value: unknown, path = ""): string[] {
  if (typeof value === "function") return [`${path}(${value.length})`];
  if (typeof value === "object" && value !== null) return Object.entries(value).flatMap(([k, v]) => shape(v, `${path}.${k}`));
  return [path];
}

test("every dictionary has the shape of the English one", () => {
  // The Dict type checks this at compile time; this guards against casts.
  for (const locale of locales) expect(shape(i18n(locale).t)).toEqual(shape(en));
});

describe("formatters", () => {
  const enf = formatters("en");
  const ruf = formatters("ru");

  test("rate shows bits per second with an SI prefix", () => {
    expect(enf.rate(0)).toBe("0 bit/s");
    expect(enf.rate(125)).toBe("1 kb/s");
    expect(enf.rate(1_562_500)).toBe("12.5 Mb/s");
    // CLDR spells the Russian unit with a Latin "c" (Мбит/c); accept either.
    expect(ruf.rate(1_562_500)).toMatch(/^12,5 Мбит\/[сc]$/);
    expect(enf.rate(250_000_000_000)).toBe("2,000 Gb/s");
  });

  test("bytes", () => {
    expect(enf.bytes(999)).toBe("999 byte");
    expect(enf.bytes(2_500_000_000)).toBe("2.5 GB");
    expect(ruf.bytes(2_500_000_000)).toBe("2,5 ГБ");
  });

  test("ago picks a unit by distance", () => {
    const now = new Date("2026-11-19T12:00:00Z");
    expect(enf.ago(new Date("2026-11-19T11:59:30Z"), now)).toBe("30 seconds ago");
    expect(enf.ago(new Date("2026-11-19T11:00:00Z"), now)).toBe("1 hour ago");
    expect(ruf.ago(new Date("2026-11-17T12:00:00Z"), now)).toBe("позавчера");
  });

  test("milliseconds and percent", () => {
    expect(enf.milliseconds(41.6)).toBe("42 ms");
    expect(ruf.percent(0.993)).toBe("99,3 %");
  });
});
