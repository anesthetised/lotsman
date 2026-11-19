// A modal dialog. Kobalte supplies the behaviour: focus trap, Escape and
// outside click to close, focus returned to the trigger, aria wiring.

import { Dialog as Kobalte } from "@kobalte/core/dialog";
import type { JSX } from "@solidjs/web";
import { Show } from "solid-js";
import { useI18n } from "../i18n";
import { buttonClass, type ButtonVariant } from "./button";
import { CloseIcon } from "./icons";

export interface DialogProps {
  /** Content of the button that opens the dialog. */
  trigger: JSX.Element;
  title: string;
  description?: string;
  children?: JSX.Element;
  /** Style of the trigger button. */
  variant?: ButtonVariant;
  /** Extra classes for the trigger, such as a larger touch target. */
  triggerClass?: string;
  /** Controlled open state, for content that closes the dialog itself. */
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}

export function Dialog(props: DialogProps) {
  const { t } = useI18n();
  return (
    <Kobalte open={props.open} onOpenChange={(open) => props.onOpenChange?.(open)}>
      <Kobalte.Trigger class={[...buttonClass(props.variant), props.triggerClass]}>{props.trigger}</Kobalte.Trigger>
      <Kobalte.Portal>
        <Kobalte.Overlay class="fixed inset-0 z-40 bg-black/50" />
        <div class="fixed inset-0 z-50 flex items-center justify-center p-4">
          <Kobalte.Content class="max-h-full w-full max-w-lg overflow-auto rounded-lg border border-line bg-surface p-5 shadow-xl">
            <div class="flex items-start justify-between gap-4">
              <Kobalte.Title class="text-lg font-semibold">{props.title}</Kobalte.Title>
              <Kobalte.CloseButton class={buttonClass("ghost")} aria-label={t.common.close}>
                <CloseIcon />
              </Kobalte.CloseButton>
            </div>
            <Show when={props.description}>
              <Kobalte.Description class="mt-1 text-sm text-muted">{props.description}</Kobalte.Description>
            </Show>
            <div class="mt-4">{props.children}</div>
          </Kobalte.Content>
        </div>
      </Kobalte.Portal>
    </Kobalte>
  );
}
