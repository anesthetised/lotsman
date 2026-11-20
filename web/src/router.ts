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
      children: [
        { path: "/", component: lazy(() => import("./pages/admin/Overview")) },
        { path: "/upstreams/:name", component: lazy(() => import("./pages/admin/UpstreamDetail")) },
        { path: "/users", component: lazy(() => import("./pages/admin/Users")) },
        { path: "/users/:name", component: lazy(() => import("./pages/admin/UserDetail")) },
        { path: "/connections", component: lazy(() => import("./pages/admin/Connections")) },
        { path: "/incidents", component: lazy(() => import("./pages/admin/Incidents")) },
        { path: "/passkeys", component: lazy(() => import("./pages/admin/Passkeys")) },
      ],
    },
    { path: "/login", component: lazy(() => import("./pages/auth/SignIn")) },
    { path: "/enroll", component: lazy(() => import("./pages/auth/Enroll")) },
    { path: "/u", component: lazy(() => import("./pages/user/UserPage")) },
    { path: "*404", component: NotFound },
  ],
});
