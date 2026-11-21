// Browser-local display settings shared by both panels. An absent setting
// follows the browser language or the OS theme, including subsequent changes.
import type { JSX } from "@solidjs/web";
import { createContext, createMemo, createRenderEffect, createSignal, merge, onSettled, useContext } from "solid-js";
import { i18n, I18nContext, locales, pickLocale } from "./i18n";

function storedChoice<T extends string>(key: string, choices: readonly T[], fallback: T) {
  const valid = (raw: string | null) => choices.find((choice) => choice === raw) ?? fallback;
  const read = () => {
    try {
      return valid(localStorage.getItem(key));
    } catch {
      return fallback;
    }
  };
  const [value, setValue] = createSignal<T>(read);
  const choose = (raw: string) => {
    const next = valid(raw);
    setValue(() => next);
    try {
      if (next === fallback) localStorage.removeItem(key);
      else localStorage.setItem(key, next);
    } catch {
      // Storage can be blocked or full. The choice still applies to this page.
    }
  };
  onSettled(() => {
    const sync = (event: StorageEvent) => {
      if (event.key === key || event.key === null) setValue(read);
    };
    window.addEventListener("storage", sync);
    return () => window.removeEventListener("storage", sync);
  });
  return { value, choose };
}

function createPreferences() {
  const language = storedChoice("lotsman.language", ["browser", ...locales] as const, "browser");
  const appearance = storedChoice("lotsman.theme", ["system", "light", "dark"] as const, "system");
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  const [dark, setDark] = createSignal(media.matches);
  const [browserLanguage, setBrowserLanguage] = createSignal(pickLocale(navigator.languages));
  onSettled(() => {
    const onScheme = () => setDark(media.matches);
    const onLanguage = () => setBrowserLanguage(pickLocale(navigator.languages));
    media.addEventListener("change", onScheme);
    window.addEventListener("languagechange", onLanguage);
    return () => {
      media.removeEventListener("change", onScheme);
      window.removeEventListener("languagechange", onLanguage);
    };
  });

  const locale = createMemo(() => {
    const choice = language.value();
    return choice === "browser" ? browserLanguage() : choice;
  });
  const theme = createMemo(() => {
    const choice = appearance.value();
    return choice === "system" ? (dark() ? "dark" : "light") : choice;
  });
  return { language, appearance, locale, theme };
}

const PreferencesContext = createContext<ReturnType<typeof createPreferences>>();

export const usePreferences = () => useContext(PreferencesContext)!;

export function PreferencesProvider(props: { children?: JSX.Element }) {
  const prefs = createPreferences();
  const current = createMemo(() => i18n(prefs.locale()));
  // Keep the dictionaries and formatters live even where a component
  // destructures { t, fmt } once. The router and its streams stay mounted.
  const lang = {
    get locale() {
      return prefs.locale();
    },
    t: merge(() => current().t),
    fmt: merge(() => current().fmt),
  };
  createRenderEffect(prefs.theme, (theme) => {
    document.documentElement.dataset.theme = theme;
  });
  createRenderEffect(current, ({ locale, t }) => {
    document.documentElement.lang = locale;
    document.title = t.app.name;
  });
  return (
    <PreferencesContext value={prefs}>
      <I18nContext value={lang}>{props.children}</I18nContext>
    </PreferencesContext>
  );
}
