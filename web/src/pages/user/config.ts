// Handing a config to the user: as a file, with a name the Amnezia apps accept.

/**
 * The file name for a device's config: the apps take the tunnel's name from
 * it, and WireGuard tunnel names are at most 15 characters of `[A-Za-z0-9_=+.-]`.
 */
export function confFilename(device: string, profile: string): string {
  const name = `${device}-${profile}`
    .normalize("NFKD")
    .replace(/\p{M}+/gu, "") // accents: é is e and a combining mark
    .replace(/[^A-Za-z0-9_=+.-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 15)
    .replace(/-+$/, "");
  return `${name || "lotsman"}.conf`;
}

/** Saves `text` as a file through a temporary link: no request leaves the page. */
export function saveAsFile(text: string, filename: string): void {
  const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}
