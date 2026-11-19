// Placeholder operator overview (#136 builds the real console): live upstream
// table, a total-rate chart, per-upstream sparklines and 7-day availability.

import { durationMs, timestampFromMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { createMemo, createSignal, Errored, For, Loading, Show } from "solid-js";
import { api } from "../../api/client";
import { appendKeyed, appendRow, createLive, num } from "../../api/live";
import type { PeerStatus, UpstreamStatus } from "../../gen/lotsman/v1/lotsman_pb";
import { useI18n } from "../../i18n";
import { AvailabilityBar } from "../../ui/AvailabilityBar";
import { ChoiceMenu } from "../../ui/ChoiceMenu";
import { Dialog } from "../../ui/Dialog";
import { LineChart } from "../../ui/LineChart";
import { Sparkline } from "../../ui/Sparkline";
import { StateBadge } from "../../ui/StateBadge";
import { LiveError } from "../LiveError";
import { kindLabel, stateLabel, stateLevel } from "../upstream";

const DAY = 86_400_000;
const windows = ["1", "5", "15"] as const;
type Window = (typeof windows)[number];

export default function Overview() {
  const { t } = useI18n();
  return (
    <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
      <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
        <LiveOverview />
      </Loading>
    </Errored>
  );
}

function LiveOverview() {
  const { t, fmt } = useI18n();
  const snapshot = createLive(() => api.status.watch({}));
  const [window, setWindow] = createSignal<Window>("1");

  // Keep the longest window; the chart shows its tail.
  const totals = createMemo<number[][]>((prev) => {
    const s = snapshot();
    const time = s.time ? timestampMs(s.time) / 1000 : 0;
    return appendRow(prev, [time, num(s.total?.downRate), num(s.total?.upRate)], 15 * 60);
  });
  const chart = createMemo(() => totals().map((column) => column.slice(-Number(window()) * 60)));
  const trends = createMemo<Record<string, number[]>>((prev) =>
    appendKeyed(prev, Object.fromEntries(snapshot().upstreams.map((u) => [u.name, num(u.traffic?.downRate)])), 60),
  );
  const peersByUpstream = createMemo(() => Object.groupBy(snapshot().peers, (p) => p.upstream));

  return (
    <div class="space-y-4">
      <div class="flex items-center justify-between gap-4">
        <h1 class="text-lg font-semibold">{t.admin.upstreams}</h1>
        <ChoiceMenu
          label={
            <>
              {t.admin.window}: {t.admin.windowMinutes(Number(window()))}
            </>
          }
          value={window()}
          choices={windows.map((w) => ({ value: w, label: t.admin.windowMinutes(Number(w)) }))}
          onChange={setWindow}
        />
      </div>

      <section class="rounded-lg border border-line bg-surface p-3">
        <LineChart
          data={chart()}
          series={[
            { label: t.traffic.down, color: "--color-chart-down" },
            { label: t.traffic.up, color: "--color-chart-up" },
          ]}
          format={fmt.rate}
          height={180}
          caption={
            <span class="flex flex-wrap items-center gap-x-4 gap-y-1">
              <span class="font-medium">{t.admin.totalChart}</span>
              <span class="tabular-nums">
                <span aria-hidden="true" class="mr-1 inline-block size-2 rounded-full bg-chart-down" />
                {t.traffic.down}: {fmt.rate(num(snapshot().total?.downRate))}
              </span>
              <span class="tabular-nums">
                <span aria-hidden="true" class="mr-1 inline-block size-2 rounded-full bg-chart-up" />
                {t.traffic.up}: {fmt.rate(num(snapshot().total?.upRate))}
              </span>
            </span>
          }
        />
      </section>

      <section class="overflow-x-auto rounded-lg border border-line bg-surface">
        <table class="w-full min-w-[56rem]">
          <thead class="text-left text-xs text-muted">
            <tr>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.name}</th>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.state}</th>
              <th scope="col" class="px-3 py-2 text-right font-medium">{t.admin.latency}</th>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.peers}</th>
              <th scope="col" class="px-3 py-2 text-right font-medium">{t.traffic.down}</th>
              <th scope="col" class="px-3 py-2 text-right font-medium">{t.traffic.up}</th>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.lastMinute}</th>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.availability}</th>
              <th scope="col" class="px-3 py-2">
                <span class="sr-only">{t.admin.details}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            <For each={snapshot().upstreams} keyed={(u) => u.name}>
              {(u) => (
                <UpstreamRow
                  upstream={u()}
                  trend={trends()[u().name] ?? []}
                  peers={peersByUpstream()[u().name] ?? []}
                />
              )}
            </For>
          </tbody>
        </table>
      </section>
    </div>
  );
}

function UpstreamRow(props: { upstream: UpstreamStatus; trend: readonly number[]; peers: readonly PeerStatus[] }) {
  const { t, fmt } = useI18n();
  const u = () => props.upstream;
  return (
    <tr class="border-t border-line">
      <th scope="row" class="px-3 py-1.5 text-left font-medium">
        {u().name}
        <div class="text-xs font-normal text-muted">
          {kindLabel(u().kind)} · {u().geo}
        </div>
      </th>
      <td class="px-3 py-1.5">
        <StateBadge level={stateLevel(u().state)} label={stateLabel(t, u().state)} />
      </td>
      <td class="px-3 py-1.5 text-right tabular-nums">
        <Show when={u().latency} fallback="—">
          {(latency) => <>{fmt.milliseconds(durationMs(latency()))}</>}
        </Show>
      </td>
      <td class="px-3 py-1.5 tabular-nums">{t.admin.activePeers(u().active, u().routed)}</td>
      <td class="px-3 py-1.5 text-right tabular-nums">{fmt.rate(num(u().traffic?.downRate))}</td>
      <td class="px-3 py-1.5 text-right tabular-nums">{fmt.rate(num(u().traffic?.upRate))}</td>
      <td class="px-3 py-1.5">
        <Sparkline values={props.trend} label={t.admin.rateTrend(u().name)} />
      </td>
      <td class="w-44 px-3 py-1.5">
        <Availability name={u().name} />
      </td>
      <td class="px-3 py-1.5 text-right">
        <Dialog
          trigger={
            <>
              {t.admin.details}
              <span class="sr-only"> {u().name}</span>
            </>
          }
          title={t.admin.detailsTitle(u().name)}
          description={t.admin.detailsDescription}
        >
          <PeerTable peers={props.peers} />
        </Dialog>
      </td>
    </tr>
  );
}

/** The upstream's hourly availability over the last 7 days. */
function Availability(props: { name: string }) {
  const { t, fmt } = useI18n();
  const history = createMemo(() => {
    const to = Date.now();
    return api.history.upstream({ name: props.name, from: timestampFromMs(to - 7 * DAY), to: timestampFromMs(to) });
  });
  const ratios = createMemo(() => history().samples.map((s) => s.upRatio));
  const overall = () => ratios().reduce((a, b) => a + b, 0) / Math.max(ratios().length, 1);
  return (
    <Errored fallback={<span class="text-muted">{t.common.noData}</span>}>
      <Loading fallback={<span class="text-muted">…</span>}>
        <AvailabilityBar ratios={ratios()} label={`${t.admin.availabilityLabel(props.name)}: ${fmt.percent(overall())}`} />
      </Loading>
    </Errored>
  );
}

function PeerTable(props: { peers: readonly PeerStatus[] }) {
  const { t, fmt } = useI18n();
  return (
    <Show when={props.peers.length > 0} fallback={<p class="text-sm text-muted">{t.admin.noPeers}</p>}>
      <table class="w-full text-sm">
        <thead class="text-left text-xs text-muted">
          <tr>
            <th scope="col" class="py-1 pr-3 font-medium">{t.admin.user}</th>
            <th scope="col" class="py-1 pr-3 font-medium">{t.admin.device}</th>
            <th scope="col" class="py-1 pr-3 font-medium">{t.admin.profile}</th>
            <th scope="col" class="py-1 pr-3 font-medium">{t.admin.address}</th>
            <th scope="col" class="py-1 text-right font-medium">{t.traffic.down}</th>
          </tr>
        </thead>
        <tbody>
          <For each={props.peers} keyed={(p) => p.peerId}>
            {(p) => (
              <tr class="border-t border-line">
                <td class="py-1 pr-3">{p().user}</td>
                <td class="py-1 pr-3">{p().device}</td>
                <td class="py-1 pr-3">{p().profile}</td>
                <td class="py-1 pr-3 font-mono text-xs">{p().address}</td>
                <td class="py-1 text-right tabular-nums">{fmt.rate(num(p().traffic?.downRate))}</td>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </Show>
  );
}
