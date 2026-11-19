// Adding a device: a name and, when the user may choose routes, the route to
// start with: any exit (the daemon's default) or the route of a device they
// have. The daemon answers with the new config, which the page shows at once.

import { createMemo, createSignal, Show } from "solid-js";
import { useI18n } from "../../i18n";
import { Segmented } from "../../ui/Toggles";
import { DEFAULT_ROUTE, routeSummary } from "../route";
import { useUser } from "./context";
import type { Failure } from "./errors";
import { FailureAlert } from "./FailureAlert";
import { fieldClass, touchButton } from "./touch";

export function AddDevice() {
  const { t, fmt } = useI18n();
  const ctx = useUser();
  const [name, setName] = createSignal("");
  /** The device whose route the new one copies; "" is any exit. */
  const [like, setLike] = createSignal("");
  const [failure, setFailure] = createSignal<Failure>();
  const [adding, setAdding] = createSignal(false);
  /** Adding is off for good once the daemon says so: the operator set `max_devices: 0`. */
  const off = createMemo(() => failure()?.kind === "denied");
  const devices = () => ctx.user().devices;
  const choice = () => (devices().some((d) => d.name === like()) ? like() : "");

  const submit = async (e: SubmitEvent) => {
    e.preventDefault();
    if (off()) return;
    setFailure(undefined);
    setAdding(true);
    const device = name().trim();
    // Without the right to choose, the route stays unset: the daemon refuses one.
    const route = ctx.rights().editRoutes ? devices().find((d) => d.name === choice())?.route : undefined;
    const outcome = await ctx.mutate(() => ctx.self.addDevice({ device, route }));
    setAdding(false);
    if (!outcome.ok) {
      setFailure(outcome.failure);
      return;
    }
    ctx.configs.set(outcome.value.peerId, outcome.value.config);
    ctx.issue({ device, config: outcome.value.config });
    setName("");
    setLike("");
  };

  return (
    <section aria-labelledby="add-title" class="space-y-3 rounded-xl border border-line bg-surface p-4">
      <h3 id="add-title" class="text-lg font-medium">
        {t.user.add.title}
      </h3>
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
        <Show when={ctx.rights().editRoutes && devices().length > 0}>
          <div class="space-y-1">
            <p class="font-medium">
              {t.route.choice}
            </p>
            <Segmented
              label={t.route.choice}
              value={choice()}
              disabled={off()}
              options={[
                { value: "", label: t.route.summary.any },
                ...devices().map((d) => ({
                  value: d.name,
                  label: (
                    <span class="flex flex-col">
                      <span>{t.route.sameAs(d.name)}</span>
                      <span class="text-xs font-normal text-muted">{routeSummary(d.route ?? DEFAULT_ROUTE, t, fmt.country, ctx.rights().showProviders)}</span>
                    </span>
                  ),
                })),
              ]}
              onChange={setLike}
            />
            <p class="text-sm text-muted">{t.route.later}</p>
          </div>
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
        <button type="submit" class={touchButton("primary")} disabled={ctx.busy() || off()}>
          {adding() ? t.user.add.busy : t.user.add.submit}
        </button>
      </form>
    </section>
  );
}
