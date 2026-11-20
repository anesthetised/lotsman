// Pure helpers of the incident log: kinds in words, the list's order, and
// what tells the pages that the open incidents changed.

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { type Incident, IncidentKind } from "../../gen/lotsman/v1/incident_pb";
import type { UpstreamStatus } from "../../gen/lotsman/v1/status_pb";
import type { ConsoleDict } from "../../i18n/console.en";
import type { Level } from "../../ui/AvailabilityBar";

/** The incidents page, of one upstream or (empty) of all. */
export function incidentsPath(upstream = ""): string {
  return upstream ? `/admin/incidents?upstream=${encodeURIComponent(upstream)}` : "/admin/incidents";
}

/** A kind's name and what it means, from the console's dictionary. */
export function incidentKind(c: ConsoleDict, kind: IncidentKind): { label: string; hint: string } {
  const k = c.incidents.kinds;
  switch (kind) {
    case IncidentKind.DOWN:
      return k.down;
    case IncidentKind.DOWNLOAD:
      return k.download;
    case IncidentKind.UPLOAD:
      return k.upload;
    case IncidentKind.SEND_ERRORS:
      return k.sendErrors;
    default:
      return k.unknown;
  }
}

/** How bad a kind is: down takes the upstream out entirely, the others leave it running. */
export function incidentLevel(kind: IncidentKind): Level {
  return kind === IncidentKind.DOWN ? "bad" : "warn";
}

/**
 * The worst level among open kinds, for a mark that sums them up; "none"
 * when nothing is open.
 */
export function worstLevel(kinds: readonly IncidentKind[]): Level {
  if (kinds.length === 0) return "none";
  return kinds.some((k) => incidentLevel(k) === "bad") ? "bad" : "warn";
}

/** Open incidents first, then newest; ties in a fixed order by id. The input is untouched. */
export function sortIncidents(list: readonly Incident[]): Incident[] {
  const started = (i: Incident) => (i.started ? timestampMs(i.started) : 0);
  return [...list].sort((a, b) => {
    const open = Number(b.ended === undefined) - Number(a.ended === undefined);
    if (open !== 0) return open;
    const newer = started(b) - started(a);
    return newer !== 0 ? newer : a.id.localeCompare(b.id);
  });
}

/** How many incidents are open across the upstreams of a snapshot. */
export function openCount(upstreams: readonly UpstreamStatus[]): number {
  return upstreams.reduce((n, u) => n + u.incidents.length, 0);
}

/**
 * The open incidents of a snapshot as one string, such as "lv-1:4 nl-1:3".
 * It changes when one opens or closes, even when the count stays the same,
 * so a list read from IncidentService can be fetched again just then.
 */
export function openKey(upstreams: readonly UpstreamStatus[]): string {
  return upstreams
    .flatMap((u) => u.incidents.map((k) => `${u.name}:${k}`))
    .sort()
    .join(" ");
}

/** How long an incident lasted, or has lasted so far by `now` while open. */
export function incidentDuration(incident: Incident, now: Date): number {
  const start = incident.started ? timestampMs(incident.started) : now.getTime();
  const end = incident.ended ? timestampMs(incident.ended) : now.getTime();
  return Math.max(0, end - start);
}
