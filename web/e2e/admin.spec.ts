import { expect, pressIn, test } from "./fixtures";

test("the operator overview shows live data", async ({ page }) => {
  await page.goto("/admin");
  await expect(page.getByRole("heading", { name: "Upstreams" })).toBeVisible();
  const rows = page.getByRole("table").first().getByRole("row");
  await expect(rows).toHaveCount(6); // header + 5 upstreams

  // The rate in the chart caption changes as snapshots stream in.
  const caption = page.locator("figcaption");
  const before = await caption.textContent();
  await expect(caption).not.toHaveText(before!, { timeout: 5000 });
  await expect(page.locator("figure canvas")).toHaveCount(1);
  await expect(page.getByRole("img", { name: /Availability of lv-1 over 7 days: \d/ })).toBeVisible();
});

test("Outline quotas follow the upstream name and keep the last value on a provider failure", async ({ page }) => {
  const quota = { limit: 500, remaining: 464.32, unit: "GB", updatedAt: "2026-11-21T15:00:00Z", stale: true };
  await page.route("**/api/outline-usage", (route) => route.fulfill({
    json: { "lv-2": quota, "al-1": null, "lv-1": { ...quota, remaining: 123 } },
  }));
  await page.goto("/admin");
  const row = (name: string) => page.getByRole("row").filter({ has: page.getByRole("link", { name, exact: true }) });
  await expect(row("lv-2")).toContainText("464.32 of 500 GB left");
  await expect(row("lv-2")).toContainText("Out of date");
  await expect(row("al-1")).toContainText("Outline quota unavailable");
  await expect(row("lv-1")).not.toContainText("123 of 500 GB left");

  await page.getByRole("link", { name: "lv-2", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Upstream lv-2" })).toBeVisible();
  await expect(page.getByText("464.32 of 500 GB left")).toBeVisible();
  await page.getByRole("button", { name: "Language" }).click();
  await page.getByRole("menuitemradio", { name: "Русский" }).click();
  await expect(page.getByText(/Осталось 464,32 из 500\sГБ/)).toBeVisible();
});

test("the gateway totals and an upstream's page", async ({ page }) => {
  await page.goto("/admin");
  const totals = page.getByRole("region", { name: "Gateway" });
  await expect(totals).toContainText("Down now");
  await expect(totals).toContainText("since start");
  await expect(totals).toContainText("of 6 active");
  await expect(totals).toContainText("1 blocked");
  const before = await totals.textContent();
  await expect(totals).not.toHaveText(before!, { timeout: 5000 });

  await page.getByRole("link", { name: "lv-1" }).click();
  await expect(page).toHaveURL("/admin/upstreams/lv-1");
  await expect(page.getByRole("heading", { name: "Upstream lv-1" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Upstreams", exact: true })).toHaveAttribute("aria-current", "page");
  // The live chart and the three history charts.
  await expect(page.locator("figure canvas")).toHaveCount(4);
  await expect(page.getByRole("img", { name: /^Availability over 24 hours: \d/ })).toBeVisible();

  const peers = page.getByRole("region", { name: "Routed peers" });
  await expect(peers.getByRole("row")).toHaveCount(3); // header + alice's phone and laptop

  await page.getByRole("button", { name: "Range: 24 hours" }).click();
  await page.getByRole("menuitemradio", { name: "90 days" }).click();
  await expect(page.getByRole("img", { name: /^Availability over 90 days: \d/ })).toBeVisible();

  await page.goto("/admin/upstreams/xx-9");
  await expect(page.getByText("The gateway has no upstream of this name.")).toBeVisible();
});

test("the window menu works from the keyboard", async ({ page }) => {
  await page.goto("/admin");
  const trigger = page.getByRole("button", { name: "Window: 1 min" });
  await trigger.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("menuitemradio", { name: "1 min" })).toHaveAttribute("aria-checked", "true");
  await pressIn(page.getByRole("menu"), "ArrowDown");
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

test("chart and console follow a manual theme and language without losing the chart window", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/admin");
  await page.getByRole("button", { name: "Window: 1 min" }).click();
  await page.getByRole("menuitemradio", { name: "5 min", exact: true }).click();
  const gridPixels = (r: number, g: number, b: number) =>
    page.locator("figure canvas").evaluate((canvas: HTMLCanvasElement, rgb) => {
      const { data } = canvas.getContext("2d")!.getImageData(0, 0, canvas.width, canvas.height);
      for (let i = 0; i < data.length; i += 4) {
        if (data[i] === rgb[0] && data[i + 1] === rgb[1] && data[i + 2] === rgb[2]) return true;
      }
      return false;
    }, [r, g, b]);
  await expect.poll(() => gridPixels(209, 213, 219)).toBe(true);
  const theme = page.getByRole("button", { name: "Theme" });
  await expect(page.getByRole("button", { name: "Window: 5 min" })).toBeFocused();
  await theme.press("Enter");
  await expect(page.getByRole("menuitemradio", { name: "System" })).toHaveAttribute("aria-checked", "true");
  await pressIn(page.getByRole("menu"), "d");
  await page.keyboard.press("Enter");
  await expect(theme).toBeFocused();
  await expect(page.locator("body")).toHaveCSS("background-color", "rgb(11, 15, 23)");
  await expect.poll(() => gridPixels(51, 65, 85)).toBe(true);
  await page.getByRole("button", { name: "Language" }).click();
  await page.getByRole("menuitemradio", { name: "Русский" }).click();
  await expect(page.getByRole("region", { name: "Шлюз" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Окно: 5 мин" })).toBeVisible();
  const caption = page.locator("figcaption");
  await expect(caption).toContainText(/бит/);
  const before = await caption.innerText();
  await expect(caption).not.toHaveText(before);
});
