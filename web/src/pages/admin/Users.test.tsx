import { create } from "@bufbuild/protobuf";
import { cleanup, fireEvent, render, screen } from "@solidjs/testing-library";
import { createRouter, memoryHistory } from "@solidjs/router";
import { Loading } from "solid-js";
import { afterEach, expect, test, vi } from "vitest";
import { admin } from "../../api/admin";
import { ListUsersResponseSchema } from "../../gen/lotsman/v1/user_pb";
import Users from "./Users";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

test("loads the selected page of users and can go back from the last page", async () => {
  const list = vi.spyOn(admin.users, "list").mockImplementation(async (req) =>
    create(ListUsersResponseSchema, {
      hasMore: !req.offset,
      users: [{ name: req.offset ? "zoe" : "alice" }],
    }),
  );
  const Router = createRouter({ routes: [{ path: "/admin/users", component: Users }], history: memoryHistory("/admin/users") });
  render(() => <Router>{(props) => <Loading>{props.children}</Loading>}</Router>);

  await screen.findByRole("link", { name: "alice" });
  expect(list).toHaveBeenLastCalledWith({ limit: 25, offset: 0 });
  expect((screen.getByRole("button", { name: "Previous page" }) as HTMLButtonElement).disabled).toBe(true);

  fireEvent.click(screen.getByRole("button", { name: "Next page" }));
  await screen.findByRole("link", { name: "zoe" });
  expect(list).toHaveBeenLastCalledWith({ limit: 25, offset: 25 });
  expect(screen.queryByRole("link", { name: "alice" })).toBeNull();
  expect((screen.getByRole("button", { name: "Next page" }) as HTMLButtonElement).disabled).toBe(true);

  fireEvent.click(screen.getByRole("button", { name: "Previous page" }));
  await screen.findByRole("link", { name: "alice" });
  expect(list).toHaveBeenLastCalledWith({ limit: 25, offset: 0 });
});
