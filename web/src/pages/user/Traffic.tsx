// The user's traffic: the speed now, and what the last day or week moved.

import { timestampFromMs, durationFromMs } from "@bufbuild/protobuf/wkt";
import { createMemo, createSignal, Errored, For, Loading, Show } from "solid-js";
import { num } from "../../api/live";
import type { StatusSnapshot } from "../../gen/lotsman/v1/status_pb";
import { useI18n } from "../../i18n";
import { Sparkline } from "../../ui/Sparkline";
import { useUser } from "./context";
import { chartPaths, mergeHistory, RANGES, type Range, type TrafficHistory } from "./history";
import { touch } from "./touch";

export function Speed(props: { snapshot: StatusSnapshot; trend: number[] }) {
  const { t, fmt } = useI18n();
  return (
    <section aria-labelledby="speed" class="rounded-xl border border-line bg-surface p-4">
      <h2 id="speed" class="text-sm text-muted">
        {t.user.speedNow}
      </h2>
      <dl class="mt-2 grid grid-cols-2 gap-2">
        <div>
          <dt class="text-xs text-muted">{t.traffic.down}</dt>
          <dd class="text-2xl font-semibold tabular-nums">{fmt.rate(num(props.snapshot.total?.downRate))}</dd>
        </div>
        <div>
          <dt class="text-xs text-muted">{t.traffic.up}</dt>
          <dd class="text-2xl font-semibold tabular-nums">{fmt.rate(num(props.snapshot.total?.upRate))}</dd>
        </div>
      </dl>
      <div class="mt-3">
        <Sparkline values={props.trend} label={t.user.speedTrend} size="h-10 w-full" />
      </div>
    </section>
  );
}

/** The history of the user's peers over a day or a week, summed. Hidden until there is a peer. */
export function History() {
  const { t } = useI18n();
  const ctx = useUser();
  const [range, setRange] = createSignal<Range>("day");
  // The peers, not the whole user: a reload of the devices must not refetch the history.
  const peerIds = createMemo(() => ctx.user().devices.map((d) => d.peerId), {
    equals: (a, b) => a.join() === b.join(),
  });
  const history = createMemo(async (): Promise<TrafficHistory> => {
    const { span, step } = RANGES[range()];
    const to = Date.now();
    const ids = peerIds();
    const responses = await Promise.all(
      ids.map((id) =>
        ctx.self.peerHistory({ name: id, from: timestampFromMs(to - span), to: timestampFromMs(to), step: durationFromMs(step) }),
      ),
    );
    return mergeHistory(new Map(ids.map((id, i) => [id, responses[i]!])));
  });

  return (
    <Show when={peerIds().length > 0}>
      <section aria-labelledby="history" class="space-y-3 rounded-xl border border-line bg-surface p-4">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h2 id="history" class="text-sm text-muted">
            {t.user.history.title}
          </h2>
          <div role="group" aria-label={t.user.history.period} class="flex gap-1">
            <For each={Object.keys(RANGES) as Range[]}>
              {(r) => (
                <button
                  type="button"
                  aria-pressed={range() === r ? "true" : "false"}
                  class={[
                    touch,
                    "rounded-md border px-3 text-sm font-medium",
                    range() === r ? "border-accent bg-accent text-accent-fg" : "border-line bg-surface hover:bg-bg",
                  ]}
                  onClick={() => setRange(r)}
                >
                  {t.user.history[r]}
                </button>
              )}
            </For>
          </div>
        </div>
        <Errored fallback={<p class="text-muted">{t.common.noData}</p>}>
          <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
            <HistoryChart history={history()} period={t.user.history[range()]} />
          </Loading>
        </Errored>
      </section>
    </Show>
  );
}

const WIDTH = 300;
const HEIGHT = 80;

function HistoryChart(props: { history: TrafficHistory; period: string }) {
  const { t, fmt } = useI18n();
  const ctx = useUser();
  const paths = createMemo(() => chartPaths(props.history.points, WIDTH, HEIGHT));
  const total = () => props.history.total;
  const label = () => t.user.history.chart(props.period, fmt.bytes(total().down), fmt.bytes(total().up));
  const devices = createMemo(() =>
    ctx.user().devices.map((d) => ({
      name: d.name,
      down: props.history.byPeer.get(d.peerId)?.down ?? 0,
      up: props.history.byPeer.get(d.peerId)?.up ?? 0,
    })),
  );
  const span = () => {
    const { points } = props.history;
    return [points[0], points.at(-1)] as const;
  };
  return (
    <Show when={total().down + total().up > 0} fallback={<p class="text-muted">{t.user.history.empty}</p>}>
      <figure class="space-y-1">
        <figcaption class="flex flex-wrap justify-between gap-x-3 text-sm">
          <span class="inline-flex items-center gap-1.5 text-chart-down">
            <span aria-hidden="true" class="h-0.5 w-4 bg-current" />
            <span class="text-fg">
              {t.traffic.down} {fmt.bytes(total().down)}
            </span>
          </span>
          <span class="inline-flex items-center gap-1.5 text-chart-up">
            <span aria-hidden="true" class="h-0.5 w-4 bg-current" />
            <span class="text-fg">
              {t.traffic.up} {fmt.bytes(total().up)}
            </span>
          </span>
          <span class="text-muted">{t.user.history.peak(fmt.rate(paths().max))}</span>
        </figcaption>
        <svg role="img" aria-label={label()} viewBox={`0 0 ${WIDTH} ${HEIGHT}`} preserveAspectRatio="none" class="h-32 w-full overflow-visible">
          <line x1="0" y1={HEIGHT} x2={WIDTH} y2={HEIGHT} stroke="currentColor" class="text-line" stroke-width="1" vector-effect="non-scaling-stroke" />
          <path d={paths().down} fill="none" stroke="currentColor" class="text-chart-down" stroke-width="1.5" vector-effect="non-scaling-stroke" />
          <path d={paths().up} fill="none" stroke="currentColor" class="text-chart-up" stroke-width="1.5" vector-effect="non-scaling-stroke" />
        </svg>
        <div class="flex justify-between text-xs text-muted">
          <Show when={span()[0]}>{(first) => <time>{fmt.dateTime(new Date(first().time))}</time>}</Show>
          <Show when={span()[1]}>{(last) => <time>{fmt.dateTime(new Date(last().time))}</time>}</Show>
        </div>
      </figure>
      <Show when={devices().length > 1}>
        <h3 class="mt-3 text-sm text-muted">{t.user.history.byDevice}</h3>
        <ul class="divide-y divide-line text-sm">
          <For each={devices()} keyed={(d) => d.name}>
            {(d) => (
              <li class="flex justify-between gap-3 py-1.5">
                <span class="min-w-0 break-words font-medium">{d().name}</span>
                <span class="tabular-nums text-muted">
                  {t.traffic.down} {fmt.bytes(d().down)} · {t.traffic.up} {fmt.bytes(d().up)}
                </span>
              </li>
            )}
          </For>
        </ul>
      </Show>
    </Show>
  );
}
