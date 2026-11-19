import { expect, test } from "./fixtures";

// A link as `lotsman user link` prints it; the fake daemon takes any token.
const link = "/u#q2V8Zp0nY3xW_4b-Lk9sTq1RmA7cJd5eHf6gUi0oPzE";

test("the user page shows live speed and devices on a phone", async ({ page }) => {
  await page.goto(link);
  await expect(page.getByRole("heading", { name: "Hello, alice" })).toBeVisible();

  const speed = page.getByRole("region", { name: "Speed now" });
  const before = await speed.textContent();
  await expect(speed).not.toHaveText(before!, { timeout: 5000 });

  const devices = page.getByRole("region", { name: "Devices" }).getByRole("heading", { level: 3 });
  await expect(devices).toHaveText(["phone", "laptop", "tv"]);

  // Only the exits the user's peers use, each with its 7-day availability.
  const exits = page.getByRole("region", { name: "Exits" });
  await expect(exits.getByRole("listitem")).toHaveCount(2);
  await expect(exits.getByRole("img", { name: /^Availability of lv-1 over 7 days: \d/ })).toBeVisible();
  await expect(exits.getByRole("img", { name: /^Availability of nl-1 over 7 days: \d/ })).toBeVisible();

  // Mobile-first: nothing scrolls sideways.
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});

test("the page tells the browser never to send the link as a referrer", async ({ page }) => {
  const response = await page.goto(link);
  expect(response?.headers()["referrer-policy"]).toBe("no-referrer");
  await expect(page.locator('meta[name="referrer"]')).toHaveAttribute("content", "no-referrer");
});

test("the link stays in the address for bookmarks and never reaches the server", async ({ page }) => {
  const requested: string[] = [];
  page.on("request", (r) => requested.push(r.url()));
  await page.goto(link);
  await expect(page.getByRole("heading", { name: "Hello, alice" })).toBeVisible();
  expect(page.url()).toMatch(/\/u#[A-Za-z0-9_-]{43}$/);
  expect(requested.filter((url) => url.includes("q2V8Zp0nY3xW"))).toEqual([]);
});

for (const [name, path] of [
  ["missing", "/u"],
  ["malformed", "/u#not-a-token"],
] as const) {
  test(`a ${name} link says so`, async ({ page }) => {
    await page.goto(path);
    await expect(page.getByRole("alert")).toHaveText(/link is missing or invalid/);
    await expect(page.getByRole("heading", { name: "Hello, alice" })).toHaveCount(0);
  });
}
