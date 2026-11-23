// How the user page tells a failed call to the user. The daemon's errors are
// Connect codes; its two AddDevice limits share one and carry a
// SelfServiceLimit detail that says which.

import { Code, ConnectError } from "@connectrpc/connect";
import { SelfServiceLimit_Reason, SelfServiceLimitSchema } from "../../gen/lotsman/v1/user_self_pb";
import type { Dict } from "../../i18n/en";

export type Failure =
  /** The link no longer resolves to a user: it was rotated or revoked. */
  | { kind: "revoked" }
  /** The browser could not complete the request to the panel. */
  | { kind: "connection" }
  /** The panel cannot reach the daemon, or the call timed out. */
  | { kind: "unavailable" }
  /** Not allowed: `users.max_devices` is 0, or the operator sets the user's routes. */
  | { kind: "denied" }
  /** The user has `limit` devices already. */
  | { kind: "maxDevices"; limit: number }
  /** Too many additions lately; the next is allowed in `retryAfterSeconds`, when known. */
  | { kind: "rateLimited"; retryAfterSeconds?: number }
  /** A limit without the daemon's detail, such as the panel's own. */
  | { kind: "exhausted" }
  /** The user has a device of this name already. */
  | { kind: "exists" }
  | { kind: "notFound" }
  /** The daemon refused a name or a route; its message says why, in English. */
  | { kind: "invalid"; message: string }
  | { kind: "other"; message: string };

export function failureOf(error: unknown): Failure {
  const e = ConnectError.from(error);
  // Connect wraps fetch's TypeError as Unknown and preserves it as the cause.
  if (
    e.code === Code.Unknown &&
    e.cause instanceof TypeError &&
    /^(Failed to fetch|Load failed|NetworkError when attempting to fetch resource\.?)$/.test(e.rawMessage)
  ) {
    return { kind: "connection" };
  }
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
    case "connection":
      return text.connection;
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
