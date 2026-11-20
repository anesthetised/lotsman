// The incident log (IncidentService.List) as a table, shared by the
// incidents page and an upstream's page, and the mark of an upstream's open
// incidents on the overview and its own page.

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createMemo, Errored, For, Loading, refresh, Show } from "solid-js";
import { admin } from "../../api/admin";
import type { IncidentKind } from "../../gen/lotsman/v1/incident_pb";
import { useI18n } from "../../i18n";
import { useConsoleI18n } from "../../i18n/console";
import { buttonClass } from "../../ui/button";
import { WarningIcon } from "../../ui/icons";
import { StateBadge } from "../../ui/StateBadge";
import { LiveError } from "../LiveError";
import { incidentDuration, incidentKind, incidentLevel, incidentsPath, sortIncidents, worstLevel } from "./incidentLog";

export interface IncidentLogProps {
  /** One upstream's incidents, or (empty) every upstream's. */
  upstream: string;
  /** Changes when an incident opens or closes (see openKey): the list is fetched again then. */
  openKey: string;
  /** The gateway's time, from the live snapshot: open incidents last until now. */
  now: Date;
  /** Whether to name the upstream of each row; not needed on an upstream's own page. */
  showUpstream?: boolean;
}

/** The log with its own loading and error states, so the rest of a page shows at once. */
export function IncidentLog(props: IncidentLogProps) {
  const { t } = useI18n();
  return (
    <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
      <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
        <IncidentTable {...props} />
      </Loading>
    </Errored>
  );
}

function IncidentTable(props: IncidentLogProps) {
  const { fmt, c } = useConsoleI18n();
  const list = createMemo(async () => {
    void props.openKey; // tracked: fetch again when an incident opens or closes
    const res = await admin.incidents.list({ upstream: props.upstream });
    return { incidents: sortIncidents(res.incidents), fetched: new Date() };
  });
  const rows = () => list().incidents;
  const open = () => rows().filter((i) => i.ended === undefined).length;
  return (
    <div class="space-y-2">
      <div class="flex flex-wrap items-center gap-x-4 gap-y-1 text-muted">
        <span>{c.incidents.summary(open(), rows().length)}</span>
        <span>{c.incidents.updated(fmt.time(list().fetched))}</span>
        <button type="button" class={buttonClass("compact")} onClick={() => refresh(list)}>
          {c.incidents.refresh}
        </button>
      </div>
      <Show
        when={rows().length > 0}
        fallback={
          <p class="rounded-lg border border-line bg-surface p-4 text-muted">
            {props.upstream ? c.incidents.emptyFor(props.upstream) : c.incidents.empty}
          </p>
        }
      >
        <div class="overflow-x-auto rounded-lg border border-line bg-surface">
          <table class={["w-full", props.showUpstream ? "min-w-[64rem]" : "min-w-[56rem]"]}>
            <caption class="sr-only">{c.incidents.caption}</caption>
            <thead class="text-left text-xs text-muted">
              <tr>
                <Show when={props.showUpstream}>
                  <th scope="col" class="px-3 py-2 font-medium">{c.incidents.upstream}</th>
                </Show>
                <th scope="col" class="px-3 py-2 font-medium">{c.incidents.kind}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.incidents.started}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.incidents.duration}</th>
                <th scope="col" class="px-3 py-2 text-right font-medium" title={c.incidents.devicesHint}>
                  {c.incidents.devices}
                </th>
                <th scope="col" class="px-3 py-2 font-medium">{c.incidents.detail}</th>
              </tr>
            </thead>
            <tbody>
              <For each={rows()} keyed={(i) => i.id}>
                {(i) => {
                  const kind = () => incidentKind(c, i().kind);
                  const ongoing = () => i().ended === undefined;
                  return (
                    <tr class="border-t border-line align-top">
                      <Show when={props.showUpstream}>
                        <td class="px-3 py-1.5">
                          <a
                            href={`/admin/upstreams/${encodeURIComponent(i().upstream)}`}
                            class="font-medium text-accent underline-offset-2 hover:underline"
                          >
                            {i().upstream}
                          </a>
                        </td>
                      </Show>
                      <td class="px-3 py-1.5">
                        <StateBadge level={ongoing() ? incidentLevel(i().kind) : "none"} label={kind().label} />
                        <div class="text-xs text-muted">{kind().hint}</div>
                      </td>
                      <td class="px-3 py-1.5 whitespace-nowrap">
                        <Show when={i().started} fallback="—">
                          {(s) => <>{fmt.dateTime(timestampDate(s()))}</>}
                        </Show>
                      </td>
                      <td class="px-3 py-1.5 whitespace-nowrap tabular-nums">
                        <Show when={ongoing()} fallback={fmt.duration(incidentDuration(i(), props.now))}>
                          <span class="font-medium">{c.incidents.ongoing}</span>
                          <div class="text-xs text-muted">{c.incidents.soFar(fmt.duration(incidentDuration(i(), props.now)))}</div>
                        </Show>
                      </td>
                      <td class="px-3 py-1.5 text-right tabular-nums">{fmt.count(i().devices)}</td>
                      <td class="px-3 py-1.5 font-mono text-xs wrap-anywhere">{i().detail || "—"}</td>
                    </tr>
                  );
                }}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </div>
  );
}

/** An upstream's open incidents by kind, leading to them on the incidents page; nothing when none is open. */
export function IncidentBadge(props: { upstream: string; kinds: readonly IncidentKind[] }) {
  const { c } = useConsoleI18n();
  const level = () => worstLevel(props.kinds);
  return (
    <Show when={props.kinds.length > 0}>
      <a href={incidentsPath(props.upstream)} class={[...buttonClass("compact"), "whitespace-nowrap"]}>
        <span class={level() === "bad" ? "text-bad" : "text-warn"}>
          <WarningIcon />
        </span>
        {c.incidents.badge(props.kinds.length, props.kinds.map((k) => incidentKind(c, k).label).join(", "))}
      </a>
    </Show>
  );
}
