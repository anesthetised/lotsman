import { test as base, expect } from "@playwright/test";

/**
 * Fails a test on any console error or warning (CSP violations land there)
 * and on any request that leaves the page's origin.
 */
export const test = base.extend<{ problems: string[] }>({
  problems: [
    async ({ page, baseURL }, use) => {
      const problems: string[] = [];
      page.on("console", (m) => {
        if (m.type() === "error" || m.type() === "warning") problems.push(`${m.type()}: ${m.text()}`);
      });
      page.on("pageerror", (e) => problems.push(`uncaught: ${e.message}`));
      page.on("request", (r) => {
        if (new URL(r.url()).origin !== new URL(baseURL!).origin) problems.push(`external request: ${r.url()}`);
      });
      await use(problems);
      expect(problems).toEqual([]);
    },
    { auto: true },
  ],
});

export { expect };
