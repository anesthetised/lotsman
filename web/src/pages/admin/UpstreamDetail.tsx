// One upstream: its live state and rate, its history (traffic, latency and
// availability over a chosen range) and the peers routed through it.

import { durationFromMs, durationMs, timestampDate, timestampFromMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { useParams } from "@solidjs/router";
import { createMemo, createSignal, Errored, For, Loading, Show } from "solid-js";
import { api } from "../../api/client";
import { appendRow, createLive, num } from "../../api/live";
import type { PeerStatus, StatusSnapshot, UpstreamStatus } from "../../gen/lotsman/v1/status_pb";
import { useI18n } from "../../i18n";
import { useConsoleI18n } from "../../i18n/console";
import { AvailabilityBar } from "../../ui/AvailabilityBar";
import { buttonClass } from "../../ui/button";
import { ChoiceMenu } from "../../ui/ChoiceMenu";
import { LineChart } from "../../ui/LineChart";
import { StateBadge } from "../../ui/StateBadge";
import { LiveError } from "../LiveError";
import { kindLabel, stateLabel, stateLevel } from "../upstream";
import { historyRanges, type RangeKey, rateSeries } from "./history";

export default function UpstreamDetail() {
  const { t } = useI18n();
  return (
    <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
      <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
        <LiveUpstream />
      </Loading>
    </Errored>
  );
}

function LiveUpstream() {
  const { t, c } = useConsoleI18n();
  const params = useParams<{ name: string }>();
  const snapshot = createLive((signal) => api.status.watch({}, { signal }));
  const upstream = createMemo(() => snapshot().upstreams.find((u) => u.name === params.name));
  return (
    <div class="space-y-4">
      <a href="/admin" class="text-muted hover:text-fg">
        ← {t.admin.upstreams}
      </a>
      <Show
        when={upstream()}
        fallback={
          <>
            <h1 class="text-lg font-semibold">{c.upstream.title(params.name)}</h1>
            <p class="text-muted">{c.upstream.missing}</p>
          </>
        }
      >
        {(u) => <Upstream upstream={u()} snapshot={snapshot()} />}
      </Show>
    </div>
  );
}

function Upstream(props: { upstream: UpstreamStatus; snapshot: StatusSnapshot }) {
  const { t, fmt, c } = useConsoleI18n();
  const u = () => props.upstream;
  const now = () => (props.snapshot.time ? timestampDate(props.snapshot.time) : new Date());
  // The last five minutes of the live rate.
  const live = createMemo<number[][]>((prev) => {
    const time = props.snapshot.time ? timestampMs(props.snapshot.time) / 1000 : 0;
    return appendRow(prev, [time, num(u().traffic?.downRate), num(u().traffic?.upRate)], 5 * 60);
  });
  const peers = createMemo(() => props.snapshot.peers.filter((p) => p.upstream === u().name));
  const fact = (label: string, value: string) => (
    <div>
      <dt class="text-xs text-muted">{label}</dt>
      <dd class="font-medium tabular-nums">{value}</dd>
    </div>
  );
  return (
    <>
      <div class="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        <h1 class="text-lg font-semibold">{c.upstream.title(u().name)}</h1>
        <StateBadge level={stateLevel(u().state)} label={stateLabel(t, u().state)} />
        <span class="text-muted">
          {[kindLabel(u().kind), u().geo, u().provider].filter(Boolean).join(" · ")}
        </span>
      </div>

      <dl class="grid grid-cols-2 gap-3 rounded-lg border border-line bg-surface p-3 sm:grid-cols-3 lg:grid-cols-6">
        {fact(t.admin.latency, u().latency ? fmt.milliseconds(durationMs(u().latency!)) : "—")}
        {fact(c.upstream.handshake, u().lastHandshake ? fmt.ago(timestampDate(u().lastHandshake!), now()) : "—")}
        {fact(t.admin.peers, t.admin.activePeers(u().active, u().routed))}
        {fact(c.upstream.downNow, fmt.rate(num(u().traffic?.downRate)))}
        {fact(c.upstream.upNow, fmt.rate(num(u().traffic?.upRate)))}
        {fact(c.upstream.sinceStart, `${fmt.bytes(num(u().traffic?.downBytes))} / ${fmt.bytes(num(u().traffic?.upBytes))}`)}
      </dl>

      <section class="rounded-lg border border-line bg-surface p-3">
        <LineChart
          data={live()}
          series={[
            { label: t.traffic.down, color: "--color-chart-down" },
            { label: t.traffic.up, color: "--color-chart-up" },
          ]}
          format={fmt.rate}
          height={160}
          caption={
            <span class="flex flex-wrap items-center gap-x-4 gap-y-1">
              <span class="font-medium">{c.upstream.liveChart}</span>
              <Legend color="bg-chart-down" text={`${t.traffic.down}: ${fmt.rate(num(u().traffic?.downRate))}`} />
              <Legend color="bg-chart-up" text={`${t.traffic.up}: ${fmt.rate(num(u().traffic?.upRate))}`} />
            </span>
          }
        />
      </section>

      <History name={u().name} />

      <section aria-labelledby="routed" class="space-y-2">
        <h2 id="routed" class="font-semibold">
          {c.upstream.routed}
        </h2>
        <PeerTable peers={peers()} now={now()} />
      </section>
    </>
  );
}

function Legend(props: { color: string; text: string }) {
  return (
    <span class="tabular-nums">
      <span aria-hidden="true" class={["mr-1 inline-block size-2 rounded-full", props.color]} />
      {props.text}
    </span>
  );
}

function History(props: { name: string }) {
  const { t, c } = useConsoleI18n();
  const [range, setRange] = createSignal<RangeKey>("24h");
  return (
    <section aria-labelledby="history" class="space-y-3 rounded-lg border border-line bg-surface p-3">
      <div class="flex items-center justify-between gap-4">
        <h2 id="history" class="font-semibold">
          {c.upstream.history}
        </h2>
        <ChoiceMenu
          label={
            <>
              {c.upstream.range}: {c.upstream.ranges[range()]}
            </>
          }
          value={range()}
          choices={historyRanges.map((r) => ({ value: r.key, label: c.upstream.ranges[r.key] }))}
          onChange={setRange}
        />
      </div>
      <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
        <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
          <HistoryCharts name={props.name} range={range()} />
        </Loading>
      </Errored>
    </section>
  );
}

function HistoryCharts(props: { name: string; range: RangeKey }) {
  const { t, fmt, c } = useConsoleI18n();
  const history = createMemo(() => {
    const r = historyRanges.find((x) => x.key === props.range)!;
    const to = Date.now();
    return api.history.upstream({
      name: props.name,
      from: timestampFromMs(to - r.span),
      to: timestampFromMs(to),
      step: durationFromMs(r.step),
    });
  });
  const step = () => (history().step ? durationMs(history().step!) : 60_000);
  const traffic = createMemo(() => rateSeries(history().samples, step()));
  const latency = createMemo(() => [
    history().samples.map((s) => (s.time ? timestampMs(s.time) / 1000 : 0)),
    history().samples.map((s) => (s.latency ? durationMs(s.latency) : null)),
  ]);
  const ratios = createMemo(() => history().samples.map((s) => s.upRatio));
  const availability = () => ratios().reduce((a, b) => a + b, 0) / Math.max(ratios().length, 1);
  const total = (i: 1 | 2) => history().samples.reduce((sum, s) => sum + num(i === 1 ? s.downBytes : s.upBytes), 0);
  const latencies = () => latency()[1]!.filter((v): v is number => v !== null);
  const meanLatency = () => latencies().reduce((a, b) => a + b, 0) / Math.max(latencies().length, 1);
  const rangeLabel = () => c.upstream.ranges[props.range];
  return (
    <div class="space-y-4">
      <div class="space-y-1">
        <p class="text-xs text-muted">{c.upstream.availability(rangeLabel(), fmt.percent(availability()))}</p>
        <AvailabilityBar ratios={ratios()} label={`${c.upstream.availability(rangeLabel(), fmt.percent(availability()))}`} />
      </div>
      <Show when={history().samples.length > 0} fallback={<p class="text-muted">{t.common.noData}</p>}>
        <div class="grid gap-4 lg:grid-cols-2">
          <LineChart
            data={traffic()}
            axis="date"
            series={[
              { label: t.traffic.down, color: "--color-chart-down" },
              { label: t.traffic.up, color: "--color-chart-up" },
            ]}
            format={fmt.rate}
            height={180}
            caption={
              <span class="flex flex-wrap items-center gap-x-4 gap-y-1">
                <span class="font-medium">{c.upstream.trafficChart}</span>
                <Legend color="bg-chart-down" text={`${t.traffic.down}: ${fmt.bytes(total(1))}`} />
                <Legend color="bg-chart-up" text={`${t.traffic.up}: ${fmt.bytes(total(2))}`} />
              </span>
            }
          />
          <LineChart
            data={latency()}
            axis="date"
            series={[{ label: t.admin.latency, color: "--color-chart-down" }]}
            format={fmt.milliseconds}
            height={180}
            caption={
              <span class="flex flex-wrap items-center gap-x-4 gap-y-1">
                <span class="font-medium">{c.upstream.latencyChart}</span>
                <span class="tabular-nums">{c.upstream.meanLatency(latencies().length > 0 ? fmt.milliseconds(meanLatency()) : "—")}</span>
              </span>
            }
          />
        </div>
      </Show>
    </div>
  );
}

function PeerTable(props: { peers: readonly PeerStatus[]; now: Date }) {
  const { t, fmt, c } = useConsoleI18n();
  return (
    <Show when={props.peers.length > 0} fallback={<p class="rounded-lg border border-line bg-surface p-4 text-muted">{t.admin.noPeers}</p>}>
      <div class="overflow-x-auto rounded-lg border border-line bg-surface">
        <table class="w-full min-w-[52rem]">
          <thead class="text-left text-xs text-muted">
            <tr>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.user}</th>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.device}</th>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.address}</th>
              <th scope="col" class="px-3 py-2 font-medium">{c.upstream.handshake}</th>
              <th scope="col" class="px-3 py-2 text-right font-medium">{t.traffic.down}</th>
              <th scope="col" class="px-3 py-2 text-right font-medium">{t.traffic.up}</th>
              <th scope="col" class="px-3 py-2">
                <span class="sr-only">{c.connections.title}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            <For each={props.peers} keyed={(p) => p.peerId}>
              {(p) => (
                <tr class="border-t border-line">
                  <th scope="row" class="px-3 py-1.5 text-left font-medium">
                    {p().user}
                  </th>
                  <td class="px-3 py-1.5">{p().device}</td>
                  <td class="px-3 py-1.5 font-mono text-xs">{p().address}</td>
                  <td class="px-3 py-1.5">
                    <Show when={p().lastHandshake} fallback={t.admin.never}>
                      {(h) => <>{fmt.ago(timestampDate(h()), props.now)}</>}
                    </Show>
                  </td>
                  <td class="px-3 py-1.5 text-right tabular-nums">{fmt.rate(num(p().traffic?.downRate))}</td>
                  <td class="px-3 py-1.5 text-right tabular-nums">{fmt.rate(num(p().traffic?.upRate))}</td>
                  <td class="px-3 py-1.5 text-right">
                    <a href={`/admin/connections?peer=${encodeURIComponent(p().peerId)}`} class={buttonClass("compact")}>
                      {c.connections.title}
                      <span class="sr-only">
                        {" "}
                        {p().user}/{p().device}
                      </span>
                    </a>
                  </td>
                </tr>
              )}
            </For>
          </tbody>
        </table>
      </div>
    </Show>
  );
}
