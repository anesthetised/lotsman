import { createMemo, createSignal, createUniqueId, For, Show, untrack } from "solid-js";
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
    <details class="group/filter relative w-full sm:w-auto" onKeyDown={(event) => {
      if (event.key !== "Escape" || !event.currentTarget.open) return;
      event.currentTarget.open = false;
      event.currentTarget.querySelector("summary")?.focus();
    }}>
      <summary class="flex min-h-11 cursor-pointer items-center justify-between gap-3 rounded-md border border-line bg-surface px-3 py-2 [&::-webkit-details-marker]:hidden">
        <span>{active() ? c.connections.selectedDevices(props.count) : c.connections.allPeers}</span>
        <span aria-hidden="true" class="mb-1 size-2 shrink-0 rotate-45 border-r border-b border-muted group-open/filter:mb-0 group-open/filter:-rotate-135" />
      </summary>
      <div class="right-0 z-10 mt-2 flex max-h-[min(60dvh,25rem)] w-full flex-col overflow-hidden rounded-lg border border-line bg-surface text-sm shadow-lg sm:absolute sm:top-full sm:w-72">
        <div class="min-h-0 overflow-y-auto overscroll-contain p-1.5">
          <For each={groups()} keyed={(group) => group.user}>
            {(group) => {
              const whole = () => props.filter.users.includes(group().user);
              const checked = (id: string) => whole() || props.filter.peers.includes(id);
              const partial = () => !whole() && group().peers.some((peer) => checked(peer.peerId));
              const [expanded, setExpanded] = createSignal(untrack(partial));
              const devicesId = createUniqueId();
              return (
                <div role="group" aria-label={group().user}>
                  <div class="flex items-center rounded-md hover:bg-bg">
                    <label class="flex min-h-11 min-w-0 flex-1 cursor-pointer items-center gap-3 px-2">
                      <input type="checkbox" class="size-4 shrink-0 accent-accent" checked={whole()} prop:indeterminate={partial()}
                        aria-label={c.connections.userDevices(group().user)}
                        onChange={(event) => chooseUser(group().user, group().peers, event.currentTarget.checked)} />
                      <span class="min-w-0">
                        <span class="block truncate font-medium" title={group().user}>{group().user}</span>
                        <Show when={!expanded() && group().peers.length === 1 && group().peers[0]}>
                          {(peer) => <span class="block truncate text-xs text-muted" title={peer().device}>{peer().device}</span>}
                        </Show>
                      </span>
                    </label>
                    <button type="button" class="flex size-11 shrink-0 items-center justify-center gap-2 rounded-md text-muted hover:text-fg"
                      aria-label={c.connections.devicesFor(group().user)}
                      aria-expanded={expanded() ? "true" : "false"} aria-controls={devicesId} onClick={() => setExpanded(!expanded())}>
                      <span aria-hidden="true" class="text-xs tabular-nums">{group().peers.length}</span>
                      <span aria-hidden="true" class={["size-1.5 border-r border-b border-current", expanded() ? "mb-0.5 rotate-45" : "-rotate-45"]} />
                    </button>
                  </div>
                  <div id={devicesId} hidden={!expanded()} class="ml-4 border-l border-line pl-3">
                    <For each={group().peers} keyed={(peer) => peer.peerId}>
                      {(peer) => (
                        <label class="flex min-h-11 cursor-pointer items-center gap-3 rounded-md px-2 hover:bg-bg">
                          <input type="checkbox" class="size-4 shrink-0 accent-accent" checked={checked(peer().peerId)}
                            onChange={(event) => choosePeer(group().user, group().peers, peer().peerId, event.currentTarget.checked)} />
                          <span class="min-w-0 wrap-anywhere">{peer().device}</span>
                        </label>
                      )}
                    </For>
                  </div>
                </div>
              );
            }}
          </For>
        </div>
        <Show when={active()}>
          <div class="shrink-0 border-t border-line p-1.5">
            <button type="button" class={[...buttonClass(), "min-h-11 w-full px-3"]} onClick={() => props.onChange({ users: [], peers: [] })}>
              {c.connections.clearFilter}
            </button>
          </div>
        </Show>
      </div>
    </details>
  );
}
