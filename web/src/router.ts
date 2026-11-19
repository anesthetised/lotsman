// Routes. Each area is its own chunk, so the user page never downloads the
// operator console (Kobalte menus, uPlot) and the other way round.

import { createRouter } from "@solidjs/router";
import { lazy } from "solid-js";
import { AdminLayout } from "./pages/admin/AdminLayout";
import { NotFound } from "./pages/NotFound";

export const Router = createRouter({
  routes: [
    {
      path: "/admin",
      component: AdminLayout,
      children: [{ path: "/", component: lazy(() => import("./pages/admin/Overview")) }],
    },
    { path: "/u/:token", component: lazy(() => import("./pages/user/UserPage")) },
    { path: "*404", component: NotFound },
  ],
});
