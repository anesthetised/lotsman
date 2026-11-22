// A dropdown menu that picks one value (menuitemradio items). Kobalte supplies
// keyboard navigation, typeahead, focus management and dismissal.

import { DropdownMenu } from "@kobalte/core/dropdown-menu";
import type { JSX } from "@solidjs/web";
import { For } from "solid-js";
import { buttonClass } from "./button";
import { CheckIcon, ChevronDownIcon } from "./icons";

export interface Choice<T extends string> {
  value: T;
  label: string;
  lang?: string;
}

export interface ChoiceMenuProps<T extends string> {
  /** Trigger label. With an icon, it is visually hidden. */
  label: JSX.Element;
  icon?: JSX.Element;
  value: T;
  choices: readonly Choice<T>[];
  onChange: (value: T) => void;
}

export function ChoiceMenu<T extends string>(props: ChoiceMenuProps<T>) {
  return (
    <DropdownMenu placement={props.icon ? "bottom-end" : undefined}>
      <DropdownMenu.Trigger
        class={props.icon ? [...buttonClass("ghost"), "size-11 cursor-pointer data-[expanded]:bg-bg data-[expanded]:text-fg"] : buttonClass()}
        title={props.icon && typeof props.label === "string" ? props.label : undefined}
      >
        <span class={props.icon ? "sr-only" : undefined}>{props.label}</span>
        {props.icon ?? <ChevronDownIcon />}
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content class="z-50 min-w-36 rounded-md border border-line bg-surface p-1 shadow-lg">
          <DropdownMenu.RadioGroup value={props.value} onChange={(v: string) => props.onChange(v as T)}>
            <For each={props.choices}>
              {(choice) => (
                <DropdownMenu.RadioItem
                  value={choice.value}
                  closeOnSelect
                  class={[
                    "flex cursor-default items-center gap-2 rounded px-2 py-1.5 text-sm outline-none data-[highlighted]:bg-accent data-[highlighted]:text-accent-fg",
                    props.icon && "min-h-11",
                  ]}
                >
                  <span class="size-4">
                    <DropdownMenu.ItemIndicator>
                      <CheckIcon />
                    </DropdownMenu.ItemIndicator>
                  </span>
                  <DropdownMenu.ItemLabel lang={choice.lang}>{choice.label}</DropdownMenu.ItemLabel>
                </DropdownMenu.RadioItem>
              )}
            </For>
          </DropdownMenu.RadioGroup>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu>
  );
}
