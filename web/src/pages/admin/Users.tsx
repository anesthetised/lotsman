// Users (UserService): the list, and adding a user. A user's devices,
// routes, rights and link are on their own page (UserDetail.tsx).

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useNavigate } from "@solidjs/router";
import { createMemo, createSignal, Errored, For, Loading, Show } from "solid-js";
import { admin } from "../../api/admin";
import { useI18n } from "../../i18n";
import { useConsoleI18n } from "../../i18n/console";
import { buttonClass } from "../../ui/button";
import { Dialog } from "../../ui/Dialog";
import { LiveError } from "../LiveError";
import { ApiError } from "./ApiError";
import { rightsSummary, userPath } from "./devices";

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
  const { t, fmt, c } = useConsoleI18n();
  const list = createMemo(() => admin.users.list({}));
  const users = () => list().users;
  return (
    <div class="space-y-4">
      <div class="flex items-center justify-between gap-4">
        <h1 class="text-lg font-semibold">{c.users.title}</h1>
        <AddUser />
      </div>
      <Show when={users().length > 0} fallback={<p class="rounded-lg border border-line bg-surface p-4 text-muted">{c.users.empty}</p>}>
        <div class="overflow-x-auto rounded-lg border border-line bg-surface">
          <table class="w-full min-w-[40rem]">
            <thead class="text-left text-xs text-muted">
              <tr>
                <th scope="col" class="px-3 py-2 font-medium">{t.admin.user}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.users.devices}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.users.rightsSummary}</th>
                <th scope="col" class="px-3 py-2 font-medium">{t.admin.linkLabel}</th>
                <th scope="col" class="px-3 py-2 font-medium">{t.admin.created}</th>
              </tr>
            </thead>
            <tbody>
              <For each={users()} keyed={(u) => u.name}>
                {(u) => (
                  <tr class="border-t border-line">
                    <th scope="row" class="px-3 py-1.5 text-left">
                      <a href={userPath(u().name)} class="font-medium text-accent hover:underline">
                        {u().name}
                      </a>
                    </th>
                    <td class="px-3 py-1.5">{c.users.deviceCount(u().devices.length)}</td>
                    <td class="px-3 py-1.5">{rightsSummary(c, u().rights)}</td>
                    <td class="px-3 py-1.5">{u().hasLink ? c.users.hasLink : <span class="text-muted">{c.users.noLink}</span>}</td>
                    <td class="px-3 py-1.5">
                      <Show when={u().created} fallback="—">
                        {(created) => <>{fmt.dateTime(timestampDate(created()))}</>}
                      </Show>
                    </td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </div>
  );
}

/** Adds a user without devices, then opens their page. */
function AddUser() {
  const { t, c } = useConsoleI18n();
  const navigate = useNavigate();
  const [open, setOpen] = createSignal(false);
  const [name, setName] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();
  const openChange = (next: boolean) => {
    setOpen(next);
    setName("");
    setError(undefined);
  };
  const submit = async (e: SubmitEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const user = await admin.users.addUser({ user: name().trim() });
      setOpen(false);
      navigate(userPath(user.name));
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open={open()} onOpenChange={openChange} variant="primary" trigger={c.users.addUser} title={c.users.addUserTitle} description={c.users.addUserDescription}>
      <form class="space-y-3 text-sm" onSubmit={submit}>
        <div class="space-y-1">
          <label for="add-user-name" class="block font-medium">
            {c.users.name}
          </label>
          <input
            id="add-user-name"
            required
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
            autocomplete="off"
            spellcheck={false}
            class="w-full rounded-md border border-line bg-bg px-3 py-1.5"
          />
        </div>
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
    </Dialog>
  );
}
