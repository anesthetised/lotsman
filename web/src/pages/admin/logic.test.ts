import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { expect, test } from "vitest";
import { type Connection, ConnectionSchema, Protocol } from "../../gen/lotsman/v1/connection_pb";
import { SampleSchema } from "../../gen/lotsman/v1/history_pb";
import { connectionKey, sortConnections } from "./connectionSort";
import { configFileName, userLink } from "./devices";
import { historyRanges, rateSeries } from "./history";
import { moveTier, nameProblem, removeTier, tierRow, toTiers, updateTier } from "./tiers";

test("tier rows move, change and go, keeping their keys", () => {
  const rows = [tierRow({ geo: "NL" }), tierRow({ tag: "eu" }), tierRow()];
  const [a, b, c] = rows.map((r) => r.key);
  expect(moveTier(rows, c!, -1).map((r) => r.key)).toEqual([a, c, b]);
  expect(moveTier(rows, a!, 1).map((r) => r.key)).toEqual([b, a, c]);
  expect(moveTier(rows, a!, -1)).toEqual(rows); // already first
  expect(moveTier(rows, c!, 1)).toEqual(rows); // already last
  expect(removeTier(rows, b!).map((r) => r.key)).toEqual([a, c]);
  const changed = updateTier(rows, b!, "provider", " acme ");
  expect(changed[1]).toEqual({ key: b, geo: "", provider: " acme ", tag: "eu" });
  expect(rows[1]?.provider).toBe(""); // the input is untouched
  expect(toTiers(changed)).toEqual([
    { geo: "NL", provider: "", tag: "" },
    { geo: "", provider: "acme", tag: "eu" },
    { geo: "", provider: "", tag: "" },
  ]);
  expect(new Set([tierRow().key, tierRow().key]).size).toBe(2);
});

test("profile names are checked before the daemon sees them", () => {
  expect(nameProblem("", [], false)).toBe("required");
  expect(nameProblem("Albania", [], false)).toBeUndefined();
  expect(nameProblem("Bad Name", [], false)).toBe("pattern");
  expect(nameProblem("-eu", [], false)).toBe("pattern");
  expect(nameProblem("a".repeat(33), [], false)).toBe("pattern");
  expect(nameProblem("eu-2", ["default"], false)).toBeUndefined();
  expect(nameProblem("default", ["default"], false)).toBe("taken"); // Put would replace it
  expect(nameProblem("default", ["default"], true)).toBeUndefined();
  expect(nameProblem("ALBANIA", ["Albania"], false)).toBe("taken"); // the store ignores case
});

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
  expect(configFileName("alice", "phone", "eu")).toBe("alice-phone-eu.conf");
  expect(configFileName("alice", "laptop", "default")).toBe("alice-laptop.conf");
  expect(configFileName("alexandra", "work-laptop", "default")).toBe("work-laptop.conf");
  expect(configFileName("alexandra", "my work laptop", "eu")).toBe("my_work_laptop.conf");
  expect(configFileName("alexandra", "the-very-long-device", "eu")).toBe("alexandra-the-v.conf");
  expect(configFileName("Иван", "phone", "eu")).toBe("____-phone-eu.conf");
});

test("a user's link carries the token after #", () => {
  expect(userLink("https://vpn.example.com", "abc_-1")).toBe("https://vpn.example.com/u#abc_-1");
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
