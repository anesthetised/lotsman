// What the parts of the user page share: the client, the live snapshot and
// the one place that runs mutations.

import { createContext, useContext } from "solid-js";
import type { selfApi } from "../../api/client";
import type { PeerStatus, UpstreamStatus } from "../../gen/lotsman/v1/status_pb";
import type { User } from "../../gen/lotsman/v1/user_pb";
import type { Failure } from "./errors";

/** A config the user was just given, shown at the top of the devices until they dismiss it. */
export interface Issued {
  kind: "added" | "changed";
  device: string;
  profile: string;
  config: string;
  /** The change gave the device the new profile but could not take the old one away. */
  leftover?: string;
}

export type Outcome<T> = { ok: true; value: T } | { ok: false; failure: Failure };

export interface UserContext {
  self: ReturnType<typeof selfApi>;
  user: () => User;
  peer: (peerId: string) => PeerStatus | undefined;
  upstream: (name: string) => UpstreamStatus | undefined;
  /** The profiles the user may choose from. */
  profiles: () => string[];
  /** Configs fetched or issued during this visit, by peer: opened panels do not ask again. In memory only. */
  configs: Map<string, string>;
  issued: () => Issued | undefined;
  issue: (issued: Issued | undefined) => void;
  /** A call that changes something is under way: others wait, so the limits and the list stay in step. */
  busy: () => boolean;
  /**
   * Runs a call that changes something, then reloads the devices. A failure comes back as a
   * value; one that says the link is dead replaces the page.
   */
  mutate: <T>(task: () => Promise<T>) => Promise<Outcome<T>>;
}

const Context = createContext<UserContext>();
export const UserProvider = Context;

export function useUser(): UserContext {
  const ctx = useContext(Context);
  if (!ctx) throw new Error("useUser outside the user page");
  return ctx;
}
