import { useI18n } from "../i18n";

export function NotFound() {
  const { t } = useI18n();
  return (
    <main id="main" class="mx-auto max-w-md px-4 py-16 text-center">
      <h1 class="text-xl font-semibold">{t.common.notFound}</h1>
    </main>
  );
}
