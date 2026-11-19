import { expect, test } from "./fixtures";

test("the user page shows live speed and devices on a phone", async ({ page }) => {
  await page.goto("/u/test-token");
  await expect(page.getByRole("heading", { name: "Hello, alice" })).toBeVisible();

  const speed = page.getByRole("region", { name: "Speed now" });
  const before = await speed.textContent();
  await expect(speed).not.toHaveText(before!, { timeout: 5000 });

  const devices = page.getByRole("region", { name: "Devices" }).getByRole("heading", { level: 3 });
  await expect(devices).toHaveText(["phone", "laptop", "tv"]);

  // Mobile-first: nothing scrolls sideways.
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});

test("the page tells the browser never to send the link as a referrer", async ({ page }) => {
  const response = await page.goto("/u/test-token");
  expect(response?.headers()["referrer-policy"]).toBe("no-referrer");
  await expect(page.locator('meta[name="referrer"]')).toHaveAttribute("content", "no-referrer");
});
