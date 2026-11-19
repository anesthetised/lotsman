import solid from "@solidjs/vite-plugin";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vitest/config";

// The CSP the panel should send. `vite preview` sends it too, so the
// Playwright smoke test fails if the build ever needs inline script or style.
const csp = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self'",
  "img-src 'self' data:",
  "connect-src 'self'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
].join("; ");

export default defineConfig(({ mode }) => ({
  plugins: [solid(), tailwindcss()],
  build: {
    // The production build goes where the Go side embeds it (internal/web);
    // the e2e build (fake daemon inside) stays local.
    outDir: mode === "e2e" ? "dist-e2e" : "../internal/web/dist",
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
