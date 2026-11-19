// Operator sign-in: the panel's own JSON endpoints under /auth (not the
// daemon's API) and the browser's passkey prompts. Every call is a POST
// with a JSON body: the panel refuses anything else, which with its Origin
// check and SameSite=Strict cookies keeps other sites out.

import { creationOptions, credentialJSON, type CreationOptionsJSON, type CredentialLike, requestOptions, type RequestOptionsJSON } from "./webauthn";

export interface Session {
  /** The name of the passkey the operator signed in with. */
  name: string;
}

export interface Passkey {
  id: number;
  name: string;
  createdAt: string;
  usedAt?: string;
  /** The passkey this session signed in with. */
  current: boolean;
}

export interface EnrollmentLink {
  url: string;
  expiresAt: string;
}

/** A refused auth call: the panel's HTTP status and message. */
export class AuthError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "AuthError";
  }
}

export type FetchLike = (url: string, init: RequestInit) => Promise<Response>;

/** What a sign-in page shows for a failure. */
export type Failure = "cancelled" | "unsupported" | "rejected" | "linkUnknown" | "linkUsed" | "tooMany" | "failed";

export function failure(error: unknown): Failure {
  if (error instanceof AuthError) {
    switch (error.status) {
      case 401:
        return "rejected";
      case 404:
        return "linkUnknown";
      case 410:
        return "linkUsed";
      case 429:
        return "tooMany";
    }
    return "failed";
  }
  // The browser's own: the prompt was dismissed, timed out, or no passkey matched.
  if (error instanceof DOMException && (error.name === "NotAllowedError" || error.name === "AbortError")) return "cancelled";
  if (error instanceof DOMException && error.name === "NotSupportedError") return "unsupported";
  return "failed";
}

/** Only pages of the console are worth returning to after sign-in. */
export function returnPath(next: string | null | undefined): string {
  return next && /^\/admin(\/|$)/.test(next) ? next : "/admin";
}

export interface AuthDeps {
  fetch: FetchLike;
  /** navigator.credentials, if the browser has WebAuthn. */
  credentials?: Pick<CredentialsContainer, "create" | "get">;
  /** Called when an operator-only call finds no session. */
  signedOut?: () => void;
}

export function createAuth(deps: AuthDeps) {
  async function post<T>(path: string, body: unknown = {}): Promise<T> {
    const res = await deps.fetch(`/auth/${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      credentials: "same-origin",
    });
    if (res.status === 204) return undefined as T;
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new AuthError(res.status, typeof data.error === "string" ? data.error : res.statusText);
    return data as T;
  }

  function operator<T>(call: Promise<T>): Promise<T> {
    return call.catch((e) => {
      if (e instanceof AuthError && e.status === 401) deps.signedOut?.();
      throw e;
    });
  }

  function credentials() {
    if (!deps.credentials) throw new DOMException("this browser has no passkeys", "NotSupportedError");
    return deps.credentials;
  }

  return {
    /** The current session, or null when signed out. */
    async session(): Promise<Session | null> {
      const s = await post<{ signedIn: boolean; name?: string }>("session");
      return s.signedIn ? { name: s.name ?? "" } : null;
    },

    async signIn(): Promise<Session> {
      const options = await post<RequestOptionsJSON>("login/begin");
      const credential = await credentials().get(requestOptions(options));
      return post<Session>("login/finish", credentialJSON(credential as unknown as CredentialLike));
    },

    async enroll(token: string, name: string): Promise<Session> {
      const options = await post<CreationOptionsJSON>("enroll/begin", { token, name });
      const credential = await credentials().create(creationOptions(options));
      return post<Session>("enroll/finish", credentialJSON(credential as unknown as CredentialLike));
    },

    signOut: () => post<void>("logout"),
    passkeys: () => operator(post<{ passkeys: Passkey[] }>("passkeys")).then((r) => r.passkeys),
    revoke: (id: number) => operator(post<{ signedOut: boolean }>("passkeys/revoke", { id })),
    issueEnrollment: () => operator(post<EnrollmentLink>("enrollments")),
  };
}

export type Auth = ReturnType<typeof createAuth>;

/** Leaves for the sign-in page, coming back here afterwards. */
export function redirectToSignIn() {
  const next = location.pathname + location.search;
  location.assign(`/login?next=${encodeURIComponent(next)}`);
}
