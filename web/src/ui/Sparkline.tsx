// A tiny trend line in plain SVG, for table cells and cards.

const WIDTH = 100;
const HEIGHT = 24;

/** The SVG path of `values` scaled into the box, the largest value at the top. */
export function sparklinePath(values: readonly number[], width = WIDTH, height = HEIGHT): string {
  if (values.length === 0) return "";
  const max = Math.max(...values, 1);
  const step = values.length > 1 ? width / (values.length - 1) : 0;
  return values
    .map((v, i) => `${i === 0 ? "M" : "L"}${(i * step).toFixed(1)},${(height - (v / max) * height).toFixed(1)}`)
    .join("");
}

export interface SparklineProps {
  values: readonly number[];
  /** Accessible name, such as what the line shows and over which period. */
  label: string;
  /** Size classes; a table cell's size by default. */
  size?: string;
}

export function Sparkline(props: SparklineProps) {
  return (
    <svg
      role="img"
      aria-label={props.label}
      viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
      preserveAspectRatio="none"
      class={[props.size ?? "h-6 w-24", "overflow-visible text-chart-down"]}
    >
      <path d={sparklinePath(props.values)} fill="none" stroke="currentColor" stroke-width="1.5" vector-effect="non-scaling-stroke" />
    </svg>
  );
}
