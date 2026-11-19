// Placeholder user page behind the secret link `/u/<token>` (#137 builds the
// real one): mobile-first cards with the live speed, the user's devices and
// the 7-day availability of the exits they use.
// The panel resolves the token and scopes UserSelfService to that user, so
// the page itself never sends the token anywhere (see web/README.md).

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createMemo, Errored, For, Loading, Show } from "solid-js";
import { api } from "../../api/client";
import { createLive, num } from "../../api/live";
import type { PeerStatus, UpstreamStatus } from "../../gen/lotsman/v1/status_pb";
import type { DevicePeer } from "../../gen/lotsman/v1/user_pb";
import { useI18n } from "../../i18n";
import { Sparkline } from "../../ui/Sparkline";
import { StateBadge } from "../../ui/StateBadge";
import { Availability } from "../Availability";
import { LiveError } from "../LiveError";
import { SkipLink } from "../SkipLink";
import { stateLabel, stateLevel } from "../upstream";

export default function UserPage() {
  const { t } = useI18n();
  return (
    <div class="min-h-screen">
      <SkipLink />
      <main id="main" class="mx-auto max-w-md space-y-4 px-4 py-6">
        <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
          <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
            <LiveUser />
          </Loading>
        </Errored>
      </main>
    </div>
  );
}

function LiveUser() {
  const { t, fmt } = useI18n();
  const user = createMemo(() => api.self.get({}));
  const snapshot = createLive(() => api.self.watch({}));
  const trend = createMemo<number[]>((prev = []) => [...prev, num(snapshot().total?.downRate)].slice(-60));
  const peers = createMemo(() => new Map(snapshot().peers.map((p) => [p.peerId, p])));
  const upstreams = createMemo(() => new Map(snapshot().upstreams.map((u) => [u.name, u])));

  return (
    <>
      <h1 class="text-xl font-semibold">{t.user.title(user().name)}</h1>

      <section aria-labelledby="speed" class="rounded-xl border border-line bg-surface p-4">
        <h2 id="speed" class="text-sm text-muted">
          {t.user.speedNow}
        </h2>
        <dl class="mt-2 grid grid-cols-2 gap-2">
          <div>
            <dt class="text-xs text-muted">{t.traffic.down}</dt>
            <dd class="text-2xl font-semibold tabular-nums">{fmt.rate(num(snapshot().total?.downRate))}</dd>
          </div>
          <div>
            <dt class="text-xs text-muted">{t.traffic.up}</dt>
            <dd class="text-2xl font-semibold tabular-nums">{fmt.rate(num(snapshot().total?.upRate))}</dd>
          </div>
        </dl>
        <div class="mt-3">
          <Sparkline values={trend()} label={t.user.speedTrend} size="h-10 w-full" />
        </div>
      </section>

      <section aria-labelledby="devices" class="space-y-2">
        <h2 id="devices" class="text-sm text-muted">
          {t.user.devices}
        </h2>
        <ul class="space-y-2">
          <For each={user().devices} keyed={(d) => d.name}>
            {(device) => (
              <li class="rounded-xl border border-line bg-surface p-4">
                <h3 class="font-medium">{device().name}</h3>
                <ul class="mt-2 space-y-2">
                  <For each={device().peers} keyed={(p) => p.peerId}>
                    {(peer) => {
                      const status = () => peers().get(peer().peerId);
                      return <PeerLine peer={peer()} status={status()} upstream={upstreams().get(status()?.upstream ?? "")} />;
                    }}
                  </For>
                </ul>
              </li>
            )}
          </For>
        </ul>
      </section>

      <section aria-labelledby="exits" class="space-y-2">
        <h2 id="exits" class="text-sm text-muted">
          {t.user.exits}
        </h2>
        <ul class="space-y-2">
          <For each={snapshot().upstreams} keyed={(u) => u.name}>
            {(u) => (
              <li class="space-y-2 rounded-xl border border-line bg-surface p-4">
                <div class="flex items-center justify-between gap-2">
                  <span class="font-medium">{u().name}</span>
                  <StateBadge level={stateLevel(u().state)} label={stateLabel(t, u().state)} />
                </div>
                <Availability name={u().name} label={t.user.availabilityLabel(u().name)} load={api.self.upstreamHistory} />
              </li>
            )}
          </For>
        </ul>
      </section>
    </>
  );
}

function PeerLine(props: { peer: DevicePeer; status: PeerStatus | undefined; upstream: UpstreamStatus | undefined }) {
  const { t, fmt } = useI18n();
  return (
    <li class="text-sm">
      <div class="flex items-center justify-between gap-2">
        <span>
          {t.user.profile}: <span class="font-medium">{props.peer.profile}</span>
        </span>
        <span class="tabular-nums">{fmt.bytes(num(props.status?.traffic?.downBytes))}</span>
      </div>
      <div class="mt-1 flex flex-wrap gap-x-3 text-xs text-muted">
        <Show when={props.upstream} fallback={<StateBadge level="bad" label={t.user.blocked} />}>
          {(u) => <StateBadge level={stateLevel(u().state)} label={`${t.user.upstream} ${u().name}: ${stateLabel(t, u().state)}`} />}
        </Show>
        <span>
          {t.user.lastSeen}:{" "}
          <Show when={props.status?.lastHandshake} fallback={t.user.never}>
            {(ts) => <>{fmt.ago(timestampDate(ts()), new Date())}</>}
          </Show>
        </span>
      </div>
    </li>
  );
}
