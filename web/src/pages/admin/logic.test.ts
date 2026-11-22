import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { expect, test } from "vitest";
import { type Connection, ConnectionSchema, Protocol } from "../../gen/lotsman/v1/connection_pb";
import { SampleSchema } from "../../gen/lotsman/v1/history_pb";
import { connectionKey, sortConnections } from "./connectionSort";
import { RightsSchema } from "../../gen/lotsman/v1/user_pb";
import { consoleEn } from "../../i18n/console.en";
import { configFileName, rightsSummary, userLink, userPath } from "./devices";
import { IncidentKind, type Incident, IncidentSchema } from "../../gen/lotsman/v1/incident_pb";
import { UpstreamStatusSchema } from "../../gen/lotsman/v1/status_pb";
import { consoleRu } from "../../i18n/console.ru";
import { historyRanges, meanOf, rateSeries, speedSeries } from "./history";
import { incidentDuration, incidentKind, incidentLevel, incidentsPath, openCount, openKey, worstLevel } from "./incidentLog";

function conn(source: string, downRate: number, upRate = 0, downBytes = 0): Connection {
  return create(ConnectionSchema, {
    peerId: "peer-1",
    protocol: Protocol.TCP,
    source,
    destination: "198.51.100.10:443",
    traffic: { downRate: BigInt(downRate), upRate: BigInt(upRate), downBytes: BigInt(downBytes) },
  });
}

test("connections sort by a traffic column, ties in a fixed order", () => {
  const list = [conn("10.8.0.2:3", 10, 5), conn("10.8.0.2:1", 30, 1), conn("10.8.0.2:2", 10, 9, 100)];
  const sources = (l: Connection[]) => l.map((c) => c.source);
  expect(sources(sortConnections(list, "downRate"))).toEqual(["10.8.0.2:1", "10.8.0.2:2", "10.8.0.2:3"]);
  expect(sources(sortConnections(list, "downRate", true))).toEqual(["10.8.0.2:2", "10.8.0.2:3", "10.8.0.2:1"]);
  expect(sources(sortConnections(list, "upRate"))).toEqual(["10.8.0.2:2", "10.8.0.2:3", "10.8.0.2:1"]);
  expect(sources(sortConnections(list, "downBytes"))[0]).toBe("10.8.0.2:2");
  expect(sources(list)).toEqual(["10.8.0.2:3", "10.8.0.2:1", "10.8.0.2:2"]); // untouched
  expect(connectionKey(list[0]!)).toBe("peer-1 1 10.8.0.2:3 198.51.100.10:443");
});

test("config file names fit an interface name", () => {
  expect(configFileName("alice", "phone")).toBe("alice-phone.conf");
  expect(configFileName("alexandra", "work-laptop")).toBe("work-laptop.conf");
  expect(configFileName("alexandra", "my work laptop")).toBe("my_work_laptop.conf");
  expect(configFileName("alexandra", "the-very-long-device")).toBe("alexandra-the-v.conf");
  expect(configFileName("Иван", "phone")).toBe("____-phone.conf");
});

test("a user's link carries the token after #; their console page escapes the name", () => {
  expect(userLink("https://vpn.example.com", "abc_-1")).toBe("https://vpn.example.com/u#abc_-1");
  expect(userPath("Иван 2")).toBe("/admin/users/%D0%98%D0%B2%D0%B0%D0%BD%202");
});

test("rights read as a few words", () => {
  const rights = (r: Parameters<typeof create<typeof RightsSchema>>[1]) => rightsSummary(consoleEn, create(RightsSchema, r));
  expect(rights({ editRoutes: true, showProviders: true })).toBe("changes routes · all exits");
  expect(rights({ editRoutes: false, showProviders: false, exits: ["lv-2", "al-1"] })).toBe("operator sets routes · 2 exits · countries only");
  expect(rightsSummary(consoleEn, undefined)).toBe("");
});

test("history samples become average rates per step", () => {
  const samples = [0, 1].map((i) => create(SampleSchema, { time: timestampFromMs(60_000 * i), downBytes: 6000n * BigInt(i + 1), upBytes: 600n }));
  expect(rateSeries(samples, 60_000)).toEqual([
    [0, 60],
    [100, 200],
    [10, 10],
  ]);
  // Every range fits the daemon's 2000 samples at its step.
  for (const r of historyRanges) expect(r.span / r.step).toBeLessThanOrEqual(2000);
});

test("speed tests become rates with gaps where none ran", () => {
  const samples = [0, 1, 2].map((i) => create(SampleSchema, { time: timestampFromMs(60_000 * i), downRate: i === 1 ? 0n : 1000n * BigInt(i + 1), upRate: i === 1 ? 0n : 100n }));
  expect(speedSeries(samples)).toEqual([
    [0, 60, 120],
    [1000, null, 3000],
    [100, null, 100],
  ]);
  expect(meanOf([1000, null, 3000])).toBe(2000);
  expect(meanOf([null, null])).toBeUndefined();
});

function incident(id: string, startedMin: number, endedMin?: number): Incident {
  return create(IncidentSchema, {
    id,
    upstream: "nl-1",
    kind: IncidentKind.UPLOAD,
    started: timestampFromMs(startedMin * 60_000),
    ended: endedMin === undefined ? undefined : timestampFromMs(endedMin * 60_000),
  });
}

test("an incident lasts until it ended, or until now while open", () => {
  const now = new Date(90 * 60_000);
  expect(incidentDuration(incident("a", 10, 20), now)).toBe(10 * 60_000);
  expect(incidentDuration(incident("b", 30), now)).toBe(60 * 60_000);
});

test("open incidents of a snapshot: the count, and a key that changes when one opens or closes", () => {
  const u = (name: string, incidents: IncidentKind[]) => create(UpstreamStatusSchema, { name, incidents });
  const before = [u("lv-1", [IncidentKind.SEND_ERRORS]), u("nl-1", [IncidentKind.UPLOAD, IncidentKind.DOWNLOAD])];
  expect(openCount(before)).toBe(3);
  expect(openKey(before)).toBe("lv-1:4 nl-1:2 nl-1:3");
  // One closes and another opens: the same count, a new key.
  const after = [u("lv-1", [IncidentKind.SEND_ERRORS]), u("nl-1", [IncidentKind.UPLOAD]), u("de-1", [IncidentKind.DOWN])];
  expect(openCount(after)).toBe(3);
  expect(openKey(after)).not.toBe(openKey(before));
  expect(openKey([])).toBe("");
});

test("incident kinds read as words, with a level", () => {
  expect([IncidentKind.DOWN, IncidentKind.DOWNLOAD, IncidentKind.UPLOAD, IncidentKind.SEND_ERRORS].map((k) => incidentKind(consoleEn, k).label)).toEqual([
    "Down",
    "Download fails",
    "Upload fails",
    "Send errors",
  ]);
  expect(incidentKind(consoleRu, IncidentKind.UPLOAD).label).toBe("Сбой отдачи");
  expect(incidentKind(consoleEn, IncidentKind.UNSPECIFIED).label).toBe("Other");
  expect(incidentKind(consoleEn, IncidentKind.SEND_ERRORS).hint).toContain("routes unchanged");
  expect(incidentLevel(IncidentKind.DOWN)).toBe("bad");
  expect(incidentLevel(IncidentKind.SEND_ERRORS)).toBe("warn");
  expect([worstLevel([]), worstLevel([IncidentKind.UPLOAD]), worstLevel([IncidentKind.UPLOAD, IncidentKind.DOWN])]).toEqual(["none", "warn", "bad"]);
  expect(consoleEn.incidents.badge(2, "Upload fails, Send errors")).toBe("Incidents: Upload fails, Send errors");
  expect(incidentsPath()).toBe("/admin/incidents");
  expect(incidentsPath("nl 1")).toBe("/admin/incidents?upstream=nl%201");
});
