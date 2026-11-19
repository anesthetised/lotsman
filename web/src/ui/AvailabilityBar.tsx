// An availability strip in plain SVG: one segment per sample, oldest first,
// coloured by how much of that step the upstream was up.

import { For } from "solid-js";

export type Level = "ok" | "warn" | "bad" | "none";

/** `undefined` means no sample for the step. */
export function availabilityLevel(ratio: number | undefined): Level {
  if (ratio === undefined) return "none";
  if (ratio >= 0.99) return "ok";
  if (ratio >= 0.9) return "warn";
  return "bad";
}

const fill: Record<Level, string> = {
  ok: "fill-ok",
  warn: "fill-warn",
  bad: "fill-bad",
  none: "fill-empty",
};

export interface AvailabilityBarProps {
  ratios: readonly (number | undefined)[];
  /** Accessible name, including a summary such as the overall percentage. */
  label: string;
}

export function AvailabilityBar(props: AvailabilityBarProps) {
  return (
    <svg
      role="img"
      aria-label={props.label}
      viewBox={`0 0 ${Math.max(props.ratios.length, 1)} 10`}
      preserveAspectRatio="none"
      class="h-4 w-full min-w-24"
    >
      <For each={props.ratios}>
        {(ratio, i) => <rect x={i()} width="0.85" height="10" class={fill[availabilityLevel(ratio)]} />}
      </For>
    </svg>
  );
}
