// Presentation of upstream states, shared by the operator and user pages.

import { UpstreamKind, UpstreamState } from "../gen/lotsman/v1/status_pb";
import type { Dict } from "../i18n/en";
import type { Level } from "../ui/AvailabilityBar";

export function stateLevel(state: UpstreamState): Level {
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

export function stateLabel(t: Dict, state: UpstreamState): string {
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
