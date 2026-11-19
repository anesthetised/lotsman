import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { expect, test } from "vitest";
import { type Connection, ConnectionSchema, Protocol } from "../../gen/lotsman/v1/connection_pb";
import { SampleSchema } from "../../gen/lotsman/v1/history_pb";
import { connectionKey, sortConnections } from "./connectionSort";
import { RightsSchema } from "../../gen/lotsman/v1/user_pb";
import { consoleEn } from "../../i18n/console.en";
import { configFileName, rightsSummary, userLink, userPath } from "./devices";
import { historyRanges, rateSeries } from "./history";

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
