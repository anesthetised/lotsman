// Adding a device: a name and, when the user may choose routes, its route,
// set in the same controls as a device's own; left as it starts, the device
// gets the daemon's default (the fastest of any exit). With devices already
// there the form waits behind a button. The daemon answers with the new
// config, which the page shows at once.

import { createMemo, createSignal, Show } from "solid-js";
import { useI18n } from "../../i18n";
import { createNewRoute, RouteFields } from "../RouteEditor";
import { useUser } from "./context";
import type { Failure } from "./errors";
import { FailureAlert } from "./FailureAlert";
import { fieldClass, touchButton } from "./touch";

export function AddDevice() {
  const { t } = useI18n();
  const ctx = useUser();
  const [name, setName] = createSignal("");
  const [open, setOpen] = createSignal(false);
  const [failure, setFailure] = createSignal<Failure>();
  const [adding, setAdding] = createSignal(false);
  /** Adding is off for good once the daemon says so: the operator set `max_devices: 0`. */
  const off = createMemo(() => failure()?.kind === "denied");
  const first = () => ctx.user().devices.length === 0;
  const shown = () => open() || first();
  const route = createNewRoute(ctx.exits, () => ctx.rights().showProviders);
  const routeReady = () => !ctx.rights().editRoutes || route.canSubmit();

  const close = () => {
    setOpen(false);
    setName("");
    setFailure(undefined);
    route.reset();
  };
  const submit = async (e: SubmitEvent) => {
    e.preventDefault();
    if (off() || !routeReady()) return;
    setFailure(undefined);
    setAdding(true);
    const device = name().trim();
    // Without the right to choose, the route stays unset: the daemon refuses one.
    const outcome = await ctx.mutate(() => ctx.self.addDevice({ device, route: ctx.rights().editRoutes ? route.route() : undefined }));
    setAdding(false);
    if (!outcome.ok) {
      setFailure(outcome.failure);
      return;
    }
    ctx.configs.set(outcome.value.peerId, outcome.value.config);
    ctx.issue({ device, config: outcome.value.config });
    close();
  };

  return (
    <Show
      when={shown()}
      fallback={
        <button type="button" class={[...touchButton(), "w-full"]} aria-expanded="false" onClick={() => setOpen(true)}>
          <span aria-hidden="true">+ </span>
          {t.user.add.open}
        </button>
      }
    >
      <section aria-labelledby="add-title" class="space-y-3 rounded-xl border border-line bg-surface p-4">
        <h3 id="add-title" class="text-lg font-medium">
          {t.user.add.title}
        </h3>
        <form class="space-y-4" onSubmit={submit}>
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
          <Show when={ctx.rights().editRoutes && route.offer().size > 0}>
            <fieldset class="space-y-3 text-sm" disabled={off()}>
              <legend class="mb-1 text-base font-medium">{t.route.title}</legend>
              <RouteFields draft={route.draft()} setDraft={route.setDraft} offer={route.offer()} exits={ctx.exits()} />
              <p class="text-xs text-muted">{t.route.later}</p>
            </fieldset>
          </Show>
          <Show when={failure()}>
            {(f) => (
              <Show when={f().kind === "denied"} fallback={<FailureAlert failure={f()} />}>
                <p role="alert" class="rounded-md border border-bad p-3">
                  {t.user.add.off}
                </p>
              </Show>
            )}
          </Show>
          <div class="flex flex-wrap justify-end gap-2">
            <Show when={!first()}>
              <button type="button" class={touchButton()} onClick={close}>
                {t.user.add.cancel}
              </button>
            </Show>
            <button type="submit" class={touchButton("primary")} disabled={ctx.busy() || off() || !routeReady()}>
              {adding() ? t.user.add.busy : t.user.add.submit}
            </button>
          </div>
        </form>
      </section>
    </Show>
  );
}
