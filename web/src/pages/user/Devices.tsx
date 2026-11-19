// The user's devices: each with its live speed, a config per profile, and
// the changes a user may make to it.

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createEffect, createSignal, Errored, For, Loading, Show } from "solid-js";
import { num } from "../../api/live";
import type { Device, DevicePeer } from "../../gen/lotsman/v1/user_pb";
import { useI18n } from "../../i18n";
import { Dialog } from "../../ui/Dialog";
import { StateBadge } from "../../ui/StateBadge";
import { stateLabel, stateLevel } from "../upstream";
import { ConfigView, PeerConfig } from "./ConfigView";
import { useUser } from "./context";
import type { Failure } from "./errors";
import { FailureAlert } from "./FailureAlert";
import { changeProfile } from "./profileChange";
import { fieldClass, touch, touchButton } from "./touch";

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
            {issued().kind === "added" ? t.user.issued.added(issued().device, issued().profile) : t.user.issued.changed(issued().device, issued().profile)}
          </h3>
          <Show when={issued().leftover}>
            {(profile) => (
              <p role="alert" class="rounded-md border border-warn p-3 text-sm">
                {t.user.change.partial(profile())}
              </p>
            )}
          </Show>
          <p class="text-sm text-muted">{t.user.issued.hint}</p>
          <ConfigView device={issued().device} profile={issued().profile} config={issued().config} />
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
  const rate = (pick: (t: { downRate?: bigint; upRate?: bigint }) => bigint | undefined) =>
    props.device.peers.reduce((sum, p) => sum + num(pick(ctx.peer(p.peerId)?.traffic ?? {})), 0);
  return (
    <li class="space-y-3 rounded-xl border border-line bg-surface p-4">
      <div class="flex items-start justify-between gap-3">
        <h3 class="min-w-0 break-words text-lg font-medium">{props.device.name}</h3>
        <RemoveDevice device={props.device.name} />
      </div>
      <dl class="grid grid-cols-2 gap-2">
        <div>
          <dt class="text-xs text-muted">{t.traffic.down}</dt>
          <dd class="font-semibold tabular-nums">{fmt.rate(rate((x) => x.downRate))}</dd>
        </div>
        <div>
          <dt class="text-xs text-muted">{t.traffic.up}</dt>
          <dd class="font-semibold tabular-nums">{fmt.rate(rate((x) => x.upRate))}</dd>
        </div>
      </dl>
      <ul class="space-y-3">
        <For each={props.device.peers} keyed={(p) => p.peerId}>
          {(peer) => <PeerRow device={props.device} peer={peer()} />}
        </For>
      </ul>
    </li>
  );
}

function PeerRow(props: { device: Device; peer: DevicePeer }) {
  const { t, fmt } = useI18n();
  const ctx = useUser();
  const [open, setOpen] = createSignal(false);
  const status = () => ctx.peer(props.peer.peerId);
  const upstream = () => ctx.upstream(status()?.upstream ?? "");
  const panel = () => `config-${props.peer.peerId}`;
  return (
    <li class="space-y-2 border-t border-line pt-3 text-sm">
      <div class="flex flex-wrap items-baseline justify-between gap-x-3">
        <span>
          {t.user.profile}: <span class="font-medium">{props.peer.profile}</span>
        </span>
        <span class="tabular-nums text-muted">{fmt.bytes(num(status()?.traffic?.downBytes))}</span>
      </div>
      <div class="flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted">
        <Show when={upstream()} fallback={<StateBadge level="bad" label={t.user.blocked} />}>
          {(u) => <StateBadge level={stateLevel(u().state)} label={`${t.user.upstream} ${u().name}: ${stateLabel(t, u().state)}`} />}
        </Show>
        <span>
          {t.user.lastSeen}:{" "}
          <Show when={status()?.lastHandshake} fallback={t.user.never}>
            {(ts) => <>{fmt.ago(timestampDate(ts()), new Date())}</>}
          </Show>
        </span>
        <Show when={props.peer.created}>{(ts) => <span>{t.user.addedOn(fmt.dateTime(timestampDate(ts())))}</span>}</Show>
      </div>
      <div class="flex flex-wrap gap-2">
        <button type="button" class={touchButton("secondary")} aria-expanded={open() ? "true" : "false"} aria-controls={panel()} onClick={() => setOpen(!open())}>
          {open() ? t.user.config.hide : t.user.config.show}
          <span class="sr-only">
            {" "}
            {props.device.name}, {props.peer.profile}
          </span>
        </button>
        <Errored fallback={null}>
          <Loading>
            <ChangeProfile device={props.device} peer={props.peer} />
          </Loading>
        </Errored>
      </div>
      <div id={panel()}>
        <Show when={open()}>
          <PeerConfig peerId={props.peer.peerId} device={props.device.name} profile={props.peer.profile} />
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
    const outcome = await ctx.mutate(() => ctx.self.removeDevice({ device: props.device, profile: "" }));
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

/** A device gets its new profile before it loses the old one (see profileChange.ts). */
function ChangeProfile(props: { device: Device; peer: DevicePeer }) {
  const { t } = useI18n();
  const ctx = useUser();
  const [open, setOpen] = createSignal(false);
  const [choice, setChoice] = createSignal("");
  const [failure, setFailure] = createSignal<Failure>();
  // The profiles the device does not have yet.
  const others = () => ctx.profiles().filter((name) => !props.device.peers.some((p) => p.profile === name));
  const target = () => (others().includes(choice()) ? choice() : others()[0]);

  const change = async () => {
    const to = target();
    if (!to) return;
    setFailure(undefined);
    const outcome = await ctx.mutate(() => changeProfile(ctx.self, props.device.name, props.peer.profile, to));
    if (outcome.ok) {
      ctx.configs.set(outcome.value.peerId, outcome.value.config);
      ctx.issue({ kind: "changed", device: props.device.name, profile: to, config: outcome.value.config });
      setOpen(false);
    } else {
      setFailure(outcome.failure);
    }
  };
  return (
    <Show when={others().length > 0}>
      <Dialog
        open={open()}
        onOpenChange={(next) => {
          setOpen(next);
          setFailure(undefined);
        }}
        trigger={
          <>
            {t.user.change.button}
            <span class="sr-only">
              {" "}
              {props.device.name}, {props.peer.profile}
            </span>
          </>
        }
        triggerClass={`${touch} px-4`}
        title={t.user.change.title(props.device.name, props.peer.profile)}
        description={t.user.change.description}
      >
        <form
          class="space-y-3 text-sm"
          onSubmit={(e) => {
            e.preventDefault();
            void change();
          }}
        >
          <div class="space-y-1">
            <label for={`profile-${props.peer.peerId}`} class="block font-medium">
              {t.user.change.choose}
            </label>
            <select id={`profile-${props.peer.peerId}`} class={fieldClass} onChange={(e) => setChoice(e.currentTarget.value)}>
              <For each={others()}>{(name) => <option value={name} selected={name === target()}>{name}</option>}</For>
            </select>
          </div>
          <FailureAlert failure={failure()} />
          <div class="flex justify-end gap-2">
            <button type="button" class={touchButton()} onClick={() => setOpen(false)}>
              {t.user.remove.cancel}
            </button>
            <button type="submit" class={touchButton("primary")} disabled={ctx.busy()}>
              {t.user.change.confirm}
            </button>
          </div>
        </form>
      </Dialog>
    </Show>
  );
}
