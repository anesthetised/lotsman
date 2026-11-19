import { expect, test } from "./fixtures";

test("the operator overview shows live data", async ({ page }) => {
  await page.goto("/admin");
  await expect(page.getByRole("heading", { name: "Upstreams" })).toBeVisible();
  const rows = page.getByRole("table").first().getByRole("row");
  await expect(rows).toHaveCount(5); // header + 4 upstreams

  // The rate in the chart caption changes as snapshots stream in.
  const caption = page.locator("figcaption");
  const before = await caption.textContent();
  await expect(caption).not.toHaveText(before!, { timeout: 5000 });
  await expect(page.locator("figure canvas")).toHaveCount(1);
  await expect(page.getByRole("img", { name: /Availability of lv-1 over 7 days: \d/ })).toBeVisible();
});

test("the details dialog works from the keyboard", async ({ page }) => {
  await page.goto("/admin");
  const trigger = page.getByRole("button", { name: "Details lv-1" });
  await trigger.focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "Upstream lv-1" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Close" })).toBeFocused();
  await expect(dialog.getByRole("row")).toHaveCount(3); // header + alice's phone and laptop

  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(trigger).toBeFocused();

  // A click outside dismisses it too.
  await trigger.click();
  await expect(dialog).toBeVisible();
  await page.mouse.click(5, 5);
  await expect(dialog).toBeHidden();
});

test("the window menu works from the keyboard", async ({ page }) => {
  await page.goto("/admin");
  const trigger = page.getByRole("button", { name: "Window: 1 min" });
  await trigger.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("menuitemradio", { name: "1 min" })).toHaveAttribute("aria-checked", "true");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect(page.getByRole("menu")).toBeHidden();
  await expect(page.getByRole("button", { name: "Window: 5 min" })).toBeFocused();
});

test("Russian, dark and strict CSP", async ({ browser, baseURL }) => {
  const context = await browser.newContext({ baseURL, locale: "ru-RU", colorScheme: "dark" });
  const page = await context.newPage();
  const response = await page.goto("/admin");
  expect(response?.headers()["content-security-policy"]).toContain("script-src 'self'");
  await expect(page.locator("html")).toHaveAttribute("lang", "ru");
  await expect(page.getByRole("heading", { name: "Выходы" })).toBeVisible();
  const background = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  expect(background).toBe("rgb(11, 15, 23)");
  await context.close();
});
