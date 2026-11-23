// Toggle controls: a switch, a chip that is on or off, and a segmented
// control that picks one of a few options. Each is a native button (role
// "switch", or aria-pressed), so it is focusable, works with Space and Enter,
// and reads its state to assistive technology. Every hit target is at least
// 44 px tall, for a thumb; the visible part may be smaller.

import type { JSX } from "@solidjs/web";
import { createUniqueId, For, Show } from "solid-js";

export interface SwitchProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
  /** An accessible name, unless `labelledBy` names it. */
  label?: string;
  labelledBy?: string;
  describedBy?: string;
  disabled?: boolean;
}

export function Switch(props: SwitchProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={props.checked ? "true" : "false"}
      aria-label={props.label}
      aria-labelledby={props.labelledBy}
      aria-describedby={props.describedBy}
      disabled={props.disabled}
      class="inline-flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-md disabled:cursor-not-allowed disabled:opacity-50"
      onClick={() => props.onChange(!props.checked)}
    >
      <span aria-hidden="true" class={["relative h-5 w-9 rounded-full transition-colors", props.checked ? "bg-accent" : "bg-muted"]}>
        <span class={["absolute top-0.5 size-4 rounded-full transition-[left]", props.checked ? "left-4.5 bg-accent-fg" : "left-0.5 bg-surface"]} />
      </span>
    </button>
  );
}

/** A switch with its name and a hint beside it, the name and hint wired to the switch. */
export function SwitchField(props: Omit<SwitchProps, "label" | "labelledBy" | "describedBy"> & { label: string; hint?: string }) {
  const id = createUniqueId();
  return (
    <div class="flex items-start justify-between gap-3">
      <div class="min-w-0 pt-2.5">
        <p id={`${id}-label`} class="text-sm font-medium">
          {props.label}
        </p>
        <Show when={props.hint}>
          <p id={`${id}-hint`} class="mt-0.5 text-xs text-muted">
            {props.hint}
          </p>
        </Show>
      </div>
      <Switch
        checked={props.checked}
        onChange={props.onChange}
        disabled={props.disabled}
        labelledBy={`${id}-label`}
        describedBy={props.hint ? `${id}-hint` : undefined}
      />
    </div>
  );
}

export interface ChipProps {
  pressed: boolean;
  onChange: (pressed: boolean) => void;
  children: JSX.Element;
  /** Marks an unavailable option, with a stronger outline when selected. */
  warn?: boolean;
  disabled?: boolean;
  mono?: boolean;
}

/** A small on/off button, such as a provider of a country. */
export function Chip(props: ChipProps) {
  return (
    <button
      type="button"
      aria-pressed={props.pressed ? "true" : "false"}
      disabled={props.disabled}
      class="group inline-flex min-h-11 items-center rounded-md disabled:cursor-not-allowed"
      onClick={() => props.onChange(!props.pressed)}
    >
      <span
        class={[
          "rounded-md border px-2 py-0.5 text-xs group-disabled:opacity-60",
          { "font-mono": props.mono },
          props.warn
            ? props.pressed
              ? "border-bad bg-bad/10 text-bad"
              : "border-bad/45 text-muted group-hover:text-fg"
            : props.pressed
              ? "border-accent bg-accent/10 text-fg"
              : "border-line text-muted group-hover:text-fg",
        ]}
      >
        {props.children}
      </span>
    </button>
  );
}

export interface SegmentedProps<T extends string> {
  /** Names the group. */
  label: string;
  value: T;
  options: readonly { value: T; label: JSX.Element }[];
  onChange: (value: T) => void;
  disabled?: boolean;
}

/** Picks one of a few options; the options wrap on a narrow screen. */
export function Segmented<T extends string>(props: SegmentedProps<T>) {
  return (
    <div role="group" aria-label={props.label} class="grid grid-cols-[repeat(auto-fit,minmax(8rem,1fr))] gap-1 rounded-lg border border-line bg-bg p-1">
      <For each={props.options}>
        {(option) => (
          <button
            type="button"
            aria-pressed={props.value === option.value ? "true" : "false"}
            disabled={props.disabled}
            class={[
              "min-h-11 rounded-md px-2 py-1 text-sm disabled:cursor-not-allowed",
              props.value === option.value ? "bg-surface font-medium text-fg shadow-[0_0_0_1px_var(--color-line)]" : "text-muted hover:text-fg",
            ]}
            onClick={() => props.onChange(option.value)}
          >
            {option.label}
          </button>
        )}
      </For>
    </div>
  );
}
