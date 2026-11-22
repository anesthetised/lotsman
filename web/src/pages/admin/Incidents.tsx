// The incident log (IncidentService.List): trouble the gateway's own checks
// found with its upstreams, open first, then newest, of all upstreams or of
// one (`?upstream=`). The live status tells when an incident opens or closes,
// and the list is fetched again then.

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useSearchParams } from "@solidjs/router";
import { createMemo, Errored, For, Loading } from "solid-js";
import { api } from "../../api/client";
import { createLive } from "../../api/live";
import { useI18n } from "../../i18n";
import { useConsoleI18n } from "../../i18n/console";
import { LiveError } from "../LiveError";
import { IncidentLog } from "./IncidentList";
import { openKey } from "./incidentLog";

export default function Incidents() {
  const { t } = useI18n();
  return (
    <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
      <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
        <LiveIncidents />
      </Loading>
    </Errored>
  );
}

function LiveIncidents() {
  const { c } = useConsoleI18n();
  const [params, setParams] = useSearchParams<{ upstream: string }>();
  const upstream = () => params.upstream ?? "";
  const status = createLive((signal) => api.status.watch({}, { signal }));
  const key = createMemo(() => openKey(status().upstreams.filter((u) => !upstream() || u.name === upstream())));
  const now = () => (status().time ? timestampDate(status().time!) : new Date());
  // The gateway's upstreams, and the one asked for even when it has gone
  // from the config: its incidents stay in the log.
  const names = createMemo(() => {
    const list = status().upstreams.map((u) => u.name);
    return upstream() === "" || list.includes(upstream()) ? list : [...list, upstream()];
  });
  return (
    <div class="space-y-4">
      <div class="flex flex-wrap items-center justify-between gap-4">
        <h1 class="text-lg font-semibold">{c.incidents.title}</h1>
        <div class="flex items-center gap-2">
          <label for="incidents-upstream" class="text-muted">
            {c.incidents.upstream}
          </label>
          <select
            id="incidents-upstream"
            class="rounded-md border border-line bg-surface px-2 py-1.5"
            onChange={(e) => setParams({ upstream: e.currentTarget.value || undefined })}
          >
            <option value="" selected={upstream() === ""}>
              {c.incidents.allUpstreams}
            </option>
            <For each={names()}>
              {(name) => (
                <option value={name} selected={name === upstream()}>
                  {name}
                </option>
              )}
            </For>
          </select>
        </div>
      </div>
      <p class="max-w-3xl text-muted">{c.incidents.intro}</p>
      <IncidentLog upstream={upstream()} openKey={key()} now={now()} showUpstream />
    </div>
  );
}
