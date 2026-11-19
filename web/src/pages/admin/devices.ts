// Pure helpers of the users pages: a user's page, their rights in words,
// and handing a device config or a user link over.

import type { Rights } from "../../gen/lotsman/v1/user_pb";
import type { ConsoleDict } from "../../i18n/console.en";

/** The console page of a user. */
export function userPath(name: string): string {
  return `/admin/users/${encodeURIComponent(name)}`;
}

/** A user's rights in a few words: who sets the routes, how many exits, whether providers show. */
export function rightsSummary(c: ConsoleDict, rights: Rights | undefined): string {
  if (!rights) return "";
  return [
    rights.editRoutes ? c.users.routesByUser : c.users.routesByOperator,
    rights.exits.length > 0 ? c.users.someExits(rights.exits.length) : c.users.allExits,
    rights.showProviders ? "" : c.users.countriesOnly,
  ]
    .filter(Boolean)
    .join(" · ");
}

/** wg-quick names the interface after the file, and Linux allows 15 characters. */
const MAX_NAME = 15;

/**
 * The file name of a downloaded config: user and device, or the device alone
 * when both do not fit an interface name, with anything wg-quick refuses replaced.
 */
export function configFileName(user: string, device: string): string {
  const clean = (s: string) => s.replace(/[^A-Za-z0-9_=+.-]/g, "_");
  const names = [`${user}-${device}`, device].map(clean);
  return `${names.find((n) => n.length <= MAX_NAME) ?? names[0]!.slice(0, MAX_NAME)}.conf`;
}

/** A user's page: the token goes after `#`, so it never reaches a server's or proxy's logs. */
export function userLink(origin: string, token: string): string {
  return `${origin}/u#${token}`;
}

/** Hands the browser a text file to save. */
export function download(name: string, text: string): void {
  const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
