// The operator overview: gateway totals, a total-rate chart, and the live
// upstream table with per-upstream sparklines and 7-day availability. Each
// upstream's name leads to its own page.

import { durationMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { createMemo, createSignal, Errored, For, Loading, Show } from "solid-js";
import { api } from "../../api/client";
import { appendKeyed, appendRow, createLive, num } from "../../api/live";
import { type StatusSnapshot, UpstreamKind, type UpstreamStatus } from "../../gen/lotsman/v1/status_pb";
import { useI18n } from "../../i18n";
import { useConsoleI18n } from "../../i18n/console";
import { ChoiceMenu } from "../../ui/ChoiceMenu";
import { LineChart } from "../../ui/LineChart";
import { Sparkline } from "../../ui/Sparkline";
import { StateBadge } from "../../ui/StateBadge";
import { Availability } from "../Availability";
import { LiveError } from "../LiveError";
import { kindLabel, stateLabel, stateLevel } from "../upstream";
import { IncidentBadge } from "./IncidentList";
import { OutlineQuota, type OutlineQuotaValue, useOutlineUsage } from "./OutlineQuota";
import { SubscriptionExpiry, subscriptionExpiry } from "./SubscriptionExpiry";

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
  const { t, fmt, c } = useConsoleI18n();
  const snapshot = createLive((signal) => api.status.watch({}, { signal }));
  const outlineUsage = useOutlineUsage();
  const showQuota = createMemo(() =>
    snapshot().upstreams.some((upstream) => upstream.kind === UpstreamKind.OUTLINE && outlineUsage()[upstream.name] !== undefined),
  );
  const showExpiry = createMemo(() => snapshot().upstreams.some((u) => u.expiresAt));
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

  return (
    <div class="space-y-4">
      <Totals snapshot={snapshot()} />

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
              <Show when={showExpiry()}>
                <th scope="col" class="px-3 py-2 font-medium">{c.upstream.expiresAt}</th>
              </Show>
              <Show when={showQuota()}>
                <th scope="col" class="px-3 py-2 font-medium">{c.outlineUsage.title}</th>
              </Show>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.state}</th>
              <th scope="col" class="px-3 py-2 text-right font-medium">{t.admin.latency}</th>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.peers}</th>
              <th scope="col" class="px-3 py-2 text-right font-medium">{t.traffic.down}</th>
              <th scope="col" class="px-3 py-2 text-right font-medium">{t.traffic.up}</th>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.lastMinute}</th>
              <th scope="col" class="px-3 py-2 font-medium">{t.admin.availability}</th>
            </tr>
          </thead>
          <tbody>
            <For each={snapshot().upstreams} keyed={(u) => u.name}>
              {(u) => (
                <UpstreamRow
                  upstream={u()}
                  trend={trends()[u().name] ?? []}
                  showQuota={showQuota()}
                  showExpiry={showExpiry()}
                  quota={outlineUsage()[u().name]}
                />
              )}
            </For>
          </tbody>
        </table>
      </section>
    </div>
  );
}

/** The gateway as a whole: every peer's traffic, removed peers' bytes included. */
function Totals(props: { snapshot: StatusSnapshot }) {
  const { t, fmt, c } = useConsoleI18n();
  const s = () => props.snapshot;
  const active = () => s().upstreams.reduce((n, u) => n + u.active, 0);
  const blocked = () => s().peers.filter((p) => p.upstream === "").length;
  const tile = (label: string, value: string, detail?: string) => (
    <div class="rounded-lg border border-line bg-surface px-3 py-2">
      <dt class="text-xs text-muted">{label}</dt>
      <dd class="text-lg font-semibold tabular-nums">{value}</dd>
      <Show when={detail}>
        <dd class="text-xs text-muted">{detail}</dd>
      </Show>
    </div>
  );
  return (
    <section aria-labelledby="totals">
      <h2 id="totals" class="sr-only">
        {c.overview.totals}
      </h2>
      <dl class="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {tile(c.upstream.downNow, fmt.rate(num(s().total?.downRate)), c.overview.sinceStart(fmt.bytes(num(s().total?.downBytes))))}
        {tile(c.upstream.upNow, fmt.rate(num(s().total?.upRate)), c.overview.sinceStart(fmt.bytes(num(s().total?.upBytes))))}
        {tile(t.admin.peers, t.admin.activePeers(active(), s().peers.length), blocked() > 0 ? c.overview.blocked(blocked()) : undefined)}
        {tile(c.overview.clientMtu, fmt.count(s().clientMtu))}
      </dl>
    </section>
  );
}

function UpstreamRow(props: {
  upstream: UpstreamStatus;
  trend: readonly number[];
  showQuota: boolean;
  showExpiry: boolean;
  quota?: OutlineQuotaValue | null;
}) {
  const { t, fmt } = useI18n();
  const u = () => props.upstream;
  return (
    <tr class="border-t border-line">
      <th scope="row" class="px-3 py-1.5 text-left font-medium">
        <a href={`/admin/upstreams/${encodeURIComponent(u().name)}`} class="text-accent underline-offset-2 hover:underline">
          {u().name}
        </a>
        <div class="text-xs font-normal text-muted">
          {kindLabel(u().kind)} · {u().geo}
        </div>
      </th>
      <Show when={props.showExpiry}>
        <td class="px-3 py-1.5 tabular-nums">
          <Show when={subscriptionExpiry(u())} fallback={<span class="text-muted">—</span>}>
            {(date) => <SubscriptionExpiry date={date()} />}
          </Show>
        </td>
      </Show>
      <Show when={props.showQuota}>
        <td class="px-3 py-1.5">
          <Show when={u().kind === UpstreamKind.OUTLINE && props.quota !== undefined} fallback={<span class="text-muted">—</span>}>
            <OutlineQuota quota={props.quota} />
          </Show>
        </td>
      </Show>
      <td class="px-3 py-1.5">
        <div class="flex flex-col items-start gap-1">
          <StateBadge level={stateLevel(u().state, u().routingBlock)} label={stateLabel(t, u().state, u().routingBlock)} />
          <IncidentBadge upstream={u().name} kinds={u().incidents} />
        </div>
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
        <Availability name={u().name} label={t.admin.availabilityLabel(u().name)} load={api.history.upstream} />
      </td>
    </tr>
  );
}
