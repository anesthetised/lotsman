import { useI18n } from "../i18n";

/** The first focusable element: jumps keyboard users past the header to `#main`. */
export function SkipLink() {
  const { t } = useI18n();
  return (
    <a
      href="#main"
      class="sr-only rounded bg-accent px-3 py-2 text-accent-fg focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-50"
    >
      {t.app.skipToContent}
    </a>
  );
}
