import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, test, vi } from "vitest";
import { HistoryService } from "../gen/lotsman/v1/history_pb";
import { StatusService } from "../gen/lotsman/v1/status_pb";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { AuthError, createAuth, failure, returnPath } from "./auth";
import { createFakeTransport } from "./fake";
import { createFakeAuthServer, FAKE_ENROLL_TOKEN } from "./fakeAuth";
import { onUnauthenticated } from "./unauthenticated";
import { creationOptions, credentialJSON, type CredentialLike, fromBase64url, requestOptions, toBase64url } from "./webauthn";

const ORIGIN = "https://vpn.example.com";

describe("webauthn JSON", () => {
  test("base64url round-trips any bytes, without padding", () => {
    const bytes = Uint8Array.from({ length: 256 }, (_, i) => i);
    const text = toBase64url(bytes);
    expect(text).not.toMatch(/[+/=]/);
    expect(fromBase64url(text)).toEqual(bytes);
    expect(toBase64url(new Uint8Array([0xfb, 0xff]))).toBe("-_8");
  });

  test("options decode their binary fields", () => {
    const creation = creationOptions({
      publicKey: {
        rp: { id: "vpn.example.com", name: "Lotsman" },
        user: { id: "AQID", name: "laptop", displayName: "laptop" },
        challenge: "BAUG",
        pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        excludeCredentials: [{ type: "public-key", id: "Bw" }],
      },
    }).publicKey!;
    expect(creation.challenge).toEqual(new Uint8Array([4, 5, 6]));
    expect(creation.user.id).toEqual(new Uint8Array([1, 2, 3]));
    expect(creation.excludeCredentials?.[0]?.id).toEqual(new Uint8Array([7]));
    expect(creation.rp).toEqual({ id: "vpn.example.com", name: "Lotsman" });

    const request = requestOptions({ publicKey: { challenge: "BAUG", rpId: "vpn.example.com", userVerification: "required" } }).publicKey!;
    expect(request.challenge).toEqual(new Uint8Array([4, 5, 6]));
    expect(request.allowCredentials).toBeUndefined();
    expect(request.userVerification).toBe("required");
  });

  test("credentials encode as the panel reads them", () => {
    const buf = (...b: number[]) => new Uint8Array(b).buffer;
    const base = { id: "Bw", rawId: buf(7), type: "public-key", authenticatorAttachment: "platform", getClientExtensionResults: () => ({}) };
    const created = credentialJSON({
      ...base,
      response: { clientDataJSON: buf(1), attestationObject: buf(2), getTransports: () => ["internal"] } as unknown as AuthenticatorResponse,
    });
    expect(created).toEqual({
      id: "Bw",
      rawId: "Bw",
      type: "public-key",
      authenticatorAttachment: "platform",
      response: { clientDataJSON: "AQ", attestationObject: "Ag", transports: ["internal"] },
      clientExtensionResults: {},
    });
    const asserted = credentialJSON({
      ...base,
      authenticatorAttachment: null,
      response: { clientDataJSON: buf(1), authenticatorData: buf(3), signature: buf(4), userHandle: buf(5) } as unknown as AuthenticatorResponse,
    });
    expect(asserted.response).toEqual({ clientDataJSON: "AQ", authenticatorData: "Aw", signature: "BA", userHandle: "BQ" });
    expect(asserted.authenticatorAttachment).toBeUndefined();
  });
});

/**
 * A passkey authenticator as navigator.credentials: it answers with client
 * data for `origin` and remembers the passkeys it created.
 */
function fakeCredentials(origin = ORIGIN) {
  const created: ArrayBuffer[] = [];
  const clientData = (type: string, challenge: BufferSource) =>
    new TextEncoder().encode(JSON.stringify({ type, challenge: toBase64url(challenge as Uint8Array), origin })).buffer;
  const credential = (rawId: ArrayBuffer, response: object): CredentialLike => ({
    id: toBase64url(rawId),
    rawId,
    type: "public-key",
    response: response as AuthenticatorResponse,
    getClientExtensionResults: () => ({}),
  });
  return {
    created,
    origin,
    async create(options?: CredentialCreationOptions) {
      const rawId = crypto.getRandomValues(new Uint8Array(16)).buffer;
      created.push(rawId);
      return credential(rawId, {
        clientDataJSON: clientData("webauthn.create", options!.publicKey!.challenge),
        attestationObject: new ArrayBuffer(1),
      }) as unknown as Credential;
    },
    async get(options?: CredentialRequestOptions) {
      const rawId = created.at(-1);
      if (!rawId) throw new DOMException("no passkey", "NotAllowedError");
      return credential(rawId, {
        clientDataJSON: clientData("webauthn.get", options!.publicKey!.challenge),
        authenticatorData: new ArrayBuffer(1),
        signature: new ArrayBuffer(1),
        userHandle: null,
      }) as unknown as Credential;
    },
  };
}

function setup(credentials = fakeCredentials()) {
  const server = createFakeAuthServer({ origin: ORIGIN });
  const signedOut = vi.fn();
  const auth = createAuth({ fetch: server.fetch, credentials, signedOut });
  return { server, auth, credentials, signedOut };
}

async function refusal(promise: Promise<unknown>) {
  try {
    await promise;
  } catch (e) {
    return e;
  }
  throw new Error("expected a failure");
}

describe("the auth flow against the fake panel", () => {
  test("enroll, sign out, sign in", async () => {
    const { server, auth } = setup();
    expect(await auth.session()).toEqual({ name: "dev" });
    await auth.signOut();
    expect(await auth.session()).toBeNull();
    expect(server.signedIn()).toBe(false);

    expect(await auth.enroll(FAKE_ENROLL_TOKEN, " laptop ")).toEqual({ name: "laptop" });
    expect(await auth.session()).toEqual({ name: "laptop" });
    await auth.signOut();
    expect(await auth.signIn()).toEqual({ name: "laptop" });
    expect(server.signedIn()).toBe(true);
  });

  test("a link works once", async () => {
    const { auth } = setup();
    await auth.enroll(FAKE_ENROLL_TOKEN, "laptop");
    expect(failure(await refusal(auth.enroll(FAKE_ENROLL_TOKEN, "phone")))).toBe("linkUsed");
    expect(failure(await refusal(auth.enroll("nope", "phone")))).toBe("linkUnknown");
  });

  test("passkeys from elsewhere and other origins are refused", async () => {
    const { auth } = setup();
    await auth.signOut();
    expect(failure(await refusal(auth.signIn()))).toBe("cancelled"); // the authenticator has none

    const phishing = setup(fakeCredentials("https://vpn.example.com.evil.example"));
    await phishing.auth.signOut();
    expect(failure(await refusal(phishing.auth.enroll(FAKE_ENROLL_TOKEN, "x")))).toBe("rejected");
  });

  test("without WebAuthn", async () => {
    const auth = createAuth({ fetch: createFakeAuthServer({ origin: ORIGIN }).fetch });
    expect(failure(await refusal(auth.signIn()))).toBe("unsupported");
  });

  test("passkeys: list, issue a link, revoke", async () => {
    const { auth, credentials, signedOut } = setup();
    const link = await auth.issueEnrollment();
    const token = link.url.replace(`${ORIGIN}/enroll#`, "");
    expect(token).toMatch(/^[\w-]{43}$/);
    await auth.enroll(token, "phone");
    expect(credentials.created).toHaveLength(1);

    const list = await auth.passkeys();
    expect(list.map((p) => [p.name, p.current])).toEqual([
      ["dev", false],
      ["phone", true],
    ]);
    expect(await auth.revoke(list[0]!.id)).toEqual({ signedOut: false });
    expect(await auth.revoke(list[1]!.id)).toEqual({ signedOut: true });
    expect(await auth.session()).toBeNull();
    expect(signedOut).not.toHaveBeenCalled();

    const e = await refusal(auth.passkeys());
    expect(e).toBeInstanceOf(AuthError);
    expect(signedOut).toHaveBeenCalledOnce();
  });
});

test("failures by kind", () => {
  expect(failure(new DOMException("closed", "NotAllowedError"))).toBe("cancelled");
  expect(failure(new AuthError(429, "too many"))).toBe("tooMany");
  expect(failure(new AuthError(500, "boom"))).toBe("failed");
  expect(failure(new TypeError("network"))).toBe("failed");
});

test("returnPath only returns to the console", () => {
  expect(returnPath("/admin/passkeys")).toBe("/admin/passkeys");
  expect(returnPath("/admin")).toBe("/admin");
  for (const next of [null, "", "/administrator", "//evil.example/admin", "https://evil.example/admin", "/u/token"]) {
    expect(returnPath(next)).toBe("/admin");
  }
});

describe("onUnauthenticated", () => {
  const transport = (signIn: () => void) =>
    createFakeTransport({ seed: 1, signedIn: () => false, interceptors: [onUnauthenticated(signIn)] });

  test("an operator stream or call without a session leads to sign-in", async () => {
    const signIn = vi.fn();
    const status = createClient(StatusService, transport(signIn));
    const read = async () => {
      for await (const _ of status.watch({})) return;
    };
    expect(ConnectError.from(await refusal(read())).code).toBe(Code.Unauthenticated);
    expect(signIn).toHaveBeenCalledOnce();

    const history = createClient(HistoryService, transport(signIn));
    await refusal(history.upstream({ name: "lv-1" }));
    expect(signIn).toHaveBeenCalledTimes(2);
  });

  test("the user page is not an operator's", async () => {
    const signIn = vi.fn();
    const self = createClient(UserSelfService, transport(signIn));
    expect((await self.get({})).name).toBe("alice");
    expect(signIn).not.toHaveBeenCalled();
  });
});
