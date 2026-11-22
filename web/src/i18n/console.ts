import { merge } from "solid-js";
import { consoleEn, type ConsoleDict } from "./console.en";
import { consoleRu } from "./console.ru";
import { type I18n, type Locale, useI18n } from "./index";

export const consoleDictionaries = { en: consoleEn, ru: consoleRu } satisfies Record<Locale, ConsoleDict>;

/** The page language with the console's strings as `c`, next to the shared `t`. */
export function useConsoleI18n(): I18n & { c: ConsoleDict } {
  const i18n = useI18n();
  return merge(i18n, { c: merge(() => consoleDictionaries[i18n.locale]) });
}
