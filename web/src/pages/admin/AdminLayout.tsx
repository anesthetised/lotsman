// The operator console shell: a compact header and a wide content area,
// shown only to a signed-in operator; anyone else goes to the sign-in page.

import { useLocation, useNavigate } from "@solidjs/router";
import type { JSX } from "@solidjs/web";
import { createMemo, Errored, Loading, onSettled, Show } from "solid-js";
import { redirectToSignIn } from "../../api/auth";
import { auth } from "../../api/client";
import { useI18n } from "../../i18n";
import { buttonClass } from "../../ui/button";
import { LiveError } from "../LiveError";
import { SkipLink } from "../SkipLink";

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
  const link = (href: string, label: string) => (
    <a
      href={href}
      aria-current={location.pathname === href ? "page" : undefined}
      class="rounded px-2 py-1 text-muted hover:text-fg aria-[current]:font-medium aria-[current]:text-fg"
    >
      {label}
    </a>
  );
  return (
    <div class="min-h-screen text-sm">
      <SkipLink />
      <header class="border-b border-line bg-surface">
        <div class="mx-auto flex h-11 max-w-7xl items-center gap-6 px-4">
          <span class="font-semibold">{t.app.name}</span>
          <nav aria-label={t.admin.nav} class="flex gap-1">
            {link("/admin", t.admin.upstreams)}
            {link("/admin/passkeys", t.admin.passkeys)}
          </nav>
          <span class="ml-auto hidden text-muted sm:inline">{t.auth.signedInAs(props.name)}</span>
          <button type="button" class={buttonClass("ghost")} onClick={signOut}>
            {t.auth.signOut}
          </button>
        </div>
      </header>
      <main id="main" class="mx-auto max-w-7xl px-4 py-4">
        {props.children}
      </main>
    </div>
  );
}
