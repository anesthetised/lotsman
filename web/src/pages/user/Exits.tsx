// The exits the user may use: country (and provider, when the user may see
// it), state, latency, which of their devices go through it now and a 7-day
// availability bar. Never who else uses them, nor the upstream's name.

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
  /** Every allowed exit by country, then provider, with the user's devices routed through it now. */
  const exits = createMemo(() => {
    const devices = new Map<string, string[]>();
    for (const device of ctx.user().devices) {
      const name = ctx.peer(device.peerId)?.upstream;
      if (name) devices.set(name, [...(devices.get(name) ?? []), device.name]);
    }
    return ctx
      .exits()
      .map((u) => ({ name: u.name, country: fmt.country(u.geo), provider: u.provider, devices: (devices.get(u.name) ?? []).join(", ") }))
      .sort((a, b) => a.country.localeCompare(b.country) || a.provider.localeCompare(b.provider) || a.name.localeCompare(b.name));
  });
  return (
    <Show when={exits().length > 0}>
      <section aria-labelledby="exits" class="space-y-2">
        <h2 id="exits" class="text-sm text-muted">
          {t.user.exits}
        </h2>
        <p class="text-sm text-muted">{t.user.exitsIntro}</p>
        <ul class="space-y-3">
          <For each={exits()} keyed={(e) => e.name}>
            {(exit) => {
              const status = () => ctx.upstream(exit().name);
              const label = () => [exit().country, exit().provider].filter(Boolean).join(" · ");
              return (
                <li class="space-y-2 rounded-xl border border-line bg-surface p-4">
                  <div class="flex flex-wrap items-center justify-between gap-2">
                    <span class="font-medium">{label()}</span>
                    <Show when={status()}>{(u) => <StateBadge level={stateLevel(u().state)} label={stateLabel(t, u().state)} />}</Show>
                  </div>
                  <p class="text-sm text-muted">
                    {exit().devices ? t.user.usedBy(exit().devices) : t.user.unused}
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
