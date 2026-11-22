// The user page behind the secret link `/u#<token>`: mobile first, one
// column on a phone and two on a wider screen. The token is in the fragment,
// so the browser never sends it with the page (see link.ts). Every call sends
// it to the panel, and only there, as `Authorization: Bearer` (see selfApi);
// the panel resolves it and scopes UserSelfService to that user.

import { create } from "@bufbuild/protobuf";
import { createMemo, createSignal, Errored, Loading, refresh, Show, untrack } from "solid-js";
import { selfApi } from "../../api/client";
import { linkToken } from "../../api/link";
import { createLive, num } from "../../api/live";
import { RightsSchema } from "../../gen/lotsman/v1/user_pb";
import { DisplayPreferences } from "../../ui/DisplayPreferences";
import { useI18n } from "../../i18n";
import { buttonClass } from "../../ui/button";
import { SkipLink } from "../SkipLink";
import { AddDevice } from "./AddDevice";
import { type Issued, type Outcome, UserProvider } from "./context";
import { Devices, IssuedConfig } from "./Devices";
import { failureOf, failureText, type Failure } from "./errors";
import { Exits } from "./Exits";
import { HowTo } from "./HowTo";
import { History, Speed } from "./Traffic";

export default function UserPage() {
  const { t } = useI18n();
  // Read once and kept in memory only; the address keeps it for bookmarks.
  const token = linkToken(location.hash);
  return (
    <div class="min-h-screen">
      <SkipLink />
      <header class="border-b border-line bg-surface">
        <div class="mx-auto flex max-w-5xl flex-wrap items-center justify-between gap-2 px-4 py-1">
          <span class="text-sm font-semibold">{t.app.name}</span>
          <DisplayPreferences />
        </div>
      </header>
      <main id="main" class="mx-auto max-w-5xl space-y-4 px-4 py-6">
        <Show when={token} fallback={<Problem text={t.user.badLink} />}>
          {(token) => (
            <Errored fallback={(error, reset) => <FailureView failure={failureOf(error())} retry={reset} />}>
              <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
                <LiveUser token={token()} />
              </Loading>
            </Errored>
          )}
        </Show>
      </main>
    </div>
  );
}

function Problem(props: { text: string }) {
  return (
    <p role="alert" class="rounded-md border border-bad bg-surface p-3">
      {props.text}
    </p>
  );
}

/** A failure that stops the page: a dead link has no way out, anything else can be retried. */
function FailureView(props: { failure: Failure; retry: () => void }) {
  const { t } = useI18n();
  return (
    <div role="alert" class="space-y-3 rounded-md border border-bad bg-surface p-4">
      <p>{failureText(t, props.failure)}</p>
      <Show when={props.failure.kind !== "revoked"}>
        <button type="button" class={[...buttonClass(), "min-h-11 px-4"]} onClick={() => props.retry()}>
          {t.common.retry}
        </button>
      </Show>
    </div>
  );
}

function LiveUser(props: { token: string }) {
  const { t } = useI18n();
  // The token is fixed for the page's life: read once, on purpose untracked.
  const self = selfApi(untrack(() => props.token));
  const user = createMemo(() => self.get({}));
  // The daemon always sends the rights; without them, the defaults.
  const rights = createMemo(() => user().rights ?? create(RightsSchema, { editRoutes: true, showProviders: true }));
  const snapshot = createLive((signal) => self.watch({}, { signal }));
  const trend = createMemo<number[]>((prev = []) => [...prev, num(snapshot().total?.downRate)].slice(-60));
  const peers = createMemo(() => new Map(snapshot().peers.map((p) => [p.peerId, p])));
  const upstreams = createMemo(() => new Map(snapshot().upstreams.map((u) => [u.name, u])));

  const [issued, issue] = createSignal<Issued>();
  const [busy, setBusy] = createSignal(false);
  const [dead, setDead] = createSignal(false);

  const mutate = async <T,>(task: () => Promise<T>): Promise<Outcome<T>> => {
    setBusy(true);
    try {
      return { ok: true, value: await task() };
    } catch (e) {
      const failure = failureOf(e);
      if (failure.kind === "revoked") setDead(true);
      return { ok: false, failure };
    } finally {
      setBusy(false);
      // Whatever happened, the devices may differ now: a change can stop halfway.
      if (!dead()) refresh(user);
    }
  };

  return (
    <Show when={!dead()} fallback={<FailureView failure={{ kind: "revoked" }} retry={() => {}} />}>
      <UserProvider
        value={{
          self,
          user,
          rights,
          peer: (id) => peers().get(id),
          upstream: (name) => upstreams().get(name),
          exits: () => {
            const allowed = rights().exits;
            return allowed.length ? snapshot().upstreams.filter((u) => allowed.includes(u.name)) : snapshot().upstreams;
          },
          configs: new Map(),
          issued,
          issue,
          busy,
          mutate,
        }}
      >
        <h1 class="text-2xl font-semibold">{t.user.title(user().name)}</h1>
        <div class="grid gap-4 lg:grid-cols-2 lg:items-start lg:gap-6">
          <div class="space-y-4">
            <Speed snapshot={snapshot()} trend={trend()} />
            <section aria-labelledby="devices" class="space-y-3">
              <h2 id="devices" class="text-sm text-muted">
                {t.user.devices}
              </h2>
              <IssuedConfig />
              <Devices />
              <AddDevice />
            </section>
          </div>
          <div class="space-y-4">
            <History />
            <Exits />
            <HowTo />
          </div>
        </div>
      </UserProvider>
    </Show>
  );
}
