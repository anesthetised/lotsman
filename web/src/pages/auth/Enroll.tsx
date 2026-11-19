// Redeems a one-time enrollment link, /enroll#<token>: names a new passkey
// and creates it. The token is in the fragment, which the browser never
// sends to the server, so no proxy logs it.

import { useNavigate } from "@solidjs/router";
import { createSignal, Show } from "solid-js";
import { auth } from "../../api/client";
import { useI18n } from "../../i18n";
import { buttonClass } from "../../ui/button";
import { AuthCard, FailureMessage } from "./AuthCard";

export default function Enroll() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const token = decodeURIComponent(location.hash.slice(1));
  const [name, setName] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();

  const enroll = async (e: SubmitEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await auth.enroll(token, name());
      // Replace the entry, so the used link leaves the history.
      navigate("/admin", { replace: true });
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthCard title={t.auth.enrollTitle} intro={t.auth.enrollIntro}>
      <Show
        when={token}
        fallback={
          <p role="alert" class="rounded-md border border-bad p-3">
            {t.auth.noToken}
          </p>
        }
      >
        <form class="space-y-4" onSubmit={enroll}>
          <div class="space-y-1">
            <label for="passkey-name" class="block font-medium">
              {t.auth.name}
            </label>
            <input
              id="passkey-name"
              required
              maxlength={64}
              autocomplete="off"
              class="w-full rounded-md border border-line bg-bg px-3 py-1.5"
              value={name()}
              onInput={(e) => setName(e.currentTarget.value)}
            />
          </div>
          <button type="submit" class={buttonClass("primary")} disabled={busy()}>
            {t.auth.enroll}
          </button>
        </form>
        <Show when={busy()}>
          <p role="status" class="text-muted">
            {t.auth.waiting}
          </p>
        </Show>
        <FailureMessage error={error()} />
      </Show>
    </AuthCard>
  );
}
