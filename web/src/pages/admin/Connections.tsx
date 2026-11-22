// Live connections (ConnectionService.Watch): what each client has open
// through the gateway, from the kernel's connection table. Live only: the
// daemon keeps none of it and neither does this page.

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useSearchParams } from "@solidjs/router";
import { createMemo, createSignal, Errored, For, Loading, Show } from "solid-js";
import { admin } from "../../api/admin";
import { api } from "../../api/client";
import { createLive, num } from "../../api/live";
import { type Connection, Protocol } from "../../gen/lotsman/v1/connection_pb";
import type { PeerStatus } from "../../gen/lotsman/v1/status_pb";
import { useI18n } from "../../i18n";
import { useConsoleI18n } from "../../i18n/console";
import { LiveError } from "../LiveError";
import { ConnectionFilter, type Filter } from "./ConnectionFilter";
import { connectionKey, type SortKey, sortConnections } from "./connectionSort";

export default function Connections() {
  const { t } = useI18n();
  return (
    <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
      <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
        <LiveConnections />
      </Loading>
    </Errored>
  );
}

function LiveConnections() {
  const { t, c } = useConsoleI18n();
  const [params, setParams] = useSearchParams<{ peer?: string | string[]; user?: string | string[] }>();
  const values = (value: string | string[] | undefined) => typeof value === "string" ? [value] : value ?? [];
  const filter = createMemo(() => ({ users: values(params.user), peers: values(params.peer) }));
  const filtered = () => filter().users.length > 0 || filter().peers.length > 0;
  const update = (f: Filter) => setParams({ user: [...f.users], peer: [...f.peers] }, { replace: true });
  const status = createLive((signal) => api.status.watch({}, { signal }));
  const peers = createMemo(() => new Map(status().peers.map((p) => [p.peerId, p])));
  // Compare the selection by value. Live status snapshots must not reopen the stream.
  const peerIds = createMemo(() => {
    const selected = new Set(filter().peers);
    for (const p of status().peers) {
      if (filter().users.includes(p.user)) selected.add(p.peerId);
    }
    return [...selected].sort();
  }, { equals: (a, b) => a.length === b.length && a.every((id, i) => id === b[i]) });
  return (
    <div class="space-y-4">
      <div class="flex flex-wrap items-center justify-between gap-4">
        <h1 class="text-lg font-semibold">{c.connections.title}</h1>
        <ConnectionFilter peers={status().peers} filter={filter()} count={peerIds().length} onChange={update} />
      </div>
      <p class="max-w-3xl text-muted">{c.connections.intro}</p>
      <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
        <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
          <Show when={!filtered() || peerIds().length > 0} fallback={<p class="text-muted">{c.connections.empty}</p>}>
            <ConnectionTable peerIds={peerIds()} peers={peers()} />
          </Show>
        </Loading>
      </Errored>
    </div>
  );
}

const protocols: Record<Protocol, string> = {
  [Protocol.UNSPECIFIED]: "—",
  [Protocol.TCP]: "TCP",
  [Protocol.UDP]: "UDP",
  [Protocol.ICMP]: "ICMP",
};

function ConnectionTable(props: { peerIds: readonly string[]; peers: ReadonlyMap<string, PeerStatus> }) {
  const { fmt, c } = useConsoleI18n();
  const snapshot = createLive((signal) => admin.connections.watch({ peerIds: [...props.peerIds] }, { signal }));
  const [sort, setSort] = createSignal<SortKey>("downRate");
  const [ascending, setAscending] = createSignal(false);
  const rows = createMemo(() => sortConnections(snapshot().connections, sort(), ascending()));
  const time = () => (snapshot().time ? timestampDate(snapshot().time!) : new Date());
  const sortBy = (key: SortKey) => {
    if (sort() === key) setAscending((a) => !a);
    else {
      setSort(key);
      setAscending(false);
    }
  };
  const SortHeader = (h: { key: SortKey; label: string }) => (
    <th scope="col" class="px-3 py-2 text-right font-medium" aria-sort={sort() === h.key ? (ascending() ? "ascending" : "descending") : undefined}>
      <button type="button" class="inline-flex items-center gap-1 hover:text-fg" onClick={() => sortBy(h.key)}>
        {h.label}
        <span aria-hidden="true" class="w-3">
          {sort() === h.key ? (ascending() ? "▲" : "▼") : ""}
        </span>
      </button>
    </th>
  );
  const peerName = (c: Connection) => {
    const p = props.peers.get(c.peerId);
    return p ? `${p.user}/${p.device}` : c.peerId;
  };
  return (
    <section class="space-y-2" aria-labelledby="connections-summary">
      <p id="connections-summary" class="flex flex-wrap gap-x-4 text-muted">
        <span>{c.connections.count(snapshot().connections.length)}</span>
        <span>{c.connections.updated(fmt.time(time()))}</span>
      </p>
      <Show when={snapshot().truncated}>
        <p role="status" class="rounded-md border border-warn p-2">
          {c.connections.truncated(snapshot().connections.length)}
        </p>
      </Show>
      <Show
        when={rows().length > 0}
        fallback={<p class="rounded-lg border border-line bg-surface p-4 text-muted">{c.connections.empty}</p>}
      >
        <div class="overflow-x-auto rounded-lg border border-line bg-surface">
          <table class="w-full min-w-[60rem]">
            <caption class="sr-only">{c.connections.caption}</caption>
            <thead class="text-left text-xs text-muted">
              <tr>
                <th scope="col" class="px-3 py-2 font-medium">{c.connections.peer}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.connections.protocol}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.connections.source}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.connections.destination}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.connections.upstream}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.connections.started}</th>
                <th scope="col" class="px-3 py-2 font-medium whitespace-nowrap">{c.connections.lastTraffic}</th>
                <SortHeader key="downRate" label={c.connections.downRate} />
                <SortHeader key="upRate" label={c.connections.upRate} />
                <SortHeader key="downBytes" label={c.connections.downBytes} />
                <SortHeader key="upBytes" label={c.connections.upBytes} />
              </tr>
            </thead>
            <tbody>
              <For each={rows()} keyed={connectionKey}>
                {(c) => (
                  <tr class="border-t border-line">
                    <td class="px-3 py-1">{peerName(c())}</td>
                    <td class="px-3 py-1">{protocols[c().protocol]}</td>
                    <td class="px-3 py-1 font-mono text-xs">{c().source}</td>
                    <td class="px-3 py-1 font-mono text-xs">{c().destination}</td>
                    <td class="px-3 py-1">{c().upstream || "—"}</td>
                    <td class="px-3 py-1 whitespace-nowrap">
                      <Show when={c().started} fallback="—">
                        {(s) => <>{fmt.ago(timestampDate(s()), time())}</>}
                      </Show>
                    </td>
                    <td class="px-3 py-1 whitespace-nowrap">
                      <Show when={c().lastTraffic} fallback="—">
                        {(s) => {
                          const last = () => timestampDate(s());
                          return <time datetime={last().toISOString()} title={fmt.dateTime(last())}>{fmt.ago(last(), time())}</time>;
                        }}
                      </Show>
                    </td>
                    <td class="px-3 py-1 text-right tabular-nums">{fmt.rate(num(c().traffic?.downRate))}</td>
                    <td class="px-3 py-1 text-right tabular-nums">{fmt.rate(num(c().traffic?.upRate))}</td>
                    <td class="px-3 py-1 text-right tabular-nums">{fmt.bytes(num(c().traffic?.downBytes))}</td>
                    <td class="px-3 py-1 text-right tabular-nums">{fmt.bytes(num(c().traffic?.upBytes))}</td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </section>
  );
}
