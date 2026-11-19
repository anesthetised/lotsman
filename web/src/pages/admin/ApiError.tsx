// How the console shows a failed call: the daemon's own message, which
// names what to fix (it already speaks the CLI's terms).

import { ConnectError } from "@connectrpc/connect";
import { Show } from "solid-js";
import { useConsoleI18n } from "../../i18n/console";

export function errorMessage(error: unknown): string {
  return ConnectError.from(error).rawMessage;
}

/** The failure in words; nothing without one. `text` overrides the daemon's message. */
export function ApiError(props: { error: unknown; text?: string }) {
  const { c } = useConsoleI18n();
  return (
    <Show when={props.error !== undefined}>
      <p role="alert" class="rounded-md border border-bad p-3 text-sm">
        {props.text ?? c.failed(errorMessage(props.error))}
      </p>
    </Show>
  );
}
