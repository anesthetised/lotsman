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
  /** How long ago `date` was, relative to `now`: "5 seconds ago". */
  ago(date: Date, now: Date): string;
}

export function formatters(locale: string): Formatters {
  const unit = (value: number, name: string) =>
    new Intl.NumberFormat(locale, {
      style: "unit",
      unit: name,
      unitDisplay: "short",
      maximumSignificantDigits: 3,
    }).format(value);
  const relative = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
  const percent = new Intl.NumberFormat(locale, { style: "percent", maximumFractionDigits: 1 });

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
    ago(date, now) {
      const seconds = Math.round((date.getTime() - now.getTime()) / 1000);
      const abs = Math.abs(seconds);
      if (abs < 60) return relative.format(seconds, "second");
      if (abs < 3600) return relative.format(Math.round(seconds / 60), "minute");
      if (abs < 86400) return relative.format(Math.round(seconds / 3600), "hour");
      return relative.format(Math.round(seconds / 86400), "day");
    },
  };
}
