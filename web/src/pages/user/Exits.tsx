// The exits the user's devices go out through now: country (and provider,
// when the user may see it), state, latency and a 7-day availability bar.
// Never who else uses them, nor the upstream's name.

import { durationMs } from "@bufbuild/protobuf/wkt";
import { createMemo, For, Show } from "solid-js";
import { useI18n } from "../../i18n";
import { StateBadge } from "../../ui/StateBadge";
import { Availability } from "../Availability";
import { stateLabel, stateLevel } from "../upstream";
import { useUser } from "./context";

export function Exits() {
  const { t, fmt } = useI18n();
  const ctx = useUser();
  /** Each exit a device is routed through now, with the names of those devices. */
  const used = createMemo(() => {
    const devices = new Map<string, Set<string>>();
    for (const device of ctx.user().devices) {
      const name = ctx.peer(device.peerId)?.upstream;
      if (name) devices.set(name, (devices.get(name) ?? new Set()).add(device.name));
    }
    return [...devices].sort(([a], [b]) => a.localeCompare(b)).map(([name, names]) => ({ name, devices: [...names].join(", ") }));
  });
  return (
    <Show when={used().length > 0}>
      <section aria-labelledby="exits" class="space-y-2">
        <h2 id="exits" class="text-sm text-muted">
          {t.user.exits}
        </h2>
        <p class="text-sm text-muted">{t.user.exitsIntro}</p>
        <ul class="space-y-3">
          <For each={used()} keyed={(e) => e.name}>
            {(exit) => {
              const status = () => ctx.upstream(exit().name);
              const label = () => [fmt.country(status()?.geo ?? ""), status()?.provider ?? ""].filter(Boolean).join(" · ");
              return (
                <li class="space-y-2 rounded-xl border border-line bg-surface p-4">
                  <div class="flex flex-wrap items-center justify-between gap-2">
                    <span class="font-medium">{label()}</span>
                    <Show when={status()}>{(u) => <StateBadge level={stateLevel(u().state)} label={stateLabel(t, u().state)} />}</Show>
                  </div>
                  <p class="text-sm text-muted">
                    {t.user.usedBy(exit().devices)}
                    <Show when={status()?.latency}>{(latency) => <> · {t.user.latency}: {fmt.milliseconds(durationMs(latency()))}</>}</Show>
                  </p>
                  <Availability name={exit().name} label={t.user.availabilityLabel(label())} load={ctx.self.upstreamHistory} />
                </li>
              );
            }}
          </For>
        </ul>
      </section>
    </Show>
  );
}
