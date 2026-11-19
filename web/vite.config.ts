import { writeFileSync } from "node:fs";
import { join } from "node:path";
import solid from "@solidjs/vite-plugin";
import tailwindcss from "@tailwindcss/vite";
import type { Plugin } from "vite";
import { defineConfig } from "vitest/config";

// The CSP the panel sends (internal/web/headers.go), spelled out. `vite
// preview` sends it too, so the Playwright tests fail if the build ever needs
// inline script or style, or a data: image (QR codes are inline SVG).
const csp = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self'",
  "img-src 'self'",
  "connect-src 'self'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
].join("; ");

/**
 * Vite empties the output directory before writing, so this puts back the
 * committed .gitkeep that keeps internal/web/dist, and with it the Go side's
 * `//go:embed all:dist`, present without a build.
 */
function keepGitkeep(): Plugin {
  return {
    name: "lotsman:keep-gitkeep",
    apply: "build",
    writeBundle(options) {
      if (options.dir) writeFileSync(join(options.dir, ".gitkeep"), "");
    },
  };
}

export default defineConfig(({ mode }) => ({
  plugins: [solid(), tailwindcss(), mode === "e2e" ? [] : keepGitkeep()],
  build: {
    // The production build goes where the Go side embeds it (internal/web);
    // the e2e build (fake daemon inside) stays local.
    outDir: mode === "e2e" ? "dist-e2e" : "../internal/web/dist",
    // Old hashed assets would pile up otherwise; keepGitkeep restores .gitkeep.
    emptyOutDir: true,
  },
  server: {
    watch: { ignored: ["**/dist-e2e/**", "**/test-results/**"] },
  },
  preview: {
    headers: { "Content-Security-Policy": csp, "Referrer-Policy": "no-referrer" },
  },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["src/test-setup.ts"],
  },
}));
