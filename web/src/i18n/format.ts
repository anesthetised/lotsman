// Locale-aware number formatting. Intl supplies the unit names ("Mbit/s",
// "Мбит/с"), so the dictionaries carry no units.

const rateUnits = ["bit", "kilobit", "megabit", "gigabit"] as const;
const sizeUnits = ["byte", "kilobyte", "megabyte", "gigabyte", "terabyte"] as const;

/** Splits a value into a mantissa below 1000 and the index of its SI unit. */
function scale(value: number, units: number): [number, number] {
  let i = 0;
  while (Math.abs(value) >= 1000 && i < units - 1) {
    value /= 1000;
    i++;
  }
  return [value, i];
}

export interface Formatters {
  /** A transfer rate given in bytes per second, shown in bits per second. */
  rate(bytesPerSecond: number): string;
  bytes(bytes: number): string;
  milliseconds(ms: number): string;
  /** A ratio in [0, 1] as a percentage. */
  percent(ratio: number): string;
  /** A length of time in its two largest units, rounded down: "45 sec", "12 min", "2 hr 5 min", "3 days 4 hr". */
  duration(ms: number): string;
  /** How long ago `date` was, relative to `now`: "5 seconds ago". */
  ago(date: Date, now: Date): string;
  /** A date and time of day: "Nov 19, 2026, 12:15 PM". */
  dateTime(date: Date, withSeconds?: boolean): string;
  /** A time of day with seconds: "12:15:03 PM". */
  time(date: Date): string;
  /** A count: "1,234". */
  count(n: number): string;
  /** A country from its ISO 3166-1 alpha-2 code: "LV" is "Latvia"; an unknown code stays as it is. */
  country(code: string): string;
}

export function formatters(locale: string, timeZone?: string): Formatters {
  const unit = (value: number, name: string) =>
    new Intl.NumberFormat(locale, {
      style: "unit",
      unit: name,
      unitDisplay: "short",
      maximumSignificantDigits: 3,
    }).format(value);
  const relative = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
  const percent = new Intl.NumberFormat(locale, { style: "percent", maximumFractionDigits: 1 });
  const dateTime = new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeStyle: "short", timeZone });
  const preciseDateTime = new Intl.DateTimeFormat(locale, {
    year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23", timeZone,
  });
  const time = new Intl.DateTimeFormat(locale, { timeStyle: "medium", timeZone });
  const count = new Intl.NumberFormat(locale);
  const regions = new Intl.DisplayNames(locale, { type: "region", fallback: "code" });

  return {
    rate(bytesPerSecond) {
      const [v, i] = scale(bytesPerSecond * 8, rateUnits.length);
      return unit(v, `${rateUnits[i]}-per-second`);
    },
    bytes(bytes) {
      const [v, i] = scale(bytes, sizeUnits.length);
      return unit(v, sizeUnits[i]!);
    },
    milliseconds(ms) {
      return unit(Math.round(ms), "millisecond");
    },
    percent(ratio) {
      return percent.format(ratio);
    },
    duration(ms) {
      const seconds = Math.max(0, Math.floor(ms / 1000));
      const [m, h, d] = [Math.floor(seconds / 60), Math.floor(seconds / 3600), Math.floor(seconds / 86400)];
      // Whole units: no rounding to significant digits as for rates.
      const whole = (value: number, name: string) => new Intl.NumberFormat(locale, { style: "unit", unit: name, unitDisplay: "short" }).format(value);
      const pair = (big: number, bigUnit: string, small: number, smallUnit: string) =>
        small > 0 ? `${whole(big, bigUnit)} ${whole(small, smallUnit)}` : whole(big, bigUnit);
      if (seconds < 60) return whole(seconds, "second");
      if (seconds < 3600) return whole(m, "minute");
      if (seconds < 86400) return pair(h, "hour", m % 60, "minute");
      return pair(d, "day", h % 24, "hour");
    },
    ago(date, now) {
      const seconds = Math.round((date.getTime() - now.getTime()) / 1000);
      const abs = Math.abs(seconds);
      if (abs < 60) return relative.format(seconds, "second");
      if (abs < 3600) return relative.format(Math.round(seconds / 60), "minute");
      if (abs < 86400) return relative.format(Math.round(seconds / 3600), "hour");
      return relative.format(Math.round(seconds / 86400), "day");
    },
    dateTime(date, withSeconds) {
      return (withSeconds ? preciseDateTime : dateTime).format(date);
    },
    time(date) {
      return time.format(date);
    },
    count(n) {
      return count.format(n);
    },
    country(code) {
      try {
        return regions.of(code) ?? code;
      } catch {
        return code; // not a well-formed region code
      }
    },
  };
}
