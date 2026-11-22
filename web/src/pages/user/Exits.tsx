// The exits the user may use: country (and provider, when the user may see
// it), state, latency, which of their devices go through it now and a 7-day
// availability bar. Never who else uses them, nor the upstream's name.

import { durationMs } from "@bufbuild/protobuf/wkt";
import { createMemo, createSignal, For, Show } from "solid-js";
import { useI18n } from "../../i18n";
import { StateBadge } from "../../ui/StateBadge";
import { Availability } from "../Availability";
import { matchesCountry } from "../route";
import { stateLabel, stateLevel } from "../upstream";
import { useUser } from "./context";

interface ExitCard {
  name: string;
  country: string;
  provider: string;
  devices: string;
}

export function Exits() {
  const { t, fmt } = useI18n();
  const ctx = useUser();
  const exits = createMemo(() => {
    const devices = ctx.user().devices;
    const countries = devices.flatMap((device) => device.route?.countries ?? []);
    const inUse = new Map<string, string[]>();
    for (const device of devices) {
      const name = ctx.peer(device.peerId)?.upstream;
      if (name) inUse.set(name, [...(inUse.get(name) ?? []), device.name]);
    }
    return ctx
      .exits()
      .map((u) => ({
        name: u.name,
        country: fmt.country(u.geo),
        provider: u.provider,
        devices: (inUse.get(u.name) ?? []).join(", "),
        visible: inUse.has(u.name) || countries.some((country) => matchesCountry(country, u)),
      }))
      .sort((a, b) => a.country.localeCompare(b.country) || a.provider.localeCompare(b.provider) || a.name.localeCompare(b.name));
  });
  const selected = createMemo(() => exits().filter((exit) => exit.visible));
  const other = createMemo(() => exits().filter((exit) => !exit.visible));
  return (
    <Show when={exits().length > 0}>
      <section aria-labelledby="exits" class="space-y-2">
        <h2 id="exits" class="text-sm text-muted">
          {t.user.exits}
        </h2>
        <p class="text-sm text-muted">{t.user.exitsIntro}</p>
        <Show when={selected().length > 0}>
          <ExitList exits={selected()} />
        </Show>
        <Show when={other().length > 0}>
          <OtherExits exits={other()} />
        </Show>
      </section>
    </Show>
  );
}

function OtherExits(props: { exits: readonly ExitCard[] }) {
  const { t } = useI18n();
  const [open, setOpen] = createSignal(false);
  return (
    <details class="space-y-3" onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary class="min-h-11 cursor-pointer rounded-xl border border-line bg-surface px-4 py-3 text-sm font-medium">
        {t.user.otherExits(props.exits.length)}
      </summary>
      <Show when={open()}>
        <ExitList exits={props.exits} />
      </Show>
    </details>
  );
}

function ExitList(props: { exits: readonly ExitCard[] }) {
  const { t, fmt } = useI18n();
  const ctx = useUser();
  return (
    <ul class="space-y-3">
      <For each={props.exits} keyed={(exit) => exit.name}>
        {(exit) => {
          const status = () => ctx.upstream(exit().name);
          const label = () => [exit().country, exit().provider].filter(Boolean).join(" · ");
          return (
            <li class="space-y-2 rounded-xl border border-line bg-surface p-4">
              <div class="flex flex-wrap items-center justify-between gap-2">
                <span class="font-medium">{label()}</span>
                <Show when={status()}>{(u) => (
                  <StateBadge level={stateLevel(u().state, u().routingBlock)} label={stateLabel(t, u().state, u().routingBlock)} />
                )}</Show>
              </div>
              <p class="text-sm text-muted">
                {exit().devices ? t.user.usedBy(exit().devices) : t.user.unused}
                <Show when={!status()?.routingBlock && status()?.latency}>
                  {(latency) => <> · {t.user.latency}: {fmt.milliseconds(durationMs(latency()))}</>}
                </Show>
              </p>
              <Availability name={exit().name} label={t.user.availabilityLabel(label())} load={ctx.self.upstreamHistory} />
            </li>
          );
        }}
      </For>
    </ul>
  );
}
