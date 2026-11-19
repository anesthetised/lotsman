// The operator sign-in page: one button, the browser's passkey prompt.

import { useNavigate } from "@solidjs/router";
import { createSignal, Show } from "solid-js";
import { returnPath } from "../../api/auth";
import { auth } from "../../api/client";
import { useI18n } from "../../i18n";
import { buttonClass } from "../../ui/button";
import { AuthCard, FailureMessage } from "./AuthCard";

export default function SignIn() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();

  const signIn = async () => {
    setBusy(true);
    setError(undefined);
    try {
      await auth.signIn();
      navigate(returnPath(new URLSearchParams(location.search).get("next")), { replace: true });
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthCard title={t.auth.signInTitle} intro={t.auth.signInIntro}>
      <button type="button" class={buttonClass("primary")} disabled={busy()} onClick={signIn}>
        {t.auth.signIn}
      </button>
      <Show when={busy()}>
        <p role="status" class="text-muted">
          {t.auth.waiting}
        </p>
      </Show>
      <FailureMessage error={error()} />
    </AuthCard>
  );
}
