// Adding a device: a name and a profile. The daemon answers with the new
// config, which the devices section shows at once.

import { createMemo, createSignal, Errored, For, Loading, Show } from "solid-js";
import { useI18n } from "../../i18n";
import { useUser } from "./context";
import type { Failure } from "./errors";
import { FailureAlert } from "./FailureAlert";
import { fieldClass, touchButton } from "./touch";

export function AddDevice() {
  const { t } = useI18n();
  return (
    <section aria-labelledby="add-title" class="space-y-3 rounded-xl border border-line bg-surface p-4">
      <h3 id="add-title" class="text-lg font-medium">
        {t.user.add.title}
      </h3>
      <Errored fallback={(error, reset) => <ProfilesError retry={reset} error={error()} />}>
        <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
          <Form />
        </Loading>
      </Errored>
    </section>
  );
}

function ProfilesError(props: { error: unknown; retry: () => void }) {
  const { t } = useI18n();
  return (
    <div role="alert" class="flex flex-wrap items-center gap-3 rounded-md border border-bad p-3 text-sm">
      <span>{t.common.liveError(props.error instanceof Error ? props.error.message : String(props.error))}</span>
      <button type="button" class={touchButton()} onClick={() => props.retry()}>
        {t.common.retry}
      </button>
    </div>
  );
}

function Form() {
  const { t } = useI18n();
  const ctx = useUser();
  const [name, setName] = createSignal("");
  const [profile, setProfile] = createSignal("");
  const [failure, setFailure] = createSignal<Failure>();
  const [adding, setAdding] = createSignal(false);
  /** Adding is off for good once the daemon says so: the operator set `max_devices: 0`. */
  const off = createMemo(() => failure()?.kind === "denied");
  const chosen = () => (ctx.profiles().includes(profile()) ? profile() : ctx.profiles()[0]);

  const submit = async (e: SubmitEvent) => {
    e.preventDefault();
    const to = chosen();
    if (!to || off()) return;
    setFailure(undefined);
    setAdding(true);
    const device = name().trim();
    const outcome = await ctx.mutate(() => ctx.self.addDevice({ device, profile: to }));
    setAdding(false);
    if (!outcome.ok) {
      setFailure(outcome.failure);
      return;
    }
    ctx.configs.set(outcome.value.peerId, outcome.value.config);
    ctx.issue({ kind: "added", device, profile: to, config: outcome.value.config });
    setName("");
  };

  return (
    <Show when={ctx.profiles().length > 0} fallback={<p class="text-muted">{t.user.add.noProfiles}</p>}>
      <form class="space-y-3" onSubmit={submit}>
        <div class="space-y-1">
          <label for="device-name" class="block font-medium">
            {t.user.add.name}
          </label>
          <input
            id="device-name"
            required
            maxlength={64}
            autocomplete="off"
            autocapitalize="off"
            spellcheck={false}
            aria-describedby="device-name-hint"
            class={fieldClass}
            disabled={off()}
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
          />
          <p id="device-name-hint" class="text-sm text-muted">
            {t.user.add.nameHint}
          </p>
        </div>
        <div class="space-y-1">
          <label for="device-profile" class="block font-medium">
            {t.user.add.profile}
          </label>
          <select id="device-profile" class={fieldClass} disabled={off()} onChange={(e) => setProfile(e.currentTarget.value)}>
            <For each={ctx.profiles()}>{(p) => <option value={p} selected={p === chosen()}>{p}</option>}</For>
          </select>
        </div>
        <FailureAlert failure={failure()} />
        <button type="submit" class={touchButton("primary")} disabled={ctx.busy() || off()}>
          {adding() ? t.user.add.busy : t.user.add.submit}
        </button>
      </form>
    </Show>
  );
}
