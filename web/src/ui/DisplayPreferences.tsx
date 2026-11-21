import { useI18n } from "../i18n";
import { usePreferences } from "../preferences";
import { ChoiceMenu } from "./ChoiceMenu";
import { GlobeIcon, MoonIcon, SunIcon } from "./icons";

export function DisplayPreferences() {
  const { t } = useI18n();
  const { language, appearance, theme } = usePreferences();
  return (
    <div class="flex items-center gap-1">
      <ChoiceMenu
        label={t.app.language}
        icon={<GlobeIcon />}
        value={language.value()}
        choices={[
          { value: "browser", label: t.app.auto },
          { value: "en", label: "English", lang: "en" },
          { value: "ru", label: "Русский", lang: "ru" },
        ]}
        onChange={language.choose}
      />
      <ChoiceMenu
        label={t.app.theme}
        icon={theme() === "dark" ? <MoonIcon /> : <SunIcon />}
        value={appearance.value()}
        choices={[
          { value: "system", label: t.app.system },
          { value: "light", label: t.app.light },
          { value: "dark", label: t.app.dark },
        ]}
        onChange={appearance.choose}
      />
    </div>
  );
}
