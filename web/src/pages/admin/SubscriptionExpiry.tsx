import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { UpstreamStatus } from "../../gen/lotsman/v1/status_pb";
import { useI18n } from "../../i18n";

export function subscriptionExpiry(upstream: UpstreamStatus): Date | undefined {
  return upstream.expiresAt ? timestampDate(upstream.expiresAt) : undefined;
}

export function SubscriptionExpiry(props: { date: Date }) {
  const { fmt } = useI18n();
  return <time class="whitespace-nowrap" datetime={props.date.toISOString()}>{fmt.dateTime(props.date, true)}</time>;
}
