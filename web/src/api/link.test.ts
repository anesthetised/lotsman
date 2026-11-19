import { createClient, createRouterTransport } from "@connectrpc/connect";
import { expect, test } from "vitest";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { linkToken, withLink } from "./link";

const token = "q2V8Zp0nY3xW_4b-Lk9sTq1RmA7cJd5eHf6gUi0oPzE";

test("the user page sends its link as a bearer token on every call", async () => {
  const seen: (string | null)[] = [];
  const transport = createRouterTransport(
    ({ service }) => {
      service(UserSelfService, {
        get: (_, ctx) => {
          seen.push(ctx.requestHeader.get("Authorization"));
          return { name: "alice" };
        },
      });
    },
    { transport: { interceptors: [withLink(token)] } },
  );
  const self = createClient(UserSelfService, transport);
  await self.get({});
  await self.get({});
  expect(seen).toEqual([`Bearer ${token}`, `Bearer ${token}`]);
});

test("the token comes from the link's fragment", () => {
  expect(linkToken(`#${token}`)).toBe(token);
});

test("a missing or malformed fragment is no token", () => {
  for (const hash of ["", "#", "#short", `#${token}x`, `#${token.slice(1)}=`, `#${token.slice(1)}+`, `#${token}&x=1`]) {
    expect(linkToken(hash), hash).toBeUndefined();
  }
});
