// A user's link is `<public_url>/u#<token>`. The token is in the fragment,
// which the browser never sends to the server, so no proxy logs it. The page
// reads it from the address and keeps it in memory only; it stays in the
// address bar, as the user bookmarks the link.

import type { Interceptor } from "@connectrpc/connect";

/** A token as the daemon issues it: 256 bits, base64url without padding. */
const tokenPattern = /^[A-Za-z0-9_-]{43}$/;

/** The token in a link's fragment (`location.hash`), or undefined when it is missing or malformed. */
export function linkToken(hash: string): string | undefined {
  const token = hash.startsWith("#") ? hash.slice(1) : hash;
  return tokenPattern.test(token) ? token : undefined;
}

/**
 * Sends the user's link with every call. A header rather than a cookie: no
 * other site can make the browser send it.
 */
export function withLink(token: string): Interceptor {
  return (next) => (req) => {
    req.header.set("Authorization", `Bearer ${token}`);
    return next(req);
  };
}
