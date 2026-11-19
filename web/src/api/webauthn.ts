// The JSON forms of WebAuthn options and credentials that the panel speaks
// (base64url for every binary field, as in the spec's *JSON types), and the
// conversions to and from what navigator.credentials takes and returns.
// Written out rather than using parseCreationOptionsFromJSON / toJSON,
// which older Safari and Firefox releases lack.

export function toBase64url(bytes: ArrayBuffer | ArrayBufferView): string {
  const view = ArrayBuffer.isView(bytes) ? new Uint8Array(bytes.buffer, bytes.byteOffset, bytes.byteLength) : new Uint8Array(bytes);
  let binary = "";
  for (const b of view) binary += String.fromCharCode(b);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

export function fromBase64url(text: string): Uint8Array<ArrayBuffer> {
  const binary = atob(text.replaceAll("-", "+").replaceAll("_", "/"));
  return Uint8Array.from(binary, (c) => c.charCodeAt(0));
}

interface DescriptorJSON {
  type: "public-key";
  id: string;
  transports?: AuthenticatorTransport[];
}

export interface CreationOptionsJSON {
  publicKey: Omit<PublicKeyCredentialCreationOptions, "challenge" | "user" | "excludeCredentials"> & {
    challenge: string;
    user: { id: string; name: string; displayName: string };
    excludeCredentials?: DescriptorJSON[];
  };
}

export interface RequestOptionsJSON {
  publicKey: Omit<PublicKeyCredentialRequestOptions, "challenge" | "allowCredentials"> & {
    challenge: string;
    allowCredentials?: DescriptorJSON[];
  };
}

const descriptor = (d: DescriptorJSON): PublicKeyCredentialDescriptor => ({ ...d, id: fromBase64url(d.id) });

export function creationOptions(json: CreationOptionsJSON): CredentialCreationOptions {
  const o = json.publicKey;
  return {
    publicKey: {
      ...o,
      challenge: fromBase64url(o.challenge),
      user: { ...o.user, id: fromBase64url(o.user.id) },
      excludeCredentials: o.excludeCredentials?.map(descriptor),
    },
  };
}

export function requestOptions(json: RequestOptionsJSON): CredentialRequestOptions {
  const o = json.publicKey;
  return {
    publicKey: { ...o, challenge: fromBase64url(o.challenge), allowCredentials: o.allowCredentials?.map(descriptor) },
  };
}

/** The parts of a PublicKeyCredential this module reads; tests fake them. */
export interface CredentialLike {
  id: string;
  rawId: ArrayBuffer;
  type: string;
  authenticatorAttachment?: string | null;
  response: AuthenticatorResponse;
  getClientExtensionResults(): AuthenticationExtensionsClientOutputs;
}

/** A registration or assertion as the panel's finish endpoints take it. */
export function credentialJSON(credential: CredentialLike): Record<string, unknown> {
  const r = credential.response;
  const response: Record<string, unknown> = { clientDataJSON: toBase64url(r.clientDataJSON) };
  if ("attestationObject" in r) {
    const attestation = r as AuthenticatorAttestationResponse;
    response.attestationObject = toBase64url(attestation.attestationObject);
    response.transports = attestation.getTransports?.() ?? [];
  } else {
    const assertion = r as AuthenticatorAssertionResponse;
    response.authenticatorData = toBase64url(assertion.authenticatorData);
    response.signature = toBase64url(assertion.signature);
    if (assertion.userHandle) response.userHandle = toBase64url(assertion.userHandle);
  }
  return {
    id: credential.id,
    rawId: toBase64url(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment ?? undefined,
    response,
    clientExtensionResults: credential.getClientExtensionResults(),
  };
}
