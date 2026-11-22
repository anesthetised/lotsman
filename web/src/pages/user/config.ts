// Handing a config to the user: as a file, with a name the Amnezia apps accept.

/**
 * The file name for a device's config: the apps take the tunnel's name from
 * it, and WireGuard tunnel names are at most 15 characters of `[A-Za-z0-9_=+.-]`.
 */
export function confFilename(device: string): string {
  const name = device
    .normalize("NFKD")
    .replace(/\p{M}+/gu, "") // accents: é is e and a combining mark
    .replace(/[^A-Za-z0-9_=+.-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 15)
    .replace(/-+$/, "");
  return `${name || "lotsman"}.conf`;
}
