// The frame of the sign-in and enrollment pages, and how they show a failure.

import type { JSX } from "@solidjs/web";
import { Show } from "solid-js";
import { failure } from "../../api/auth";
import { useI18n } from "../../i18n";

export function AuthCard(props: { title: string; intro: string; children?: JSX.Element }) {
  const { t } = useI18n();
  return (
    <main id="main" class="mx-auto max-w-md px-4 py-16 text-sm">
      <p class="mb-6 text-center font-semibold">{t.app.name}</p>
      <div class="space-y-4 rounded-lg border border-line bg-surface p-6">
        <h1 class="text-lg font-semibold">{props.title}</h1>
        <p class="text-muted">{props.intro}</p>
        {props.children}
      </div>
    </main>
  );
}

/** The failure of a sign-in, enrollment or passkey call, in words; nothing without one. */
export function FailureMessage(props: { error: unknown }) {
  const { t } = useI18n();
  const text = () => {
    if (props.error === undefined) return undefined;
    const kind = failure(props.error);
    if (kind !== "failed") return t.auth[kind];
    return t.auth.failed(props.error instanceof Error ? props.error.message : String(props.error));
  };
  return (
    <Show when={text()}>
      {(message) => (
        <p role="alert" class="rounded-md border border-bad p-3">
          {message()}
        </p>
      )}
    </Show>
  );
}
