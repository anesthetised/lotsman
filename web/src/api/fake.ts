// An in-memory daemon for development and tests: the generated service
// descriptors served by Connect's router transport from a Simulation.
// Methods without a fake answer Unimplemented, like the daemon before their issue lands.

import { durationMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createRouterTransport, type Interceptor, type Transport } from "@connectrpc/connect";
import { type HistoryRequest, HistoryService } from "../gen/lotsman/v1/history_pb";
import { type StatusSnapshot, StatusService, type WatchStatusRequest } from "../gen/lotsman/v1/status_pb";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { createSelfService, type FakeSelfOptions } from "./fakeSelf";
import { Simulation } from "./simulation";

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
        return watch(req, ctx.signal, () => sim.snapshot());
      },
    });
    service(UserSelfService, createSelfService(sim, clock, watch, history, options.self));
    service(HistoryService, {
      upstream: (req) => (operator(), history("upstream", req)),
      peer: (req) => (operator(), history("peer", req)),
    });
  }, { transport: { interceptors: options.interceptors } });
}

/** A snapshot now and then every interval (at least 1 s, as the daemon does), until aborted. */
async function* watch(req: WatchStatusRequest, signal: AbortSignal, snapshot: () => StatusSnapshot) {
  const interval = Math.max(1000, req.interval ? durationMs(req.interval) : 1000);
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
