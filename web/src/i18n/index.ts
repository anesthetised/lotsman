import { createContext, useContext } from "solid-js";
import { en, type Dict } from "./en";
import { formatters, type Formatters } from "./format";
import { ru } from "./ru";

const dictionaries = { en, ru } satisfies Record<string, Dict>;

export type Locale = keyof typeof dictionaries;
export const locales = Object.keys(dictionaries) as Locale[];

/** The first supported language in the browser's preference list, else English. */
export function pickLocale(languages: readonly string[]): Locale {
  for (const tag of languages) {
    const primary = tag.toLowerCase().split("-")[0];
    if (primary && Object.hasOwn(dictionaries, primary)) return primary as Locale;
  }
  return "en";
}

export interface I18n {
  locale: Locale;
  t: Dict;
  fmt: Formatters;
}

export function i18n(locale: Locale): I18n {
  return { locale, t: dictionaries[locale], fmt: formatters(locale) };
}

/** English by default, so components render outside the app shell (tests) too. */
export const I18nContext = createContext<I18n>(i18n("en"));

export function useI18n(): I18n {
  return useContext(I18nContext);
}
