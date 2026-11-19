import { test as base, expect, type Locator } from "@playwright/test";

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

/**
 * Waits until focus is inside `layer` (a dialog or a menu) and two more frames
 * have passed. Kobalte moves focus into a layer and attaches its key and
 * outside-click handlers a frame or so after the layer opens, so a key pressed
 * earlier can be lost. A person never presses that fast.
 */
export async function settled(layer: Locator): Promise<void> {
  await expect.poll(() => layer.evaluate((el) => el.contains(document.activeElement))).toBe(true);
  await layer.page().evaluate(() => new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(done))));
}

/** Presses `key` inside `layer` once it has settled. */
export async function pressIn(layer: Locator, key: string): Promise<void> {
  await settled(layer);
  await layer.page().keyboard.press(key);
}

export { expect };
