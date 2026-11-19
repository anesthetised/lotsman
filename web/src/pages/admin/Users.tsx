// Users and their devices (UserService): add a device and hand over its
// config, show a config again, remove devices and users, and issue or
// revoke a user's link to their own page.

import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { JSX } from "@solidjs/web";
import { createMemo, createSignal, Errored, For, Loading, refresh, Show } from "solid-js";
import { admin } from "../../api/admin";
import type { Device, DevicePeer, User } from "../../gen/lotsman/v1/user_pb";
import { useI18n } from "../../i18n";
import { useConsoleI18n } from "../../i18n/console";
import { buttonClass } from "../../ui/button";
import { Dialog } from "../../ui/Dialog";
import { QrCode } from "../../ui/QrCode";
import { LiveError } from "../LiveError";
import { ApiError } from "./ApiError";
import { Confirm } from "./Confirm";
import { configFileName, download, userLink } from "./devices";

export default function Users() {
  const { t } = useI18n();
  return (
    <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
      <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
        <UserList />
      </Loading>
    </Errored>
  );
}

function UserList() {
  const { c } = useConsoleI18n();
  const list = createMemo(() => admin.users.list({}));
  const profiles = createMemo(() => admin.profiles.list({}));
  const users = () => list().users;
  const userNames = createMemo(() => users().map((u) => u.name));
  const profileNames = createMemo(() => profiles().profiles.map((p) => p.name));
  const changed = () => refresh(list);
  return (
    <div class="space-y-4">
      <div class="flex items-center justify-between gap-4">
        <h1 class="text-lg font-semibold">{c.users.title}</h1>
        <AddDevice users={userNames()} profiles={profileNames()} onAdded={changed} />
      </div>
      <Show when={users().length > 0} fallback={<p class="rounded-lg border border-line bg-surface p-4 text-muted">{c.users.empty}</p>}>
        <For each={users()} keyed={(u) => u.name}>
          {(u) => <UserCard user={u()} users={userNames()} profiles={profileNames()} onChanged={changed} />}
        </For>
      </Show>
    </div>
  );
}

function UserCard(props: { user: User; users: readonly string[]; profiles: readonly string[]; onChanged: () => void }) {
  const { t, fmt, c } = useConsoleI18n();
  const u = () => props.user;
  const headingId = () => `user-${u().name}`;
  const peerCount = () => u().devices.reduce((n, d) => n + d.peers.length, 0);
  return (
    <section aria-labelledby={headingId()} class="rounded-lg border border-line bg-surface">
      <div class="flex flex-wrap items-center gap-x-4 gap-y-2 px-3 py-2">
        <h2 id={headingId()} class="font-semibold">
          {u().name}
        </h2>
        <Show when={u().created}>{(created) => <span class="text-xs text-muted">{c.users.since(fmt.dateTime(timestampDate(created())))}</span>}</Show>
        <span class="text-xs">{u().hasLink ? c.users.hasLink : <span class="text-muted">{c.users.noLink}</span>}</span>
        <div class="ml-auto flex flex-wrap gap-2">
          <AddDevice user={u().name} users={props.users} profiles={props.profiles} onAdded={props.onChanged} />
          <IssueLink user={u()} onIssued={props.onChanged} />
          <Show when={u().hasLink}>
            <Confirm
              trigger={
                <>
                  {c.users.revokeLink}
                  <span class="sr-only"> {u().name}</span>
                </>
              }
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
            trigger={
              <>
                {c.users.removeUser}
                <span class="sr-only"> {u().name}</span>
              </>
            }
            title={c.users.removeUserTitle(u().name)}
            description={c.users.removeUserDescription(peerCount())}
            confirm={c.users.removeUser}
            action={async () => {
              await admin.users.removeUser({ user: u().name });
              props.onChanged();
            }}
          />
        </div>
      </div>
      <Show when={u().devices.length > 0} fallback={<p class="border-t border-line px-3 py-2 text-muted">{c.users.noDevices}</p>}>
        <div class="overflow-x-auto border-t border-line">
          <table class="w-full min-w-[44rem]">
            <thead class="text-left text-xs text-muted">
              <tr>
                <th scope="col" class="px-3 py-1.5 font-medium">{t.admin.device}</th>
                <th scope="col" class="px-3 py-1.5 font-medium">{t.admin.profile}</th>
                <th scope="col" class="px-3 py-1.5 font-medium">{t.admin.address}</th>
                <th scope="col" class="px-3 py-1.5 font-medium">{t.admin.created}</th>
                <th scope="col" class="px-3 py-1.5">
                  <span class="sr-only">{c.profiles.actions}</span>
                </th>
              </tr>
            </thead>
            <tbody>
              <For each={u().devices.flatMap((d) => d.peers.map((p) => ({ device: d, peer: p })))} keyed={(r) => r.peer.peerId}>
                {(r) => <PeerRow user={u().name} device={r().device} peer={r().peer} onChanged={props.onChanged} />}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </section>
  );
}

function PeerRow(props: { user: string; device: Device; peer: DevicePeer; onChanged: () => void }) {
  const { fmt, c } = useConsoleI18n();
  const [everyProfile, setEveryProfile] = createSignal(false);
  const what = () => `${props.user}/${props.device.name} · ${props.peer.profile}`;
  return (
    <tr class="border-t border-line">
      <th scope="row" class="px-3 py-1.5 text-left font-medium">
        {props.device.name}
      </th>
      <td class="px-3 py-1.5">{props.peer.profile}</td>
      <td class="px-3 py-1.5 font-mono text-xs">{props.peer.address}</td>
      <td class="px-3 py-1.5">
        <Show when={props.peer.created} fallback="—">
          {(c) => <>{fmt.dateTime(timestampDate(c()))}</>}
        </Show>
      </td>
      <td class="px-3 py-1.5">
        <div class="flex justify-end gap-2">
          <ShowConfig user={props.user} device={props.device.name} peer={props.peer} />
          <a href={`/admin/connections?peer=${encodeURIComponent(props.peer.peerId)}`} class={buttonClass("compact")}>
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
              const profile = everyProfile() ? "" : props.peer.profile;
              await admin.users.removeDevice({ user: props.user, device: props.device.name, profile });
              props.onChanged();
            }}
          >
            <Show when={props.device.peers.length > 1}>
              <label class="flex items-center gap-2">
                <input type="checkbox" checked={everyProfile()} onChange={(e) => setEveryProfile(e.currentTarget.checked)} />
                {c.users.everyProfile(props.device.name, props.device.peers.length)}
              </label>
            </Show>
          </Confirm>
        </div>
      </td>
    </tr>
  );
}

interface AddDeviceProps {
  /** Prefills the user: the button of one user's card. */
  user?: string;
  users: readonly string[];
  profiles: readonly string[];
  onAdded: () => void;
}

/** Adds a device (creating the user if needed) and then shows its config. */
function AddDevice(props: AddDeviceProps) {
  const { t, c } = useConsoleI18n();
  const [open, setOpen] = createSignal(false);
  const [user, setUser] = createSignal("");
  const [device, setDevice] = createSignal("");
  const [profile, setProfile] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();
  const [added, setAdded] = createSignal<{ config: string; file: string; title: string }>();
  const id = () => (props.user ? `add-${props.user}` : "add");

  const openChange = (next: boolean) => {
    setOpen(next);
    if (!next) return;
    setUser(props.user ?? "");
    setDevice("");
    setProfile(props.profiles.includes("default") ? "default" : (props.profiles[0] ?? ""));
    setError(undefined);
    setAdded(undefined);
  };
  const submit = async (e: SubmitEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    const req = { user: user().trim(), device: device().trim(), profile: profile() };
    try {
      const res = await admin.users.addDevice(req);
      const name = req.device || "default";
      setAdded({ config: res.config, file: configFileName(req.user, name, req.profile), title: `${req.user}/${name} · ${req.profile}` });
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
      variant={props.user ? "compact" : "primary"}
      trigger={
        props.user ? (
          <>
            {c.users.addDevice}
            <span class="sr-only"> {c.users.forUser(props.user)}</span>
          </>
        ) : (
          c.users.addDevice
        )
      }
      title={added() ? c.users.configTitle(added()!.title) : c.users.addDeviceTitle}
      description={added() ? c.users.configDescription : c.users.addDeviceDescription}
    >
      <Show
        when={added()}
        fallback={
          <form class="space-y-3 text-sm" onSubmit={submit}>
            <div class="grid gap-3 sm:grid-cols-3">
              <Field id={`${id()}-user`} label={t.admin.user}>
                <input
                  id={`${id()}-user`}
                  required
                  value={user()}
                  onInput={(e) => setUser(e.currentTarget.value)}
                  list={`${id()}-users`}
                  autocomplete="off"
                  class="w-full rounded-md border border-line bg-bg px-3 py-1.5"
                />
                <datalist id={`${id()}-users`}>
                  <For each={props.users}>{(name) => <option value={name} />}</For>
                </datalist>
              </Field>
              <Field id={`${id()}-device`} label={t.admin.device}>
                <input
                  id={`${id()}-device`}
                  value={device()}
                  onInput={(e) => setDevice(e.currentTarget.value)}
                  placeholder="default"
                  autocomplete="off"
                  aria-describedby={`${id()}-device-hint`}
                  class="w-full rounded-md border border-line bg-bg px-3 py-1.5"
                />
              </Field>
              <Field id={`${id()}-profile`} label={t.admin.profile}>
                <select
                  id={`${id()}-profile`}
                  required
                  onChange={(e) => setProfile(e.currentTarget.value)}
                  class="w-full rounded-md border border-line bg-bg px-2 py-1.5"
                >
                  <For each={props.profiles}>
                    {(name) => (
                      <option value={name} selected={name === profile()}>
                        {name}
                      </option>
                    )}
                  </For>
                </select>
              </Field>
            </div>
            <p id={`${id()}-device-hint`} class="text-xs text-muted">
              {c.users.deviceHint}
            </p>
            <Show when={props.profiles.length === 0}>
              <p class="text-bad">{c.users.noProfiles}</p>
            </Show>
            <ApiError error={error()} />
            <div class="flex justify-end gap-2">
              <button type="button" class={buttonClass()} onClick={() => setOpen(false)}>
                {t.admin.cancel}
              </button>
              <button type="submit" class={buttonClass("primary")} disabled={busy() || props.profiles.length === 0}>
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

function Field(props: { id: string; label: string; children?: JSX.Element }) {
  return (
    <div class="space-y-1">
      <label for={props.id} class="block font-medium">
        {props.label}
      </label>
      {props.children}
    </div>
  );
}

/** Shows a device's config again (GetConfig). */
function ShowConfig(props: { user: string; device: string; peer: DevicePeer }) {
  const { t, c } = useConsoleI18n();
  const [open, setOpen] = createSignal(false);
  const [config, setConfig] = createSignal<string>();
  const [error, setError] = createSignal<unknown>();
  const title = () => `${props.user}/${props.device} · ${props.peer.profile}`;
  const openChange = async (next: boolean) => {
    setOpen(next);
    setConfig(undefined);
    setError(undefined);
    if (!next) return;
    try {
      setConfig((await admin.users.getConfig({ peerId: props.peer.peerId })).config);
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
        {(c) => <ConfigView config={c()} file={configFileName(props.user, props.device, props.peer.profile)} label={title()} />}
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
      trigger={
        <>
          {props.user.hasLink ? c.users.newLink : c.users.issueLink}
          <span class="sr-only"> {name()}</span>
        </>
      }
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
              <label for={`link-${name()}`} class="block font-medium">
                {t.admin.linkLabel}
              </label>
              <div class="flex gap-2">
                <input
                  id={`link-${name()}`}
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
