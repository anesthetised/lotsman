// The user's devices: each with its exit now, its route, its config and
// the way to remove it.

import { durationMs, timestampDate } from "@bufbuild/protobuf/wkt";
import { createEffect, createSignal, For, Show } from "solid-js";
import { num } from "../../api/live";
import type { UpstreamStatus } from "../../gen/lotsman/v1/status_pb";
import type { Device, Route } from "../../gen/lotsman/v1/user_pb";
import { useI18n } from "../../i18n";
import { Dialog } from "../../ui/Dialog";
import { ChevronDownIcon } from "../../ui/icons";
import { StateBadge } from "../../ui/StateBadge";
import { RouteEditor } from "../RouteEditor";
import { stateLabel, stateLevel } from "../upstream";
import { ConfigView, PeerConfig } from "./ConfigView";
import { useUser } from "./context";
import { type Failure, failureText } from "./errors";
import { FailureAlert } from "./FailureAlert";
import { touch, touchButton } from "./touch";

export function Devices() {
  const ctx = useUser();
  return (
    <ul class="space-y-3">
      <For each={ctx.user().devices} keyed={(d) => d.name} fallback={<EmptyDevices />}>
        {(device) => <DeviceCard device={device()} />}
      </For>
    </ul>
  );
}

function EmptyDevices() {
  const { t } = useI18n();
  return <li class="rounded-xl border border-dashed border-line p-4 text-muted">{t.user.noDevices}</li>;
}

/** What the user was just given, until they dismiss it. */
export function IssuedConfig() {
  const { t } = useI18n();
  const ctx = useUser();
  let heading: HTMLHeadingElement | undefined;
  // Brings a screen reader and the viewport to a config that has just appeared.
  createEffect(
    () => ctx.issued(),
    (issued) => {
      if (!issued) return;
      heading?.focus();
      heading?.scrollIntoView({ block: "nearest" });
    },
  );
  return (
    <Show when={ctx.issued()}>
      {(issued) => (
        <section aria-labelledby="issued-title" class="space-y-3 rounded-xl border-2 border-accent bg-surface p-4">
          <h3 id="issued-title" ref={(el) => (heading = el)} tabindex="-1" class="text-lg font-medium">
            {t.user.issued.added(issued().device)}
          </h3>
          <p class="text-sm text-muted">{t.user.issued.hint}</p>
          <ConfigView device={issued().device} config={issued().config} />
          <button type="button" class={touchButton()} onClick={() => ctx.issue(undefined)}>
            {t.user.issued.done}
          </button>
        </section>
      )}
    </Show>
  );
}

function DeviceCard(props: { device: Device }) {
  const { t, fmt } = useI18n();
  const ctx = useUser();
  // Mount the editor on first expansion and keep its draft when collapsed.
  const [expanded, setExpanded] = createSignal<boolean>();
  const [open, setOpen] = createSignal(false);
  const status = () => ctx.peer(props.device.peerId);
  const exit = () => ctx.upstream(status()?.upstream ?? "");
  const panel = () => `config-${props.device.peerId}`;
  const title = () => `device-${props.device.peerId}`;
  const details = () => `device-details-${props.device.peerId}`;
  const routeHeading = () => `route-${props.device.peerId}`;
  /** An exit in words: its country, and its provider when the user may see it. */
  const exitName = (u: UpstreamStatus) => [fmt.country(u.geo), ctx.rights().showProviders ? u.provider : ""].filter(Boolean).join(" · ");
  const apply = async (route: Route) => {
    const outcome = await ctx.mutate(() => ctx.self.setRoute({ device: props.device.name, route }));
    return outcome.ok ? undefined : () => failureText(t, outcome.failure);
  };
  return (
    <li class="rounded-xl border border-line bg-surface p-4">
      <div class="relative grid gap-3">
        <div>
          <div class="flex flex-wrap items-start justify-between gap-x-3 gap-y-1">
            <h3 id={title()} class="min-w-0 flex-1 break-words text-lg font-medium">{props.device.name}</h3>
            <div class="flex items-center gap-2">
              <Show when={exit()} fallback={<StateBadge level="bad" label={t.user.blocked} />}>
                {(u) => <StateBadge level={stateLevel(u().state, u().routingBlock)} label={stateLabel(t, u().state, u().routingBlock)} />}
              </Show>
              <span class={["text-muted", { "rotate-180": expanded() }]}><ChevronDownIcon /></span>
            </div>
          </div>
          <p class="mt-0.5 flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted">
            <span class="font-mono">{props.device.address}</span>
            <span>
              {t.user.lastSeen}:{" "}
              <Show when={status()?.lastHandshake} fallback={t.user.never}>
                {(ts) => <>{fmt.ago(timestampDate(ts()), new Date())}</>}
              </Show>
            </span>
            <span class="tabular-nums">
              {t.traffic.down} {fmt.rate(num(status()?.traffic?.downRate))} · {t.traffic.up} {fmt.rate(num(status()?.traffic?.upRate))}
            </span>
          </p>
        </div>
        <p class="flex justify-between gap-3 rounded-lg bg-bg px-3 py-2 text-sm">
          <span class="min-w-0 break-words">
            {t.route.now}: <span class="font-semibold">{exit() ? exitName(exit()!) : t.route.blocked}</span>
          </span>
          <span class="shrink-0 tabular-nums text-muted">{exit()?.latency && !exit()?.routingBlock ? fmt.milliseconds(durationMs(exit()!.latency!)) : "—"}</span>
        </p>
        <button
          type="button"
          class="absolute inset-0 w-full cursor-pointer rounded-lg hover:bg-fg/5"
          aria-labelledby={title()}
          aria-controls={details()}
          aria-expanded={expanded() ? "true" : "false"}
          onClick={() => setExpanded(!expanded())}
        />
      </div>
      <div id={details()} hidden={!expanded()} class="mt-3 space-y-3">
        <Show when={expanded() !== undefined}>
          <section aria-labelledby={routeHeading()} class="space-y-2 border-t border-line pt-3">
            <h4 id={routeHeading()} class="font-medium">
              {t.route.title}
              <span class="sr-only"> {props.device.name}</span>
            </h4>
            <RouteEditor
              route={props.device.route}
              exits={ctx.exits()}
              providers={ctx.rights().showProviders}
              readOnly={!ctx.rights().editRoutes}
              busy={ctx.busy()}
              apply={apply}
            />
          </section>
          <div class="flex flex-wrap gap-2 border-t border-line pt-3">
            <button type="button" class={touchButton("secondary")} aria-expanded={open() ? "true" : "false"} aria-controls={panel()} onClick={() => setOpen(!open())}>
              {open() ? t.user.config.hide : t.user.config.show}
              <span class="sr-only"> {props.device.name}</span>
            </button>
            <RemoveDevice device={props.device.name} />
          </div>
          <div id={panel()}>
            <Show when={open()}>
              <PeerConfig peerId={props.device.peerId} device={props.device.name} />
            </Show>
          </div>
        </Show>
      </div>
    </li>
  );
}

function RemoveDevice(props: { device: string }) {
  const { t } = useI18n();
  const ctx = useUser();
  const [open, setOpen] = createSignal(false);
  const [failure, setFailure] = createSignal<Failure>();
  const remove = async () => {
    setFailure(undefined);
    const outcome = await ctx.mutate(() => ctx.self.removeDevice({ device: props.device }));
    if (outcome.ok) setOpen(false);
    else setFailure(outcome.failure);
  };
  return (
    <Dialog
      open={open()}
      onOpenChange={(next) => {
        setOpen(next);
        setFailure(undefined);
      }}
      trigger={
        <>
          {t.user.remove.button}
          <span class="sr-only"> {props.device}</span>
        </>
      }
      triggerClass={`${touch} px-4`}
      title={t.user.remove.title(props.device)}
      description={t.user.remove.description}
    >
      <div class="space-y-3 text-sm">
        <FailureAlert failure={failure()} />
        <div class="flex justify-end gap-2">
          <button type="button" class={touchButton()} onClick={() => setOpen(false)}>
            {t.user.remove.cancel}
          </button>
          <button type="button" class={touchButton("danger")} disabled={ctx.busy()} onClick={remove}>
            {t.user.remove.confirm}
          </button>
        </div>
      </div>
    </Dialog>
  );
}
