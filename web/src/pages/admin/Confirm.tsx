// A confirmation dialog for a destructive action. It stays open with the
// failure in words when the action fails, and closes when it succeeds.

import type { JSX } from "@solidjs/web";
import { createSignal } from "solid-js";
import { useI18n } from "../../i18n";
import { buttonClass, type ButtonVariant } from "../../ui/button";
import { Dialog } from "../../ui/Dialog";
import { ApiError } from "./ApiError";

export interface ConfirmProps {
  trigger: JSX.Element;
  title: string;
  description?: string;
  /** The label of the confirming button. */
  confirm: string;
  variant?: ButtonVariant;
  children?: JSX.Element;
  /** Does the work; a rejection keeps the dialog open with the error. */
  action: () => Promise<unknown>;
  /** Words for a failure instead of the daemon's message, or undefined to keep it. */
  failure?: (error: unknown) => string | undefined;
}

export function Confirm(props: ConfirmProps) {
  const { t } = useI18n();
  const [open, setOpen] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();
  const openChange = (next: boolean) => {
    setOpen(next);
    setError(undefined);
  };
  const run = async () => {
    setBusy(true);
    setError(undefined);
    try {
      await props.action();
      setOpen(false);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      open={open()}
      onOpenChange={openChange}
      variant={props.variant ?? "compact"}
      trigger={props.trigger}
      title={props.title}
      description={props.description}
    >
      <div class="space-y-3 text-sm">
        {props.children}
        <ApiError error={error()} text={error() === undefined ? undefined : props.failure?.(error())} />
        <div class="flex justify-end gap-2">
          <button type="button" class={buttonClass()} onClick={() => setOpen(false)}>
            {t.admin.cancel}
          </button>
          <button type="button" class={buttonClass("danger")} disabled={busy()} onClick={run}>
            {props.confirm}
          </button>
        </div>
      </div>
    </Dialog>
  );
}
