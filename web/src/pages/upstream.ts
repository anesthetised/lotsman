// Presentation of upstream states, shared by the operator and user pages.

import { RoutingBlock, UpstreamKind, UpstreamState } from "../gen/lotsman/v1/status_pb";
import type { Dict } from "../i18n/en";
import type { Level } from "../ui/AvailabilityBar";

export function stateLevel(state: UpstreamState, block = RoutingBlock.UNSPECIFIED): Level {
  if (block !== RoutingBlock.UNSPECIFIED) return "warn";
  switch (state) {
    case UpstreamState.UP:
      return "ok";
    case UpstreamState.DEGRADED:
      return "warn";
    case UpstreamState.DOWN:
      return "bad";
    default:
      return "none";
  }
}

export function stateLabel(t: Dict, state: UpstreamState, block = RoutingBlock.UNSPECIFIED): string {
  switch (block) {
    case RoutingBlock.SUBSCRIPTION_EXPIRED:
      return t.state.subscriptionExpired;
    case RoutingBlock.QUOTA_EXHAUSTED:
      return t.state.quotaExhausted;
    case RoutingBlock.UNAVAILABLE:
      return t.state.temporarilyUnavailable;
  }
  switch (state) {
    case UpstreamState.UP:
      return t.state.up;
    case UpstreamState.DEGRADED:
      return t.state.degraded;
    case UpstreamState.DOWN:
      return t.state.down;
    default:
      return t.state.unknown;
  }
}

/** Protocol names are not translated. */
export function kindLabel(kind: UpstreamKind): string {
  switch (kind) {
    case UpstreamKind.AWG:
      return "AmneziaWG";
    case UpstreamKind.OUTLINE:
      return "Outline";
    case UpstreamKind.VLESS:
      return "VLESS";
    default:
      return "—";
  }
}
