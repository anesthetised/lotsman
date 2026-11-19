// How the user page tells a failed call to the user. The daemon's errors are
// Connect codes; its two AddDevice limits share one and carry a
// SelfServiceLimit detail that says which.

import { Code, ConnectError } from "@connectrpc/connect";
import { SelfServiceLimit_Reason, SelfServiceLimitSchema } from "../../gen/lotsman/v1/user_self_pb";
import type { Dict } from "../../i18n/en";
import { ProfileChangeError } from "./profileChange";

export type Failure =
  /** The link no longer resolves to a user: it was rotated or revoked. */
  | { kind: "revoked" }
  /** The panel cannot reach the daemon, or the call timed out. */
  | { kind: "unavailable" }
  /** `users.max_devices` is 0: users may not add devices (nor switch a device's profile). */
  | { kind: "denied" }
  /** The user has `limit` devices already. */
  | { kind: "maxDevices"; limit: number }
  /** Too many additions lately; the next is allowed in `retryAfterSeconds`, when known. */
  | { kind: "rateLimited"; retryAfterSeconds?: number }
  /** A limit without the daemon's detail, such as the panel's own. */
  | { kind: "exhausted" }
  /** The device already has this profile. */
  | { kind: "exists" }
  | { kind: "notFound" }
  /** The daemon refused a name or profile; its message says why, in English. */
  | { kind: "invalid"; message: string }
  | { kind: "other"; message: string };

/** A profile change fails in one of its two calls; what matters is why that one failed. */
export function failureOf(error: unknown): Failure {
  if (error instanceof ProfileChangeError) return failureOf(error.cause);
  const e = ConnectError.from(error);
  switch (e.code) {
    case Code.Unauthenticated:
      return { kind: "revoked" };
    case Code.Unavailable:
    case Code.DeadlineExceeded:
    case Code.Aborted:
      return { kind: "unavailable" };
    case Code.PermissionDenied:
      return { kind: "denied" };
    case Code.ResourceExhausted: {
      const [limit] = e.findDetails(SelfServiceLimitSchema);
      switch (limit?.reason) {
        case SelfServiceLimit_Reason.MAX_DEVICES:
          return { kind: "maxDevices", limit: limit.maxDevices };
        case SelfServiceLimit_Reason.RATE_LIMITED: {
          const seconds = limit.retryAfter ? Number(limit.retryAfter.seconds) : undefined;
          return seconds === undefined ? { kind: "rateLimited" } : { kind: "rateLimited", retryAfterSeconds: seconds };
        }
      }
      return { kind: "exhausted" };
    }
    case Code.AlreadyExists:
      return { kind: "exists" };
    case Code.NotFound:
      return { kind: "notFound" };
    case Code.InvalidArgument:
      return { kind: "invalid", message: e.rawMessage };
    default:
      return { kind: "other", message: e.rawMessage };
  }
}

/** The failure in the page's language. */
export function failureText(t: Dict, failure: Failure): string {
  const text = t.user.failure;
  switch (failure.kind) {
    case "revoked":
      return t.user.revoked;
    case "unavailable":
      return text.unavailable;
    case "denied":
      return text.denied;
    case "maxDevices":
      return text.maxDevices(failure.limit);
    case "rateLimited":
      return failure.retryAfterSeconds ? text.rateLimitedFor(Math.ceil(failure.retryAfterSeconds / 60)) : text.rateLimited;
    case "exhausted":
      return text.exhausted;
    case "exists":
      return text.exists;
    case "notFound":
      return text.notFound;
    case "invalid":
      return text.invalid(failure.message);
    case "other":
      return text.other(failure.message);
  }
}
