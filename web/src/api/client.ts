// The API clients the pages use. The transport is chosen at build time:
// the in-memory fake in dev, tests and the e2e build (VITE_FAKE_API=1), the
// real panel endpoint otherwise. In a production build the condition is a
// constant, so the fake and its simulation are dropped from the bundle.

import { createClient, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { HistoryService } from "../gen/lotsman/v1/history_pb";
import { StatusService } from "../gen/lotsman/v1/status_pb";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { createFakeTransport } from "./fake";

const transport: Transport = import.meta.env.VITE_FAKE_API
  ? createFakeTransport({ seed: 7 })
  : createConnectTransport({ baseUrl: "/api" });

export const api = {
  status: createClient(StatusService, transport),
  history: createClient(HistoryService, transport),
  self: createClient(UserSelfService, transport),
};
