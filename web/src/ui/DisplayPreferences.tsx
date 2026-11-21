import type { JSX } from "@solidjs/web";
import { useI18n } from "../i18n";
import { usePreferences } from "../preferences";
import { ChevronDownIcon } from "./icons";

function PreferenceSelect(props: { label: string; value: string; choose: (value: string) => void; children: JSX.Element }) {
  return (
    <label class="grid gap-1">
      <span class="px-0.5 text-xs leading-none text-muted">{props.label}</span>
      <span class="relative">
        <select
          class="block min-h-11 w-full cursor-pointer appearance-none rounded-md border border-line bg-surface py-2 pr-8 pl-2 text-sm font-medium hover:bg-bg"
          value={props.value}
          onChange={(e) => props.choose(e.currentTarget.value)}
        >
          {props.children}
        </select>
        <span class="pointer-events-none absolute top-1/2 right-2 -translate-y-1/2 text-muted">
          <ChevronDownIcon />
        </span>
      </span>
    </label>
  );
}

export function DisplayPreferences() {
  const { t } = useI18n();
  const { language, appearance } = usePreferences();
  return (
    <div class="flex flex-wrap items-end gap-2">
      <PreferenceSelect label={t.app.language} value={language.value()} choose={language.choose}>
        <option value="browser">{t.app.auto}</option>
        <option value="en" lang="en">English</option>
        <option value="ru" lang="ru">Русский</option>
      </PreferenceSelect>
      <PreferenceSelect label={t.app.theme} value={appearance.value()} choose={appearance.choose}>
        <option value="system">{t.app.system}</option>
        <option value="light">{t.app.light}</option>
        <option value="dark">{t.app.dark}</option>
      </PreferenceSelect>
    </div>
  );
}
