// An in-memory daemon for development and tests: the generated service
// descriptors served by Connect's router transport from a Simulation.
// Methods without a fake answer Unimplemented, like the daemon before their issue lands.

import { timestampMs, durationMs } from "@bufbuild/protobuf/wkt";
import { createRouterTransport, type Transport } from "@connectrpc/connect";
import {
  HistoryService,
  type StatusSnapshot,
  StatusService,
  UserSelfService,
  type WatchStatusRequest,
} from "../gen/lotsman/v1/lotsman_pb";
import { Simulation } from "./simulation";

/** The user every fake `/u/<token>` link resolves to. */
export const FAKE_USER = "alice";

export interface FakeOptions {
  seed?: number;
  clock?: () => number;
}

export function createFakeTransport(options: FakeOptions = {}): Transport {
  const sim = new Simulation(options.seed, options.clock);
  const own = (p: { user: string }) => p.user === FAKE_USER;

  return createRouterTransport(({ service }) => {
    service(StatusService, {
      watch: (req, ctx) => watch(req, ctx.signal, () => sim.snapshot()),
    });
    service(UserSelfService, {
      get: () => sim.user(FAKE_USER),
      watch: (req, ctx) => watch(req, ctx.signal, () => sim.snapshot(own)),
    });
    service(HistoryService, {
      upstream: (req) =>
        sim.history(req.name, req.from ? timestampMs(req.from) : Date.now() - 7 * 86_400_000, req.to ? timestampMs(req.to) : Date.now()),
    });
  });
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
