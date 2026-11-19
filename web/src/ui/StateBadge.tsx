// A coloured dot with a text label: the label carries the meaning, so the
// state never depends on colour alone.

import type { Level } from "./AvailabilityBar";

const dot: Record<Level, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  bad: "bg-bad",
  none: "bg-empty",
};

export function StateBadge(props: { level: Level; label: string }) {
  return (
    <span class="inline-flex items-center gap-1.5 whitespace-nowrap">
      <span aria-hidden="true" class={["size-2 rounded-full", dot[props.level]]} />
      {props.label}
    </span>
  );
}
