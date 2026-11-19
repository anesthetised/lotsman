// An operator call that fails with Unauthenticated (no session, or it
// expired) sends the browser to the sign-in page. Streams fail while being
// read, not when opened, so their messages are watched too. User-page
// calls are left alone: a user link is not an operator's sign-in.

import { Code, ConnectError, type Interceptor } from "@connectrpc/connect";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";

export function onUnauthenticated(signIn: () => void): Interceptor {
  const check = (e: unknown) => {
    if (ConnectError.from(e).code === Code.Unauthenticated) signIn();
    return e;
  };
  async function* watched<T>(messages: AsyncIterable<T>): AsyncIterable<T> {
    try {
      yield* messages;
    } catch (e) {
      throw check(e);
    }
  }
  return (next) => async (req) => {
    if (req.service.typeName === UserSelfService.typeName) return next(req);
    try {
      const res = await next(req);
      return res.stream ? { ...res, message: watched(res.message) } : res;
    } catch (e) {
      throw check(e);
    }
  };
}
