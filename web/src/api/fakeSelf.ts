// The fake daemon's UserSelfService (#137): what a user's page can do against
// the daemon, with the same limits and errors, deterministic for tests.
// A link's token picks the scenario (see FAKE_LINKS); any other token is the
// ordinary user. Calls without a token (unit tests) take the ordinary one too.

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, type HandlerContext, type ServiceImpl, type Transport } from "@connectrpc/connect";
import { type HistoryRequest, type HistoryResponse, HistoryResponseSchema, SampleSchema } from "../gen/lotsman/v1/history_pb";
import { ListProfilesResponseSchema, ProfileSchema } from "../gen/lotsman/v1/profile_pb";
import type { StatusSnapshot, WatchStatusRequest } from "../gen/lotsman/v1/status_pb";
import { DeviceConfigSchema } from "../gen/lotsman/v1/user_pb";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import type { Simulation } from "./simulation";

/** The user every ordinary fake `/u#<token>` link resolves to. */
export const FAKE_USER = "alice";

const token = (name: string) => `fake-${name}`.padEnd(43, "_");

/** Links with a scenario of their own; the fake takes any other token as the ordinary user. */
export const FAKE_LINKS = {
  ordinary: token("ordinary"),
  /** A link that was revoked or rotated: every call is Unauthenticated. */
  revoked: token("revoked"),
  /** The operator turned self-service off (`max_devices: 0`). */
  noAdding: token("no-adding"),
  /** A user without devices. */
  empty: token("empty"),
  /** The panel cannot reach the daemon. */
  unavailable: token("unavailable"),
} as const;

const NAME_LIMIT = 64; // bytes

export interface FakeSelfOptions {
  /** `users.max_devices` (default 5). */
  maxDevices?: number;
  /** Additions allowed at once before the rate limit (default 10); then one every 6 minutes. */
  addBurst?: number;
}

/** The same transport, with `token` sent as `Authorization: Bearer` the way `withLink` does it for the real panel. */
export function withToken(base: Transport, token: string): Transport {
  const header = (h?: HeadersInit) => {
    const headers = new Headers(h);
    headers.set("Authorization", `Bearer ${token}`);
    return headers;
  };
  return {
    unary: (method, signal, timeout, h, input, values) => base.unary(method, signal, timeout, header(h), input, values),
    stream: (method, signal, timeout, h, input, values) => base.stream(method, signal, timeout, header(h), input, values),
  };
}

interface Acting {
  user: string;
  maxDevices: number;
}

/** Every RPC but ResolveLink, which only the panel calls. */
export function createSelfService(
  sim: Simulation,
  clock: () => number,
  watch: (req: WatchStatusRequest, signal: AbortSignal, snapshot: () => StatusSnapshot) => AsyncIterable<StatusSnapshot>,
  history: (kind: "upstream" | "peer", req: HistoryRequest, allowed: boolean) => HistoryResponse,
  options: FakeSelfOptions = {},
): Partial<ServiceImpl<typeof UserSelfService>> {
  const burst = options.addBurst ?? 10;
  const additions = new Map<string, { tokens: number; at: number }>();
  /** A token bucket per user: `burst` at once, then one every 6 minutes. */
  const allowAddition = (user: string) => {
    const now = clock();
    const bucket = additions.get(user) ?? { tokens: burst, at: now };
    const tokens = Math.min(burst, bucket.tokens + (now - bucket.at) / 360_000);
    if (tokens < 1) {
      additions.set(user, { tokens, at: now });
      return false;
    }
    additions.set(user, { tokens: tokens - 1, at: now });
    return true;
  };

  const acting = (ctx: HandlerContext): Acting => {
    const link = ctx.requestHeader.get("Authorization")?.replace(/^Bearer /, "");
    if (link === FAKE_LINKS.revoked) throw new ConnectError("unknown link", Code.Unauthenticated);
    if (link === FAKE_LINKS.unavailable) throw new ConnectError("the daemon is not reachable", Code.Unavailable);
    return {
      user: link === FAKE_LINKS.empty ? "dave" : FAKE_USER,
      maxDevices: link === FAKE_LINKS.noAdding ? 0 : (options.maxDevices ?? 5),
    };
  };

  /** The daemon's checks on a device or profile name. */
  const checkName = (what: string, value: string) => {
    if (value === "") throw new ConnectError(`${what} is required`, Code.InvalidArgument);
    if (new TextEncoder().encode(value).length > NAME_LIMIT || value !== value.trim()) {
      throw new ConnectError(`${what} must be printable UTF-8 of at most ${NAME_LIMIT} bytes, without spaces around it`, Code.InvalidArgument);
    }
  };

  return {
    get: (_, ctx) => sim.user(acting(ctx).user),
    watch: (req, ctx) => watch(req, ctx.signal, () => sim.snapshot(acting(ctx).user)),

    addDevice: (req, ctx) => {
      const { user, maxDevices } = acting(ctx);
      checkName("device", req.device);
      checkName("profile", req.profile);
      if (sim.hasPeer(user, req.device, req.profile)) {
        throw new ConnectError(`${user}/${req.device} already has profile ${req.profile}`, Code.AlreadyExists);
      }
      if (maxDevices === 0) throw new ConnectError("adding devices is turned off; ask the operator", Code.PermissionDenied);
      const devices = sim.devicesOf(user);
      if (!devices.includes(req.device) && devices.length >= maxDevices) {
        throw new ConnectError(`at most ${maxDevices} devices; remove one first or ask the operator`, Code.ResourceExhausted);
      }
      if (!allowAddition(user)) {
        throw new ConnectError("too many devices added lately; try again in a few minutes", Code.ResourceExhausted);
      }
      if (!sim.profileNames().includes(req.profile)) throw new ConnectError(`no such profile: ${req.profile}`, Code.InvalidArgument);
      const peer = sim.addPeer(user, req.device, req.profile);
      return create(DeviceConfigSchema, { peerId: peer.peerId, config: sim.config(peer.peerId) });
    },

    removeDevice: (req, ctx) => {
      const { user } = acting(ctx);
      if (req.device === "") throw new ConnectError("device is required", Code.InvalidArgument);
      if (sim.removePeers(user, req.device, req.profile) === 0) throw new ConnectError("no such device or profile", Code.NotFound);
      return {};
    },

    getConfig: (req, ctx) => {
      const { user } = acting(ctx);
      // Another user's peer and a missing one answer alike.
      const config = sim.ownerOf(req.peerId) === user ? sim.config(req.peerId) : undefined;
      if (config === undefined) throw new ConnectError("no such peer", Code.NotFound);
      return create(DeviceConfigSchema, { peerId: req.peerId, config });
    },

    listProfiles: (_, ctx) => {
      acting(ctx);
      return create(ListProfilesResponseSchema, { profiles: sim.profileNames().map((name) => create(ProfileSchema, { name })) });
    },

    upstreamHistory: (req, ctx) => {
      const { user } = acting(ctx);
      return withoutBytes(history("upstream", req, sim.upstreamsOf(user).has(req.name)));
    },
    peerHistory: (req, ctx) => {
      const { user } = acting(ctx);
      return history("peer", req, sim.ownerOf(req.name) === user);
    },
  };
}

/** A user sees an upstream's availability and latency, not the traffic of everyone routed through it. */
function withoutBytes(res: HistoryResponse): HistoryResponse {
  return create(HistoryResponseSchema, {
    step: res.step,
    samples: res.samples.map((s) => create(SampleSchema, { time: s.time, upRatio: s.upRatio, latency: s.latency })),
  });
}
