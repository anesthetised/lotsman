// An in-memory daemon for development and tests: the generated service
// descriptors served by Connect's router transport from a Simulation.
// Methods without a fake answer Unimplemented, like the daemon before their issue lands.

import { create } from "@bufbuild/protobuf";
import { type Duration, durationMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createRouterTransport, type Interceptor, type Transport } from "@connectrpc/connect";
import { type ConnectionSnapshot, ConnectionService } from "../gen/lotsman/v1/connection_pb";
import { type HistoryRequest, HistoryService } from "../gen/lotsman/v1/history_pb";
import { IncidentService } from "../gen/lotsman/v1/incident_pb";
import { StatusService, TrafficSchema } from "../gen/lotsman/v1/status_pb";
import { UserService } from "../gen/lotsman/v1/user_pb";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { createSelfService, type FakeSelfOptions } from "./fakeSelf";
import { routeValue, Simulation } from "./simulation";

export { FAKE_LINKS, FAKE_USER, withToken } from "./fakeSelf";

const DAY = 86_400_000;

export interface FakeOptions {
  seed?: number;
  clock?: () => number;
  interceptors?: Interceptor[];
  /** Whether an operator is signed in (default: always); operator calls answer Unauthenticated otherwise, as the panel does. */
  signedIn?: () => boolean;
  /** The user page's limits (see FakeSelfOptions). */
  self?: FakeSelfOptions;
}

export function createFakeTransport(options: FakeOptions = {}): Transport {
  const sim = new Simulation(options.seed, options.clock);
  const clock = options.clock ?? Date.now;
  const history = (kind: "upstream" | "peer", req: HistoryRequest, allowed = true) => {
    const to = req.to ? timestampMs(req.to) : clock();
    const from = req.from ? timestampMs(req.from) : to - 7 * DAY;
    const res = allowed ? sim.history(kind, req.name, from, to, req.step ? durationMs(req.step) : 0) : undefined;
    if (!res) throw new ConnectError(`no ${kind} ${req.name}`, Code.NotFound);
    return res;
  };

  const operator = () => {
    if (options.signedIn && !options.signedIn()) throw new ConnectError("not signed in", Code.Unauthenticated);
  };

  return createRouterTransport(({ service }) => {
    service(StatusService, {
      watch: (req, ctx) => {
        operator();
        return watch(req.interval, ctx.signal, () => sim.snapshot());
      },
    });
    service(ConnectionService, {
      watch: (req, ctx) => {
        operator();
        // Like the daemon, each stream meters on its own: its first snapshot has bytes but no rates.
        let first = true;
        const ids = req.peerId ? [...req.peerIds, req.peerId] : req.peerIds;
        return watch(req.interval, ctx.signal, () => {
          const s = sim.connections(ids);
          if (first) withoutRates(s);
          first = false;
          return s;
        });
      },
    });
    service(UserService, {
      list: (req) => {
        operator();
        if (req.limit > 200 || (req.limit === 0 && req.offset !== 0)) {
          throw new ConnectError("pagination requires a limit from 1 to 200", Code.InvalidArgument);
        }
        const users = sim.listUsers().filter((u) => !req.user || u.name === req.user);
        if (!req.limit) return { users };
        users.sort((a, b) => a.name === b.name ? 0 : a.name < b.name ? -1 : 1);
        const end = req.offset + req.limit;
        return { users: users.slice(req.offset, end), hasMore: end < users.length };
      },
      addUser: (req) => (operator(), sim.addUser(req.user)),
      setRights: (req) => {
        operator();
        const r = req.rights;
        return sim.setRights(req.user, { editRoutes: r?.editRoutes ?? false, showProviders: r?.showProviders ?? false, exits: [...(r?.exits ?? [])] });
      },
      addDevice: (req) => (operator(), sim.addDevice(req.user, req.device, routeValue(req.route))),
      setRoute: (req) => (operator(), sim.setRoute(req.user, req.device, routeValue(req.route))),
      removeDevice: (req) => (operator(), sim.removeDevice(req.user, req.device), {}),
      removeUser: (req) => (operator(), sim.removeUser(req.user), {}),
      getConfig: (req) => (operator(), { peerId: req.peerId, config: sim.config(req.peerId) }),
      issueLink: (req) => (operator(), { token: sim.issueLink(req.user) }),
      revokeLink: (req) => (operator(), sim.revokeLink(req.user), {}),
    });
    service(UserSelfService, createSelfService(sim, clock, watch, history, options.self));
    service(HistoryService, {
      upstream: (req) => (operator(), history("upstream", req)),
      peer: (req) => (operator(), history("peer", req)),
    });
    service(IncidentService, {
      list: (req) => {
        operator();
        const limit = req.limit || 200;
        const rows = sim.listIncidents(req.upstream, limit + 1, req.offset);
        return { incidents: rows.slice(0, limit), hasMore: rows.length > limit };
      },
    });
  }, { transport: { interceptors: options.interceptors } });
}

/** A connections snapshot as the first of a stream has it: no rates yet. */
function withoutRates(s: ConnectionSnapshot): void {
  for (const c of s.connections) c.traffic = create(TrafficSchema, { downBytes: c.traffic?.downBytes, upBytes: c.traffic?.upBytes });
}

/** A snapshot now and then every interval (at least 1 s, as the daemon does), until aborted. */
async function* watch<T>(every: Duration | undefined, signal: AbortSignal, snapshot: () => T) {
  const interval = Math.max(1000, every ? durationMs(every) : 1000);
  while (!signal.aborted) {
    yield snapshot();
    await sleep(interval, signal);
  }
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done);
  });
}
