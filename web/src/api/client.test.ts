import { createClient, createRouterTransport } from "@connectrpc/connect";
import { expect, test } from "vitest";
import { UserSelfService } from "../gen/lotsman/v1/user_self_pb";
import { withLink } from "./client";

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
    { transport: { interceptors: [withLink("secret-token")] } },
  );
  const self = createClient(UserSelfService, transport);
  await self.get({});
  await self.get({});
  expect(seen).toEqual(["Bearer secret-token", "Bearer secret-token"]);
});
