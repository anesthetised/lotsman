// An in-memory stand-in for the panel's /auth endpoints, for development
// and tests. It speaks the same JSON and runs real passkey prompts, so the
// pages and the WebAuthn plumbing are exercised as against the panel, but
// it checks only the client data (type, challenge, origin), not signatures:
// the Go tests cover verification.
//
// It starts signed in as "dev", so `pnpm dev` opens the console at once,
// and accepts the enrollment token "dev" once. State lives in the given
// Storage (sessionStorage in the browser), so it survives reloads.

import type { FetchLike } from "./auth";
import { fromBase64url, toBase64url } from "./webauthn";

export const FAKE_ENROLL_TOKEN = "dev";

interface FakePasskey {
  id: number;
  credentialId: string;
  name: string;
  createdAt: string;
  usedAt?: string;
}

interface State {
  passkeys: FakePasskey[];
  signedIn: number | null;
  tokens: string[];
  used: string[];
  nextId: number;
}

interface Ceremony {
  kind: "webauthn.get" | "webauthn.create";
  token?: string;
  name?: string;
}

export interface FakeAuthOptions {
  storage?: Pick<Storage, "getItem" | "setItem">;
  origin?: string;
  clock?: () => number;
}

const KEY = "lotsman-fake-auth";

function initial(now: string): State {
  return { passkeys: [{ id: 1, credentialId: "", name: "dev", createdAt: now }], signedIn: 1, tokens: [FAKE_ENROLL_TOKEN], used: [], nextId: 2 };
}

const random = () => toBase64url(crypto.getRandomValues(new Uint8Array(32)));

export function createFakeAuthServer(options: FakeAuthOptions = {}) {
  const clock = options.clock ?? Date.now;
  const origin = options.origin ?? location.origin;
  const rpId = new URL(origin).hostname;
  const now = () => new Date(clock()).toISOString();
  let memory: State | null = null;
  const load = (): State => {
    const stored = options.storage?.getItem(KEY);
    memory = stored ? (JSON.parse(stored) as State) : (memory ?? initial(now()));
    return memory;
  };
  const save = (s: State) => {
    memory = s;
    options.storage?.setItem(KEY, JSON.stringify(s));
  };
  const ceremonies = new Map<string, Ceremony>();

  class Refusal {
    constructor(
      readonly status: number,
      readonly error: string,
    ) {}
  }

  /** The ceremony a credential answers, checked as the panel checks it. */
  function finish(body: { response?: { clientDataJSON?: string } }, kind: Ceremony["kind"]): Ceremony {
    let data: { type?: string; challenge?: string; origin?: string };
    try {
      data = JSON.parse(new TextDecoder().decode(fromBase64url(body.response?.clientDataJSON ?? "")));
    } catch {
      throw new Refusal(400, "malformed credential");
    }
    const c = ceremonies.get(data.challenge ?? "");
    ceremonies.delete(data.challenge ?? "");
    if (!c || c.kind !== kind) throw new Refusal(400, "unknown or expired ceremony; start again");
    if (data.type !== kind || data.origin !== origin) throw new Refusal(401, "the passkey was not accepted");
    return c;
  }

  function signedIn(s: State): FakePasskey {
    const p = s.passkeys.find((p) => p.id === s.signedIn);
    if (!p) throw new Refusal(401, "not signed in");
    return p;
  }

  function route(path: string, body: Record<string, unknown>): unknown {
    const s = load();
    switch (path) {
      case "session": {
        const p = s.passkeys.find((p) => p.id === s.signedIn);
        return p ? { signedIn: true, name: p.name } : { signedIn: false };
      }
      case "logout":
        save({ ...s, signedIn: null });
        return undefined;
      case "login/begin": {
        const challenge = random();
        ceremonies.set(challenge, { kind: "webauthn.get" });
        return { publicKey: { challenge, rpId, userVerification: "required", timeout: 300_000 } };
      }
      case "login/finish": {
        finish(body, "webauthn.get");
        const p = s.passkeys.find((p) => p.credentialId === body.rawId);
        if (!p) throw new Refusal(401, "the passkey was not accepted");
        save({ ...s, signedIn: p.id, passkeys: s.passkeys.map((q) => (q === p ? { ...q, usedAt: now() } : q)) });
        return { name: p.name };
      }
      case "enroll/begin": {
        const name = String(body.name ?? "").trim();
        if (!name || name.length > 64) throw new Refusal(400, "name the passkey");
        if (s.used.includes(String(body.token))) throw new Refusal(410, "this enrollment link has been used");
        if (!s.tokens.includes(String(body.token))) throw new Refusal(404, "unknown enrollment link");
        const challenge = random();
        ceremonies.set(challenge, { kind: "webauthn.create", token: String(body.token), name });
        return {
          publicKey: {
            rp: { id: rpId, name: "Lotsman" },
            user: { id: random(), name, displayName: name },
            challenge,
            pubKeyCredParams: [
              { type: "public-key", alg: -7 },
              { type: "public-key", alg: -257 },
            ],
            authenticatorSelection: { residentKey: "required", requireResidentKey: true, userVerification: "required" },
            attestation: "none",
            timeout: 300_000,
          },
        };
      }
      case "enroll/finish": {
        const c = finish(body, "webauthn.create");
        if (!s.tokens.includes(c.token!)) throw new Refusal(410, "this enrollment link has been used or has expired");
        const p: FakePasskey = { id: s.nextId, credentialId: String(body.rawId), name: c.name!, createdAt: now() };
        save({
          passkeys: [...s.passkeys, p],
          signedIn: p.id,
          tokens: s.tokens.filter((t) => t !== c.token),
          used: [...s.used, c.token!],
          nextId: s.nextId + 1,
        });
        return { name: p.name };
      }
      case "passkeys": {
        const me = signedIn(s);
        return { passkeys: s.passkeys.map(({ credentialId: _, ...p }) => ({ ...p, current: p.id === me.id })) };
      }
      case "passkeys/revoke": {
        const me = signedIn(s);
        if (!s.passkeys.some((p) => p.id === body.id)) throw new Refusal(404, "no such passkey");
        const self = body.id === me.id;
        save({ ...s, passkeys: s.passkeys.filter((p) => p.id !== body.id), signedIn: self ? null : s.signedIn });
        return { signedOut: self };
      }
      case "enrollments": {
        signedIn(s);
        const token = random();
        save({ ...s, tokens: [...s.tokens, token] });
        return { url: `${origin}/enroll#${token}`, expiresAt: new Date(clock() + 15 * 60_000).toISOString() };
      }
    }
    throw new Refusal(404, "404 page not found");
  }

  const fetch: FetchLike = async (url, init) => {
    const path = url.replace(/^\/auth\//, "");
    try {
      const data = route(path, JSON.parse(String(init.body ?? "{}")));
      return data === undefined ? new Response(null, { status: 204 }) : Response.json(data);
    } catch (e) {
      if (e instanceof Refusal) return Response.json({ error: e.error }, { status: e.status });
      throw e;
    }
  };

  return {
    fetch,
    /** Whether an operator is signed in: the fake daemon refuses operator calls otherwise. */
    signedIn: () => load().signedIn !== null,
  };
}
