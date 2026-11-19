// An upstream's hourly availability over the last 7 days, shared by the
// operator page (HistoryService) and the user page (UserSelfService).

import { timestampFromMs, durationFromMs } from "@bufbuild/protobuf/wkt";
import { createMemo, Errored, Loading } from "solid-js";
import type { api } from "../api/client";
import { useI18n } from "../i18n";
import { AvailabilityBar } from "../ui/AvailabilityBar";

const HOUR = 3_600_000;
const DAY = 24 * HOUR;

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
    return props.load({ name: props.name, from: timestampFromMs(to - 7 * DAY), to: timestampFromMs(to), step: durationFromMs(HOUR) });
  });
  const ratios = createMemo(() => history().samples.map((s) => s.upRatio));
  const overall = () => ratios().reduce((a, b) => a + b, 0) / Math.max(ratios().length, 1);
  return (
    <Errored fallback={<span class="text-muted">{t.common.noData}</span>}>
      <Loading fallback={<span class="text-muted">…</span>}>
        <AvailabilityBar ratios={ratios()} label={`${props.label}: ${fmt.percent(overall())}`} />
      </Loading>
    </Errored>
  );
}
