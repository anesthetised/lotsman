// One user (UserService): their devices with each route and exit now, the
// route editor, their rights, their link, and adding or removing devices.

import { create } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useNavigate, useParams } from "@solidjs/router";
import { createMemo, createSignal, Errored, For, Loading, refresh, Show } from "solid-js";
import { admin } from "../../api/admin";
import { api } from "../../api/client";
import { createLive } from "../../api/live";
import type { PeerStatus, StatusSnapshot, UpstreamStatus } from "../../gen/lotsman/v1/status_pb";
import { type Device, type Rights, RightsSchema, type Route, type User } from "../../gen/lotsman/v1/user_pb";
import { useI18n } from "../../i18n";
import { useConsoleI18n } from "../../i18n/console";
import { buttonClass } from "../../ui/button";
import { Dialog } from "../../ui/Dialog";
import { QrCode } from "../../ui/QrCode";
import { Chip, Segmented, SwitchField } from "../../ui/Toggles";
import { LiveError } from "../LiveError";
import { DEFAULT_ROUTE, routeSummary } from "../route";
import { RouteEditor } from "../RouteEditor";
import { ApiError, errorMessage } from "./ApiError";
import { Confirm } from "./Confirm";
import { configFileName, download, userLink } from "./devices";

export default function UserDetail() {
  const { t } = useI18n();
  const params = useParams<{ name: string }>();
  return (
    <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
      <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
        <Detail name={params.name ?? ""} />
      </Loading>
    </Errored>
  );
}

/** The exits a user's devices may use: their rights' exits, or all. */
function allowedExits(upstreams: readonly UpstreamStatus[], rights: Rights): UpstreamStatus[] {
  return upstreams.filter((u) => rights.exits.length === 0 || rights.exits.includes(u.name));
}

function Detail(props: { name: string }) {
  const { t, c } = useConsoleI18n();
  const navigate = useNavigate();
  const list = createMemo(() => admin.users.list({}));
  const snapshot = createLive(() => api.status.watch({}));
  const user = () => list().users.find((u) => u.name === props.name);
  return (
    <div class="space-y-4">
      <p>
        <a href="/admin/users" class="text-accent hover:underline">
          {t.admin.users}
        </a>
        <span aria-hidden="true" class="text-muted">
          {" "}
          ›
        </span>
      </p>
      <Show when={user()} fallback={<p class="rounded-lg border border-line bg-surface p-4 text-muted">{c.users.missing}</p>}>
        {(u) => <UserView user={u()} snapshot={snapshot()} onChanged={() => refresh(list)} onRemoved={() => navigate("/admin/users")} />}
      </Show>
    </div>
  );
}

function UserView(props: { user: User; snapshot: StatusSnapshot; onChanged: () => void; onRemoved: () => void }) {
  const { t, fmt, c } = useConsoleI18n();
  const u = () => props.user;
  const rights = () => u().rights ?? create(RightsSchema, { editRoutes: true, showProviders: true });
  const peers = createMemo(() => new Map(props.snapshot.peers.map((p) => [p.peerId, p])));
  const exits = () => allowedExits(props.snapshot.upstreams, rights());
  // The device whose route the editor shows: the first one until another is picked.
  const [picked, setPicked] = createSignal<string>();
  const selected = () => (u().devices.some((d) => d.name === picked()) ? picked() : u().devices[0]?.name);
  return (
    <>
      <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
        <h1 class="text-lg font-semibold">{u().name}</h1>
        <Show when={u().created}>{(created) => <span class="text-xs text-muted">{c.users.since(fmt.dateTime(timestampDate(created())))}</span>}</Show>
        <span class="text-xs">{u().hasLink ? c.users.hasLink : <span class="text-muted">{c.users.noLink}</span>}</span>
        <div class="ml-auto flex flex-wrap gap-2">
          <AddDevice user={u()} onAdded={props.onChanged} />
          <IssueLink user={u()} onIssued={props.onChanged} />
          <Show when={u().hasLink}>
            <Confirm
              trigger={c.users.revokeLink}
              title={c.users.revokeLinkTitle(u().name)}
              description={c.users.revokeLinkDescription}
              confirm={c.users.revokeLink}
              action={async () => {
                await admin.users.revokeLink({ user: u().name });
                props.onChanged();
              }}
            />
          </Show>
          <Confirm
            trigger={c.users.removeUser}
            title={c.users.removeUserTitle(u().name)}
            description={c.users.removeUserDescription(u().devices.length)}
            confirm={c.users.removeUser}
            action={async () => {
              await admin.users.removeUser({ user: u().name });
              props.onRemoved();
            }}
          />
        </div>
      </div>

      <section aria-labelledby="devices-title" class="rounded-lg border border-line bg-surface">
        <h2 id="devices-title" class="px-3 py-2 font-semibold">
          {c.users.devices}
        </h2>
        <Show when={u().devices.length > 0} fallback={<p class="border-t border-line px-3 py-2 text-muted">{c.users.noDevices}</p>}>
          {/* relative: the rows' screen-reader-only text stays inside the scrolling frame instead of widening the page. */}
          <div class="relative overflow-x-auto border-t border-line">
            <table class="w-full min-w-[48rem]">
              <thead class="text-left text-xs text-muted">
                <tr>
                  <th scope="col" class="px-3 py-1.5 font-medium">{t.admin.device}</th>
                  <th scope="col" class="px-3 py-1.5 font-medium">{t.route.title}</th>
                  <th scope="col" class="px-3 py-1.5 font-medium">{c.users.exit}</th>
                  <th scope="col" class="px-3 py-1.5 font-medium">{t.admin.address}</th>
                  <th scope="col" class="px-3 py-1.5">
                    <span class="sr-only">{c.users.actions}</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                <For each={u().devices} keyed={(d) => d.peerId}>
                  {(d) => (
                    <DeviceRow
                      user={u().name}
                      device={d()}
                      status={peers().get(d().peerId)}
                      selected={selected() === d().name}
                      onSelect={() => setPicked(d().name)}
                      onChanged={props.onChanged}
                    />
                  )}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </section>

      <div class="grid items-start gap-4 lg:grid-cols-2">
        {/* Keyed by the name, so picking another device starts from its route, while a reload keeps the edits. */}
        <Show when={selected()} keyed>
          {(name) => (
            <section aria-labelledby="route-title" class="space-y-3 rounded-lg border-2 border-accent bg-surface p-4">
              <h2 id="route-title" class="font-semibold">
                {c.users.routeOf(name)}
              </h2>
              <RouteEditor
                route={u().devices.find((d) => d.name === name)?.route}
                exits={exits()}
                providers
                apply={async (route: Route) => {
                  try {
                    await admin.users.setRoute({ user: u().name, device: name, route });
                    props.onChanged();
                    return undefined;
                  } catch (e) {
                    return c.failed(errorMessage(e));
                  }
                }}
              />
            </section>
          )}
        </Show>

        <RightsBlock user={u()} rights={rights()} upstreams={props.snapshot.upstreams} onChanged={props.onChanged} />
      </div>
    </>
  );
}

function DeviceRow(props: { user: string; device: Device; status: PeerStatus | undefined; selected: boolean; onSelect: () => void; onChanged: () => void }) {
  const { t, fmt, c } = useConsoleI18n();
  const what = () => `${props.user}/${props.device.name}`;
  return (
    <tr class={["border-t border-line", { "bg-bg": props.selected }]}>
      <th scope="row" class="px-3 py-1.5 text-left font-medium">
        {props.device.name}
      </th>
      <td class="px-3 py-1.5">{routeSummary(props.device.route ?? DEFAULT_ROUTE, t, fmt.country)}</td>
      <td class="whitespace-nowrap px-3 py-1.5 font-mono text-xs">
        <Show when={props.status?.upstream} fallback={<span class="font-sans text-sm text-bad">{t.user.blocked}</span>}>
          {(name) => <>{name()}</>}
        </Show>
      </td>
      <td class="px-3 py-1.5 font-mono text-xs">{props.device.address}</td>
      <td class="px-3 py-1.5">
        <div class="flex justify-end gap-2">
          <button type="button" class={buttonClass("compact")} aria-pressed={props.selected ? "true" : "false"} onClick={() => props.onSelect()}>
            {c.users.editRoute}
            <span class="sr-only"> {props.device.name}</span>
          </button>
          <ShowConfig user={props.user} device={props.device} />
          <a href={`/admin/connections?peer=${encodeURIComponent(props.device.peerId)}`} class={buttonClass("compact")}>
            {c.connections.title}
            <span class="sr-only"> {what()}</span>
          </a>
          <Confirm
            trigger={
              <>
                {c.users.remove}
                <span class="sr-only"> {what()}</span>
              </>
            }
            title={c.users.removeDeviceTitle(what())}
            description={c.users.removeDeviceDescription}
            confirm={c.users.remove}
            action={async () => {
              await admin.users.removeDevice({ user: props.user, device: props.device.name });
              props.onChanged();
            }}
          />
        </div>
      </td>
    </tr>
  );
}

/**
 * What the user may do. Each change is stored at once. Exits: every one
 * checked is stored as none named (all, including upstreams added later);
 * the last checked one stays, since none would mean all.
 */
function RightsBlock(props: { user: User; rights: Rights; upstreams: readonly UpstreamStatus[]; onChanged: () => void }) {
  const { c } = useConsoleI18n();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();
  const allowed = (name: string) => props.rights.exits.length === 0 || props.rights.exits.includes(name);
  const checked = () => props.upstreams.filter((u) => allowed(u.name)).map((u) => u.name);
  const save = async (change: Partial<Pick<Rights, "editRoutes" | "showProviders" | "exits">>) => {
    setBusy(true);
    setError(undefined);
    const { editRoutes, showProviders, exits } = props.rights;
    try {
      await admin.users.setRights({ user: props.user.name, rights: { editRoutes, showProviders, exits, ...change } });
      props.onChanged();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  const toggleExit = (name: string) => {
    const next = allowed(name) ? checked().filter((n) => n !== name) : [...checked(), name];
    void save({ exits: next.length === props.upstreams.length ? [] : next });
  };
  return (
    <section aria-labelledby="rights-title" class="space-y-3 rounded-lg border border-line bg-surface p-4">
      <h2 id="rights-title" class="font-semibold">
        {c.users.rights}
      </h2>
      <SwitchField
        label={c.users.editRoutes}
        hint={c.users.editRoutesHint}
        checked={props.rights.editRoutes}
        disabled={busy()}
        onChange={(editRoutes) => void save({ editRoutes })}
      />
      <div>
        <p id="rights-exits" class="text-sm font-medium">
          {c.users.exits}
        </p>
        <div role="group" aria-labelledby="rights-exits" aria-describedby="rights-exits-hint" class="flex flex-wrap gap-x-1.5">
          <For each={props.upstreams} keyed={(u) => u.name}>
            {(u) => (
              <Chip mono pressed={allowed(u().name)} disabled={busy() || (allowed(u().name) && checked().length === 1)} onChange={() => toggleExit(u().name)}>
                {u().name}
              </Chip>
            )}
          </For>
        </div>
        <p id="rights-exits-hint" class="text-xs text-muted">
          {c.users.exitsHint}
        </p>
      </div>
      <SwitchField
        label={c.users.showProviders}
        hint={c.users.showProvidersHint}
        checked={props.rights.showProviders}
        disabled={busy()}
        onChange={(showProviders) => void save({ showProviders })}
      />
      <ApiError error={error()} />
    </section>
  );
}

/** Adds a device for the user, with the default route or that of a device they have, and then shows its config. */
function AddDevice(props: { user: User; onAdded: () => void }) {
  const { t, fmt, c } = useConsoleI18n();
  const [open, setOpen] = createSignal(false);
  const [device, setDevice] = createSignal("");
  const [like, setLike] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();
  const [added, setAdded] = createSignal<{ config: string; file: string; title: string }>();

  const openChange = (next: boolean) => {
    setOpen(next);
    if (!next) return;
    setDevice("");
    setLike("");
    setError(undefined);
    setAdded(undefined);
  };
  const submit = async (e: SubmitEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    const name = device().trim();
    const route = props.user.devices.find((d) => d.name === like())?.route;
    try {
      const res = await admin.users.addDevice({ user: props.user.name, device: name, route });
      setAdded({ config: res.config, file: configFileName(props.user.name, name), title: `${props.user.name}/${name}` });
      props.onAdded();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      open={open()}
      onOpenChange={openChange}
      wide
      variant="primary"
      trigger={c.users.addDevice}
      title={added() ? c.users.configTitle(added()!.title) : c.users.addDeviceTitle(props.user.name)}
      description={added() ? c.users.configDescription : c.users.addDeviceDescription}
    >
      <Show
        when={added()}
        fallback={
          <form class="space-y-3 text-sm" onSubmit={submit}>
            <div class="space-y-1">
              <label for="add-device-name" class="block font-medium">
                {t.admin.device}
              </label>
              <input
                id="add-device-name"
                required
                value={device()}
                onInput={(e) => setDevice(e.currentTarget.value)}
                autocomplete="off"
                spellcheck={false}
                aria-describedby="add-device-hint"
                class="w-full max-w-xs rounded-md border border-line bg-bg px-3 py-1.5"
              />
              <p id="add-device-hint" class="text-xs text-muted">
                {c.users.deviceHint}
              </p>
            </div>
            <Show when={props.user.devices.length > 0}>
              <div class="space-y-1">
                <p class="font-medium">{t.route.choice}</p>
                <Segmented
                  label={t.route.choice}
                  value={props.user.devices.some((d) => d.name === like()) ? like() : ""}
                  options={[
                    { value: "", label: t.route.summary.any },
                    ...props.user.devices.map((d) => ({
                      value: d.name,
                      label: (
                        <span class="flex flex-col">
                          <span>{t.route.sameAs(d.name)}</span>
                          <span class="text-xs font-normal text-muted">{routeSummary(d.route ?? DEFAULT_ROUTE, t, fmt.country)}</span>
                        </span>
                      ),
                    })),
                  ]}
                  onChange={setLike}
                />
                <p class="text-xs text-muted">{t.route.later}</p>
              </div>
            </Show>
            <ApiError error={error()} />
            <div class="flex justify-end gap-2">
              <button type="button" class={buttonClass()} onClick={() => setOpen(false)}>
                {t.admin.cancel}
              </button>
              <button type="submit" class={buttonClass("primary")} disabled={busy()}>
                {c.users.add}
              </button>
            </div>
          </form>
        }
      >
        {(a) => <ConfigView config={a().config} file={a().file} label={a().title} />}
      </Show>
    </Dialog>
  );
}

/** Shows a device's config again (GetConfig). */
function ShowConfig(props: { user: string; device: Device }) {
  const { t, c } = useConsoleI18n();
  const [open, setOpen] = createSignal(false);
  const [config, setConfig] = createSignal<string>();
  const [error, setError] = createSignal<unknown>();
  const title = () => `${props.user}/${props.device.name}`;
  const openChange = async (next: boolean) => {
    setOpen(next);
    setConfig(undefined);
    setError(undefined);
    if (!next) return;
    try {
      setConfig((await admin.users.getConfig({ peerId: props.device.peerId })).config);
    } catch (e) {
      setError(e);
    }
  };
  return (
    <Dialog
      open={open()}
      onOpenChange={openChange}
      wide
      variant="compact"
      trigger={
        <>
          {c.users.config}
          <span class="sr-only"> {title()}</span>
        </>
      }
      title={c.users.configTitle(title())}
      description={c.users.configDescription}
    >
      <ApiError error={error()} />
      <Show when={config()} fallback={<Show when={error() === undefined}>{<p class="text-sm text-muted">{t.common.connecting}</p>}</Show>}>
        {(text) => <ConfigView config={text()} file={configFileName(props.user, props.device.name)} label={title()} />}
      </Show>
    </Dialog>
  );
}

/** A config as a QR code and as text, with copy and download. */
function ConfigView(props: { config: string; file: string; label: string }) {
  const { t, c } = useConsoleI18n();
  const [copied, setCopied] = createSignal(false);
  const copy = async () => {
    await navigator.clipboard.writeText(props.config);
    setCopied(true);
  };
  return (
    <div class="grid gap-4 text-sm sm:grid-cols-[16rem_1fr]">
      <QrCode text={props.config} label={c.users.qrLabel(props.label)} tooLong={c.users.qrTooLong} />
      <div class="min-w-0 space-y-2">
        <label for="device-config" class="block font-medium">
          {c.users.configText}
        </label>
        <textarea
          id="device-config"
          readonly
          rows={12}
          wrap="off"
          value={props.config}
          class="w-full resize-y rounded-md border border-line bg-bg p-2 font-mono text-xs"
        />
        <div class="flex flex-wrap gap-2">
          <button type="button" class={buttonClass()} onClick={copy}>
            {copied() ? t.admin.copied : t.admin.copy}
          </button>
          <button type="button" class={buttonClass()} onClick={() => download(props.file, props.config)}>
            {c.users.download(props.file)}
          </button>
        </div>
        <p class="text-xs text-muted">{c.users.configSecret}</p>
      </div>
    </div>
  );
}

/** Issues a user's link, which rotates an existing one, and shows it once. */
function IssueLink(props: { user: User; onIssued: () => void }) {
  const { t, c } = useConsoleI18n();
  const [open, setOpen] = createSignal(false);
  const [link, setLink] = createSignal<string>();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();
  const [copied, setCopied] = createSignal(false);
  const name = () => props.user.name;
  const openChange = (next: boolean) => {
    setOpen(next);
    setLink(undefined);
    setError(undefined);
    setCopied(false);
  };
  const issue = async () => {
    setBusy(true);
    setError(undefined);
    try {
      const { token } = await admin.users.issueLink({ user: name() });
      setLink(userLink(location.origin, token));
      props.onIssued();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  const copy = async (url: string) => {
    await navigator.clipboard.writeText(url);
    setCopied(true);
  };
  return (
    <Dialog
      open={open()}
      onOpenChange={openChange}
      variant="compact"
      trigger={props.user.hasLink ? c.users.newLink : c.users.issueLink}
      title={c.users.linkTitle(name())}
    >
      <div class="space-y-3 text-sm">
        <ApiError error={error()} />
        <Show
          when={link()}
          fallback={
            <>
              <p class="text-muted">{c.users.linkIntro}</p>
              <Show when={props.user.hasLink}>
                <p>{c.users.linkRotates}</p>
              </Show>
              <div class="flex justify-end gap-2">
                <button type="button" class={buttonClass()} onClick={() => setOpen(false)}>
                  {t.admin.cancel}
                </button>
                <button type="button" class={buttonClass("primary")} disabled={busy()} onClick={issue}>
                  {props.user.hasLink ? c.users.newLink : c.users.issueLink}
                </button>
              </div>
            </>
          }
        >
          {(l) => (
            <>
              <p class="text-muted">{c.users.linkOnce}</p>
              <label for="user-link" class="block font-medium">
                {t.admin.linkLabel}
              </label>
              <div class="flex gap-2">
                <input
                  id="user-link"
                  readonly
                  value={l()}
                  class="w-full rounded-md border border-line bg-bg px-3 py-1.5 font-mono text-xs"
                  onFocus={(e) => e.currentTarget.select()}
                />
                <button type="button" class={buttonClass()} onClick={() => copy(l())}>
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
