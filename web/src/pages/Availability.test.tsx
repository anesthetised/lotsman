import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { cleanup, render, screen } from "@solidjs/testing-library";
import { afterEach, expect, test, vi } from "vitest";
import { HistoryResponseSchema } from "../gen/lotsman/v1/history_pb";
import { Availability, availabilityHistory } from "./Availability";

const HOUR = 3_600_000;
const hourStart = Date.UTC(2026, 10, 22, 16);

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

test("shows a new exit's first reading in the newest hour of the seven-day strip", async () => {
  vi.spyOn(Date, "now").mockReturnValue(hourStart + 34 * 60_000);
  const load = async () => create(HistoryResponseSchema, {
    step: durationFromMs(HOUR),
    samples: [{ time: timestampFromMs(hourStart), upRatio: 0.75 }],
  });
  render(() => <Availability name="rs-by" label="Availability of Belarus" load={load} />);

  const strip = await screen.findByRole("img", { name: "Availability of Belarus: 75%" });
  const cells = [...strip.querySelectorAll("rect")];
  expect(cells).toHaveLength(169);
  expect(cells.slice(0, -1).every((cell) => cell.classList.contains("fill-empty"))).toBe(true);
  expect(cells.at(-1)?.getAttribute("class")).toBe("fill-bad");
});

test("shows that an exit with no readings has no data", async () => {
  vi.spyOn(Date, "now").mockReturnValue(hourStart + 34 * 60_000);
  const load = async () => create(HistoryResponseSchema, { step: durationFromMs(HOUR) });
  render(() => <Availability name="rs-de" label="Availability of Germany" load={load} />);

  const strip = await screen.findByRole("img", { name: "Availability of Germany: No data" });
  expect(strip.querySelectorAll(".fill-empty")).toHaveLength(169);
});

test("uses the returned step, preserving gaps and counting measured outages", () => {
  const result = create(HistoryResponseSchema, {
    step: durationFromMs(HOUR),
    samples: [
      { time: timestampFromMs(hourStart), upRatio: 0 },
      { time: timestampFromMs(hourStart + 2 * HOUR), upRatio: 1 },
    ],
  });

  expect(availabilityHistory(result, hourStart + 500, hourStart + 3 * HOUR + 500, 60_000)).toEqual({
    ratios: [0, undefined, 1],
    average: 0.5,
  });
});
