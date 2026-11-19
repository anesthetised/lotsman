// Controls on the user page are opened with a thumb: at least 44 px tall.

import { buttonClass, type ButtonVariant } from "../../ui/button";

export const touch = "min-h-11";

export function touchButton(variant?: ButtonVariant): string[] {
  return [...buttonClass(variant), touch, "px-4"];
}

export const fieldClass = "min-h-11 w-full rounded-md border border-line bg-bg px-3 py-2 text-base";
