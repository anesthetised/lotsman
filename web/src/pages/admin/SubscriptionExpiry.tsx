import { timestampDate } from "@bufbuild/protobuf/wkt";
import { UpstreamKind, type UpstreamStatus } from "../../gen/lotsman/v1/status_pb";
import { useI18n } from "../../i18n";
import type { OutlineQuotaValue } from "./OutlineQuota";

export function subscriptionExpiry(upstream: UpstreamStatus, quota?: OutlineQuotaValue | null): Date | undefined {
  if (upstream.kind === UpstreamKind.OUTLINE && quota?.expiresAt) {
    const providerDate = new Date(quota.expiresAt);
    if (Number.isFinite(providerDate.getTime())) return providerDate;
  }
  return upstream.expiresAt ? timestampDate(upstream.expiresAt) : undefined;
}

export function SubscriptionExpiry(props: { date: Date }) {
  const { fmt } = useI18n();
  return <time datetime={props.date.toISOString()}>{fmt.dateTime(props.date, true)}</time>;
}
