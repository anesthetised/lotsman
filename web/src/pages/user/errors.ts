// How the user page tells a failed call to the user. The daemon's errors are
// Connect codes; two of them share a code and differ only in their message.

import { Code, ConnectError } from "@connectrpc/connect";
import type { Dict } from "../../i18n/en";
import { ProfileChangeError } from "./profileChange";

export type Failure =
  /** The link no longer resolves to a user: it was rotated or revoked. */
  | { kind: "revoked" }
  /** The panel cannot reach the daemon, or the call timed out. */
  | { kind: "unavailable" }
  /** `users.max_devices` is 0: users may not add devices (nor switch a device's profile). */
  | { kind: "denied" }
  /** The user has `limit` devices; `limit` is unknown if the message was not the one we know. */
  | { kind: "maxDevices"; limit?: number }
  | { kind: "rateLimited" }
  /** A limit, but not one we can tell apart: a message we do not know. */
  | { kind: "exhausted" }
  /** The device already has this profile. */
  | { kind: "exists" }
  | { kind: "notFound" }
  /** The daemon refused a name or profile; its message says why, in English. */
  | { kind: "invalid"; message: string }
  | { kind: "other"; message: string };

/**
 * The daemon words its two ResourceExhausted refusals differently ("at most N
 * devices; …" and "too many devices added lately; …"); there is no other
 * way to tell them apart, so an unknown wording is a generic limit.
 */
const maxDevicesMessage = /^at most (\d+) devices?\b/;
const rateLimitedMessage = /^too many devices added lately\b/;

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
      const limit = maxDevicesMessage.exec(e.rawMessage)?.[1];
      if (limit) return { kind: "maxDevices", limit: Number(limit) };
      return rateLimitedMessage.test(e.rawMessage) ? { kind: "rateLimited" } : { kind: "exhausted" };
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
      return failure.limit === undefined ? text.exhausted : text.maxDevices(failure.limit);
    case "rateLimited":
      return text.rateLimited;
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
