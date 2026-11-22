// The operator console shell: a compact header and a wide content area,
// shown only to a signed-in operator; anyone else goes to the sign-in page.

import { useLocation, useNavigate } from "@solidjs/router";
import type { JSX } from "@solidjs/web";
import { createMemo, Errored, Loading, onSettled, Show } from "solid-js";
import { redirectToSignIn } from "../../api/auth";
import { api, auth } from "../../api/client";
import { createLive } from "../../api/live";
import { DisplayPreferences } from "../../ui/DisplayPreferences";
import { useI18n } from "../../i18n";
import { buttonClass } from "../../ui/button";
import { LiveError } from "../LiveError";
import { SkipLink } from "../SkipLink";
import { openCount } from "./incidentLog";

export function AdminLayout(props: { children?: JSX.Element }) {
  const { t } = useI18n();
  const session = createMemo(() => auth.session());
  return (
    <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
      <Loading fallback={<p class="p-4 text-sm text-muted">{t.common.connecting}</p>}>
        <Show when={session()} fallback={<ToSignIn />}>
          {(s) => <Shell name={s().name}>{props.children}</Shell>}
        </Show>
      </Loading>
    </Errored>
  );
}

/**
 * How many incidents are open, live: a count beside the nav item, nothing
 * while none is open, before the first snapshot or when the stream fails
 * (the page itself says why).
 */
function OpenIncidents() {
  return (
    <Errored fallback={null}>
      <Loading fallback={null}>
        <OpenCount />
      </Loading>
    </Errored>
  );
}

function OpenCount() {
  const { t } = useI18n();
  const snapshot = createLive((signal) => api.status.watch({}, { signal }));
  const count = createMemo(() => openCount(snapshot().upstreams));
  return (
    <Show when={count() > 0}>
      <span aria-hidden="true" class="ml-1.5 rounded-full bg-bad px-1.5 py-px text-xs font-medium text-accent-fg tabular-nums">
        {count()}
      </span>
      <span class="sr-only"> ({t.admin.openIncidents(count())})</span>
    </Show>
  );
}

function ToSignIn() {
  onSettled(() => redirectToSignIn());
  return null;
}

function Shell(props: { name: string; children?: JSX.Element }) {
  const { t } = useI18n();
  const location = useLocation();
  const navigate = useNavigate();
  const signOut = async () => {
    await auth.signOut();
    navigate("/login", { replace: true });
  };
  // A section's link is current on its own page and on the pages under it
  // (an upstream's page belongs to the overview, a user's to the users).
  const link = (href: string, label: () => string, under?: string, extra?: JSX.Element) => {
    const current = () => location.pathname === href || (under !== undefined && location.pathname.startsWith(under));
    return (
      <a
        href={href}
        aria-current={current() ? "page" : undefined}
        class="rounded px-2 py-1 whitespace-nowrap text-muted hover:text-fg aria-[current]:font-medium aria-[current]:text-fg"
      >
        {label()}
        {extra}
      </a>
    );
  };
  return (
    <div class="min-h-screen text-sm">
      <SkipLink />
      <header class="border-b border-line bg-surface">
        <div class="mx-auto flex min-h-11 max-w-7xl flex-wrap items-center gap-x-6 gap-y-1 px-4 py-1">
          <span class="font-semibold">{t.app.name}</span>
          <nav aria-label={t.admin.nav} class="flex flex-wrap gap-1">
            {link("/admin", () => t.admin.upstreams, "/admin/upstreams/")}
            {link("/admin/users", () => t.admin.users, "/admin/users/")}
            {link("/admin/connections", () => t.admin.connections)}
            {link("/admin/incidents", () => t.admin.incidents, undefined, <OpenIncidents />)}
            {link("/admin/passkeys", () => t.admin.passkeys)}
          </nav>
          <div class="ml-auto flex flex-wrap items-center gap-x-3 gap-y-1">
            <DisplayPreferences />
            <span class="hidden text-muted sm:inline">{t.auth.signedInAs(props.name)}</span>
            <button type="button" class={buttonClass("ghost")} onClick={signOut}>
              {t.auth.signOut}
            </button>
          </div>
        </div>
      </header>
      <main id="main" class="mx-auto max-w-7xl px-4 py-4">
        {props.children}
      </main>
    </div>
  );
}
