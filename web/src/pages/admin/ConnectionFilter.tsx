import { createMemo, For, Show } from "solid-js";
import type { PeerStatus } from "../../gen/lotsman/v1/status_pb";
import { useConsoleI18n } from "../../i18n/console";
import { buttonClass } from "../../ui/button";

export interface Filter {
  users: readonly string[];
  peers: readonly string[];
}

export function ConnectionFilter(props: {
  peers: readonly PeerStatus[];
  filter: Filter;
  count: number;
  onChange: (filter: Filter) => void;
}) {
  const { c } = useConsoleI18n();
  const groups = createMemo(() => {
    const byUser = new Map<string, PeerStatus[]>();
    for (const peer of props.peers) {
      const peers = byUser.get(peer.user) ?? [];
      peers.push(peer);
      byUser.set(peer.user, peers);
    }
    return [...byUser].sort(([a], [b]) => a.localeCompare(b)).map(([user, peers]) => ({
      user,
      peers: peers.sort((a, b) => a.device.localeCompare(b.device)),
    }));
  });
  const active = () => props.filter.users.length > 0 || props.filter.peers.length > 0;
  const chooseUser = (user: string, peers: readonly PeerStatus[], checked: boolean) => {
    const users = props.filter.users.filter((name) => name !== user);
    if (checked) users.push(user);
    props.onChange({ users, peers: props.filter.peers.filter((id) => !peers.some((peer) => peer.peerId === id)) });
  };
  const choosePeer = (user: string, peers: readonly PeerStatus[], id: string, checked: boolean) => {
    const selected = new Set(props.filter.peers);
    if (props.filter.users.includes(user)) {
      for (const peer of peers) selected.add(peer.peerId);
    }
    if (checked) selected.add(id);
    else selected.delete(id);
    props.onChange({ users: props.filter.users.filter((name) => name !== user), peers: [...selected] });
  };
  return (
    <details class="relative w-full sm:w-auto">
      <summary class="min-h-11 cursor-pointer rounded-md border border-line bg-surface px-3 py-2">
        {active() ? c.connections.selectedDevices(props.count) : c.connections.allPeers}
      </summary>
      <div class="right-0 z-10 mt-2 max-h-[60vh] w-full space-y-2 overflow-auto rounded-lg border border-line bg-surface p-3 shadow-lg sm:absolute sm:w-80">
        <Show when={active()}>
          <button type="button" class={[...buttonClass(), "min-h-11 px-3"]} onClick={() => props.onChange({ users: [], peers: [] })}>
            {c.connections.clearFilter}
          </button>
        </Show>
        <For each={groups()} keyed={(group) => group.user}>
          {(group) => {
            const whole = () => props.filter.users.includes(group().user);
            const checked = (id: string) => whole() || props.filter.peers.includes(id);
            const all = () => group().peers.every((p) => checked(p.peerId));
            return (
              <fieldset class="border-t border-line">
                <legend class="break-all text-sm font-medium">{group().user}</legend>
                <label class="flex min-h-11 cursor-pointer items-center gap-2">
                  <input type="checkbox" class="size-4 accent-accent" checked={all()}
                    aria-label={c.connections.userDevices(group().user)}
                    onChange={(event) => chooseUser(group().user, group().peers, event.currentTarget.checked)} />
                  {c.connections.allDevices}
                </label>
                <For each={group().peers} keyed={(peer) => peer.peerId}>
                  {(peer) => (
                    <label class="flex min-h-11 cursor-pointer items-center gap-2 pl-5">
                      <input type="checkbox" class="size-4 shrink-0 accent-accent" checked={checked(peer().peerId)}
                        onChange={(event) => choosePeer(group().user, group().peers, peer().peerId, event.currentTarget.checked)} />
                      <span class="break-all">{peer().device}</span>
                    </label>
                  )}
                </For>
              </fieldset>
            );
          }}
        </For>
      </div>
    </details>
  );
}
