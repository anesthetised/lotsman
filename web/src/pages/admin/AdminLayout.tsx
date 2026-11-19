// The operator console shell: a compact header and a wide content area.

import type { JSX } from "@solidjs/web";
import { useI18n } from "../../i18n";
import { SkipLink } from "../SkipLink";

export function AdminLayout(props: { children?: JSX.Element }) {
  const { t } = useI18n();
  return (
    <div class="min-h-screen text-sm">
      <SkipLink />
      <header class="border-b border-line bg-surface">
        <div class="mx-auto flex h-11 max-w-7xl items-center gap-6 px-4">
          <span class="font-semibold">{t.app.name}</span>
          <nav aria-label={t.admin.nav}>
            <a href="/admin" class="rounded px-2 py-1 text-muted hover:text-fg aria-[current]:text-fg aria-[current]:font-medium">
              {t.admin.upstreams}
            </a>
          </nav>
        </div>
      </header>
      <main id="main" class="mx-auto max-w-7xl px-4 py-4">
        {props.children}
      </main>
    </div>
  );
}
