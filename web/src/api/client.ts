// The API clients the pages use. The transport is chosen at build time:
// the in-memory fake in dev, tests and the e2e build (VITE_FAKE_API=1), the
// real panel endpoint otherwise. In a production build the condition is a
// constant, so the fake and its simulation are dropped from the bundle.

import { createClient, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { HistoryService } from "../gen/lotsman/v1/history_pb";
import { StatusService } from "../gen/lotsman/v1/status_pb";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { createAuth, redirectToSignIn } from "./auth";
import { createFakeTransport } from "./fake";
import { createFakeAuthServer } from "./fakeAuth";
import { onUnauthenticated } from "./unauthenticated";

const fakeAuth = import.meta.env.VITE_FAKE_API ? createFakeAuthServer({ storage: sessionStorage }) : undefined;

const transport: Transport = fakeAuth
  ? createFakeTransport({ seed: 7, interceptors: [onUnauthenticated(redirectToSignIn)], signedIn: fakeAuth.signedIn })
  : createConnectTransport({ baseUrl: "/api", interceptors: [onUnauthenticated(redirectToSignIn)] });

export const api = {
  status: createClient(StatusService, transport),
  history: createClient(HistoryService, transport),
  self: createClient(UserSelfService, transport),
};

export const auth = createAuth({
  fetch: fakeAuth ? fakeAuth.fetch : (url, init) => fetch(url, init),
  credentials: "credentials" in navigator && window.PublicKeyCredential ? navigator.credentials : undefined,
  signedOut: redirectToSignIn,
});
