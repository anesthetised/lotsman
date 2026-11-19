// The operators' passkeys: who can sign in, revoking one, and issuing a
// one-time link that enrolls another.

import { useNavigate } from "@solidjs/router";
import { createMemo, createSignal, Errored, For, Loading, refresh, Show } from "solid-js";
import type { EnrollmentLink, Passkey } from "../../api/auth";
import { auth } from "../../api/client";
import { useI18n } from "../../i18n";
import { buttonClass } from "../../ui/button";
import { Dialog } from "../../ui/Dialog";
import { FailureMessage } from "../auth/AuthCard";
import { LiveError } from "../LiveError";

export default function Passkeys() {
  const { t } = useI18n();
  return (
    <div class="space-y-4">
      <div class="flex items-center justify-between gap-4">
        <h1 class="text-lg font-semibold">{t.admin.passkeys}</h1>
        <IssueLink />
      </div>
      <p class="text-muted">{t.admin.passkeysIntro}</p>
      <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
        <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
          <PasskeyTable />
        </Loading>
      </Errored>
    </div>
  );
}

function PasskeyTable() {
  const { t, fmt } = useI18n();
  const navigate = useNavigate();
  const passkeys = createMemo(() => auth.passkeys());
  const revoked = (signedOut: boolean) => (signedOut ? navigate("/login", { replace: true }) : refresh(passkeys));
  return (
    <section class="overflow-x-auto rounded-lg border border-line bg-surface">
      <table class="w-full min-w-[36rem]">
        <thead class="text-left text-xs text-muted">
          <tr>
            <th scope="col" class="px-3 py-2 font-medium">{t.admin.name}</th>
            <th scope="col" class="px-3 py-2 font-medium">{t.admin.created}</th>
            <th scope="col" class="px-3 py-2 font-medium">{t.admin.lastUsed}</th>
            <th scope="col" class="px-3 py-2">
              <span class="sr-only">{t.admin.revoke}</span>
            </th>
          </tr>
        </thead>
        <tbody>
          <For each={passkeys()} keyed={(p) => p.id}>
            {(p) => (
              <tr class="border-t border-line">
                <th scope="row" class="px-3 py-1.5 text-left font-medium">
                  {p().name}
                  <Show when={p().current}>
                    {" "}
                    <span class="text-xs font-normal text-muted">({t.admin.thisSession})</span>
                  </Show>
                </th>
                <td class="px-3 py-1.5">{fmt.dateTime(new Date(p().createdAt))}</td>
                <td class="px-3 py-1.5">
                  <Show when={p().usedAt} fallback={t.admin.never}>
                    {(used) => <>{fmt.dateTime(new Date(used()))}</>}
                  </Show>
                </td>
                <td class="px-3 py-1.5 text-right">
                  <Revoke passkey={p()} last={passkeys().length === 1} onRevoked={revoked} />
                </td>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </section>
  );
}

function Revoke(props: { passkey: Passkey; last: boolean; onRevoked: (signedOut: boolean) => void }) {
  const { t } = useI18n();
  const [open, setOpen] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();
  const revoke = async () => {
    setBusy(true);
    setError(undefined);
    try {
      const { signedOut } = await auth.revoke(props.passkey.id);
      setOpen(false);
      props.onRevoked(signedOut);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      open={open()}
      onOpenChange={setOpen}
      trigger={
        <>
          {t.admin.revoke}
          <span class="sr-only"> {props.passkey.name}</span>
        </>
      }
      title={t.admin.revokeTitle(props.passkey.name)}
      description={t.admin.revokeDescription}
    >
      <div class="space-y-3 text-sm">
        <Show when={props.passkey.current}>
          <p>{t.admin.revokeSelf}</p>
        </Show>
        <Show when={props.last}>
          <p>{t.admin.revokeLast}</p>
        </Show>
        <FailureMessage error={error()} />
        <div class="flex justify-end gap-2">
          <button type="button" class={buttonClass()} onClick={() => setOpen(false)}>
            {t.admin.cancel}
          </button>
          <button type="button" class={buttonClass("danger")} disabled={busy()} onClick={revoke}>
            {t.admin.revoke}
          </button>
        </div>
      </div>
    </Dialog>
  );
}

function IssueLink() {
  const { t, fmt } = useI18n();
  const [open, setOpen] = createSignal(false);
  const [link, setLink] = createSignal<EnrollmentLink>();
  const [error, setError] = createSignal<unknown>();
  const [copied, setCopied] = createSignal(false);
  const openChange = async (next: boolean) => {
    setOpen(next);
    setLink(undefined);
    setError(undefined);
    setCopied(false);
    if (!next) return;
    try {
      setLink(await auth.issueEnrollment());
    } catch (e) {
      setError(e);
    }
  };
  const copy = async (url: string) => {
    await navigator.clipboard.writeText(url);
    setCopied(true);
  };
  return (
    <Dialog open={open()} onOpenChange={openChange} variant="primary" trigger={t.admin.issueLink} title={t.admin.linkTitle}>
      <div class="space-y-3 text-sm">
        <FailureMessage error={error()} />
        <Show when={link()} fallback={<Show when={!error()}>{<p class="text-muted">{t.common.connecting}</p>}</Show>}>
          {(l) => (
            <>
              <p class="text-muted">{t.admin.linkDescription(fmt.dateTime(new Date(l().expiresAt)))}</p>
              <label for="enroll-link" class="block font-medium">
                {t.admin.linkLabel}
              </label>
              <div class="flex gap-2">
                <input
                  id="enroll-link"
                  readonly
                  value={l().url}
                  class="w-full rounded-md border border-line bg-bg px-3 py-1.5 font-mono text-xs"
                  onFocus={(e) => e.currentTarget.select()}
                />
                <button type="button" class={buttonClass()} onClick={() => copy(l().url)}>
                  {copied() ? t.admin.copied : t.admin.copy}
                </button>
              </div>
            </>
          )}
        </Show>
      </div>
    </Dialog>
  );
}
