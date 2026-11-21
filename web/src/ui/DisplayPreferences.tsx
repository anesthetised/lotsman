import { useI18n } from "../i18n";
import { usePreferences } from "../preferences";

export function DisplayPreferences() {
  const { t } = useI18n();
  const { language, appearance } = usePreferences();
  const caption = "pointer-events-none absolute top-1 left-2 text-xs text-muted";
  const select = "min-h-11 cursor-pointer rounded-md border border-line bg-surface pt-4 pr-1 pl-2 text-sm font-medium hover:bg-bg";
  return (
    <div class="flex flex-wrap items-center gap-2">
      <label class="relative">
        <span class={caption}>{t.app.language}</span>
        <select class={select} value={language.value()} onChange={(e) => language.choose(e.currentTarget.value)}>
          <option value="browser">{t.app.auto}</option>
          <option value="en" lang="en">English</option>
          <option value="ru" lang="ru">Русский</option>
        </select>
      </label>
      <label class="relative">
        <span class={caption}>{t.app.theme}</span>
        <select class={select} value={appearance.value()} onChange={(e) => appearance.choose(e.currentTarget.value)}>
          <option value="system">{t.app.system}</option>
          <option value="light">{t.app.light}</option>
          <option value="dark">{t.app.dark}</option>
        </select>
      </label>
    </div>
  );
}
