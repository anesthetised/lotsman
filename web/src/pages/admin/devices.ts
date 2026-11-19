// Handing a device config or a user link over: pure helpers of the users page.

/** wg-quick names the interface after the file, and Linux allows 15 characters. */
const MAX_NAME = 15;

/**
 * The file name of a downloaded config: user, device and profile, as much of
 * that as fits an interface name, with anything wg-quick refuses replaced.
 */
export function configFileName(user: string, device: string, profile: string): string {
  const clean = (s: string) => s.replace(/[^A-Za-z0-9_=+.-]/g, "_");
  const names = [`${user}-${device}-${profile}`, `${user}-${device}`, device].map(clean);
  return `${names.find((n) => n.length <= MAX_NAME) ?? names[1]!.slice(0, MAX_NAME)}.conf`;
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
