// An upstream's hourly availability over the last 7 days, shared by the
// operator page (HistoryService) and the user page (UserSelfService).

import { durationFromMs, durationMs, timestampFromMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { createMemo, Errored, Loading } from "solid-js";
import type { api } from "../api/client";
import type { HistoryResponse } from "../gen/lotsman/v1/history_pb";
import { useI18n } from "../i18n";
import { AvailabilityBar } from "../ui/AvailabilityBar";

const HOUR = 3_600_000;
const DAY = 24 * HOUR;

/** Use the daemon's step and aligned bounds. Omitted buckets have no data. */
export function availabilityHistory(response: HistoryResponse, from: number, to: number, step: number) {
  const size = response.step ? durationMs(response.step) : step;
  const first = Math.floor(from / size);
  const end = Math.ceil(Math.floor(to / 1000) * 1000 / size);
  const readings = new Map<number, number>();
  for (const sample of response.samples) {
    if (sample.time) readings.set(timestampMs(sample.time), sample.upRatio);
  }
  const ratios = Array.from({ length: end - first }, (_, i) => readings.get((first + i) * size));
  const known = ratios.filter((ratio) => ratio !== undefined);
  return {
    ratios,
    average: known.length > 0 ? known.reduce((sum, ratio) => sum + ratio, 0) / known.length : undefined,
  };
}

export interface AvailabilityProps {
  name: string;
  /** Accessible name of the bar, before the overall percentage. */
  label: string;
  load: typeof api.history.upstream;
}

export function Availability(props: AvailabilityProps) {
  const { t, fmt } = useI18n();
  const history = createMemo(() => {
    const to = Date.now();
    const from = to - 7 * DAY;
    return props.load({ name: props.name, from: timestampFromMs(from), to: timestampFromMs(to), step: durationFromMs(HOUR) })
      .then((response) => availabilityHistory(response, from, to, HOUR));
  });
  const summary = () => {
    const average = history().average;
    return average === undefined ? t.common.noData : fmt.percent(average);
  };
  return (
    <Errored fallback={<span class="text-muted">{t.common.noData}</span>}>
      <Loading fallback={<span class="text-muted">…</span>}>
        <AvailabilityBar ratios={history().ratios} label={`${props.label}: ${summary()}`} />
      </Loading>
    </Errored>
  );
}
