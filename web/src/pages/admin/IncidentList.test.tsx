// The incident log in a DOM, against the fake daemon: its order, how open
// and closed incidents read, the empty state, and the overview's mark.

import { cleanup, fireEvent, render, screen, waitFor, within } from "@solidjs/testing-library";
import { createSignal } from "solid-js";
import { afterEach, expect, test, vi } from "vitest";
import { admin } from "../../api/admin";
import { IncidentKind } from "../../gen/lotsman/v1/incident_pb";
import { IncidentBadge, IncidentLog } from "./IncidentList";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

test("lists every upstream's incidents, open first, then newest", async () => {
  render(() => <IncidentLog upstream="" openKey="" now={new Date()} showUpstream />);
  const table = await screen.findByRole("table", { name: "Incidents, open first, then newest" });
  const rows = within(table).getAllByRole("row").slice(1);
  expect(rows.map((r) => r.querySelector("td")?.textContent)).toEqual(["nl-1", "de-1", "lv-1", "lv-2", "nl-1", "de-1"]);
  expect(rows[0]!.textContent).toContain("Upload fails");
  expect(rows[0]!.textContent).toContain("Ongoing");
  expect(rows[0]!.textContent).toMatch(/2 hr 1[34] min so far/);
  expect(rows[2]!.textContent).toContain("Send errors");
  expect(rows[2]!.textContent).toContain("7 min");
  expect(rows[2]!.textContent).not.toContain("Ongoing");
  expect(within(rows[0]!).getByRole("link", { name: "nl-1" }).getAttribute("href")).toBe("/admin/upstreams/nl-1");
  expect(screen.getByText("2 open · 6 in the log")).toBeTruthy();
});

test("one upstream's incidents leave out the upstream column; none reads as such", async () => {
  render(() => <IncidentLog upstream="nl-1" openKey="" now={new Date()} />);
  const table = await screen.findByRole("table");
  expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Kind", "Started", "Duration", "Devices affected", "Detail"]);
  expect(within(table).getAllByRole("row")).toHaveLength(3);
  cleanup();

  render(() => <IncidentLog upstream="al-1" openKey="" now={new Date()} />);
  expect(await screen.findByText("No incidents for al-1.")).toBeTruthy();
  expect(screen.queryByRole("table")).toBeNull();
});

test("the mark names the open kinds and leads to them; nothing when none is open", () => {
  render(() => <IncidentBadge upstream="nl-1" kinds={[IncidentKind.UPLOAD, IncidentKind.SEND_ERRORS]} />);
  const link = screen.getByRole("link", { name: "Incidents: Upload fails, Send errors" });
  expect(link.getAttribute("href")).toBe("/admin/incidents?upstream=nl-1");
  cleanup();
  render(() => <IncidentBadge upstream="lv-1" kinds={[]} />);
  expect(screen.queryByRole("link")).toBeNull();
});

test("fetches the list again when an incident opens or closes, and on Refresh", async () => {
  const list = vi.spyOn(admin.incidents, "list");
  const [key, setKey] = createSignal("nl-1:3");
  render(() => <IncidentLog upstream="" openKey={key()} now={new Date()} showUpstream />);
  await screen.findByRole("table");
  expect(list).toHaveBeenCalledTimes(1);
  setKey("de-1:1 nl-1:3");
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() => expect(list).toHaveBeenCalledTimes(3));
  expect(list.mock.calls.every(([req]) => req.upstream === "")).toBe(true);
});
