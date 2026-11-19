import { FAKE_LINKS } from "../src/api/fakeSelf";
import { expect, test } from "./fixtures";

// A link as `lotsman user link` prints it; the fake daemon takes any token as
// the ordinary user and gives a few well-known ones a scenario of their own.
const link = "/u#q2V8Zp0nY3xW_4b-Lk9sTq1RmA7cJd5eHf6gUi0oPzE";
const open = (token: string) => `/u#${token}`;

test("the user page shows live speed, devices and exits", async ({ page }) => {
  await page.goto(link);
  await expect(page.getByRole("heading", { name: "Hello, alice" })).toBeVisible();

  const speed = page.getByRole("region", { name: "Speed now" });
  const before = await speed.textContent();
  await expect(speed).not.toHaveText(before!, { timeout: 5000 });

  await expect(page.locator("#devices ~ ul > li > div h3")).toHaveText(["phone", "laptop", "tv"]);

  // Only the exits the user's peers use, each with its 7-day availability and latency.
  const exits = page.getByRole("region", { name: "Exits" });
  await expect(exits.getByRole("listitem")).toHaveCount(2);
  await expect(exits.getByRole("img", { name: /^Availability of lv-1 over 7 days: \d/ })).toBeVisible();
  await expect(exits.getByRole("img", { name: /^Availability of nl-1 over 7 days: \d/ })).toBeVisible();
  await expect(exits).toContainText("Used by phone, laptop");
  await expect(exits).toContainText("Latency:");

  // Mobile-first: nothing scrolls sideways.
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});

test("every control is a large touch target", async ({ page }) => {
  await page.goto(link);
  await expect(page.getByRole("heading", { name: "Hello, alice" })).toBeVisible();
  const controls = page.locator("main").locator("button, summary, input, select");
  const heights = await controls.evaluateAll((els) => els.map((el) => [el.textContent?.trim() || el.id, el.getBoundingClientRect().height]));
  expect(heights.length).toBeGreaterThan(6);
  for (const [name, height] of heights) expect(height, String(name)).toBeGreaterThanOrEqual(43.5);
});

test("a device's config shows as a large QR code, a file and text", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto(link);
  await page.getByRole("button", { name: "Config and QR code phone, default" }).click();

  const qr = page.getByRole("img", { name: "QR code of the config for phone, profile default" });
  await expect(qr).toBeVisible();
  const box = (await qr.boundingBox())!;
  expect(box.width).toBeGreaterThanOrEqual(280);
  expect(Math.abs(box.width - box.height)).toBeLessThan(1);
  expect(await qr.locator("path").getAttribute("d")).toMatch(/^M\d+ \d+L/);
  // Dark modules on white with a 4-module quiet zone, in either theme.
  expect(await qr.locator("rect").getAttribute("fill")).toBe("#fff");
  expect(await qr.getAttribute("viewBox")).toMatch(/^-4 -4 (\d+) \1$/);

  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download .conf" }).click()]);
  expect(download.suggestedFilename()).toBe("phone-default.conf");
  const { createReadStream } = await import("node:fs");
  const chunks: Buffer[] = [];
  for await (const chunk of createReadStream((await download.path())!)) chunks.push(chunk as Buffer);
  const file = Buffer.concat(chunks).toString();
  expect(file).toMatch(/^\[Interface\]\nPrivateKey = /);

  await page.getByRole("button", { name: "Copy config" }).click();
  await expect(page.getByRole("status")).toHaveText("Copied");
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(file);

  await page.getByRole("button", { name: "Hide config phone, default" }).click();
  await expect(qr).toBeHidden();
});

test("adding a device shows its config and QR code at once", async ({ page }) => {
  await page.goto(link);
  await page.getByLabel("Device name").fill("tablet");
  await page.getByLabel("Profile", { exact: true }).selectOption("streaming");
  await page.getByRole("button", { name: "Add device" }).click();

  const issued = page.getByRole("region", { name: "tablet added (streaming)" });
  await expect(issued).toBeVisible();
  await expect(issued.getByRole("img", { name: "QR code of the config for tablet, profile streaming" })).toBeVisible();
  await expect(issued.getByRole("heading")).toBeFocused();
  await expect(page.locator("#devices ~ ul > li > div h3")).toHaveText(["phone", "laptop", "tv", "tablet"]);
  await expect(page.getByLabel("Device name")).toHaveValue("");

  const [download] = await Promise.all([page.waitForEvent("download"), issued.getByRole("button", { name: "Download .conf" }).click()]);
  expect(download.suggestedFilename()).toBe("tablet-streamin.conf") // tunnel names are at most 15 characters;

  await issued.getByRole("button", { name: "Done" }).click();
  await expect(issued).toBeHidden();
});

test("removing a device asks first", async ({ page }) => {
  await page.goto(link);
  const devices = page.locator("#devices ~ ul > li > div h3");
  await expect(devices).toHaveCount(3);

  await page.getByRole("button", { name: "Remove tv" }).click();
  const dialog = page.getByRole("dialog", { name: "Remove tv?" });
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toBeHidden();
  await expect(devices).toHaveCount(3);

  await page.getByRole("button", { name: "Remove tv" }).click();
  await page.getByRole("dialog", { name: "Remove tv?" }).getByRole("button", { name: "Remove device" }).click();
  await expect(dialog).toBeHidden();
  await expect(devices).toHaveText(["phone", "laptop"]);
  // Its exit is no longer used by anyone of ours.
  await expect(page.getByRole("region", { name: "Exits" }).getByRole("listitem")).toHaveCount(1);
});

test("a profile change adds the new profile, shows its config, and drops the old one", async ({ page }) => {
  await page.goto(link);
  const laptop = page.locator("#devices ~ ul > li", { hasText: "laptop" });
  await expect(laptop).toContainText("Profile: default");
  await laptop.getByRole("button", { name: "Change profile laptop, default" }).click();
  const dialog = page.getByRole("dialog", { name: "Change the profile of laptop (now default)" });
  await dialog.getByLabel("New profile").selectOption("streaming");
  await dialog.getByRole("button", { name: "Change profile" }).click();

  const issued = page.getByRole("region", { name: "laptop now uses streaming" });
  await expect(issued.getByRole("img", { name: "QR code of the config for laptop, profile streaming" })).toBeVisible();
  await expect(laptop).toContainText("Profile: streaming");
  await expect(laptop).not.toContainText("Profile: default");
});

test("the device limit is explained, and removing a device lifts it", async ({ page }) => {
  await page.goto(link);
  for (const name of ["d4", "d5"]) {
    await page.getByLabel("Device name").fill(name);
    await page.getByRole("button", { name: "Add device" }).click();
    await expect(page.getByRole("region", { name: `${name} added (default)` })).toBeVisible();
  }
  await page.getByLabel("Device name").fill("d6");
  await page.getByRole("button", { name: "Add device" }).click();
  await expect(page.getByRole("alert")).toHaveText("You already have the most devices allowed (5). Remove one first, or ask the operator for more.");
  await expect(page.locator("#devices ~ ul > li > div h3")).toHaveCount(5);

  await page.getByRole("button", { name: "Remove d5" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Remove device" }).click();
  await page.getByRole("button", { name: "Add device" }).click();
  await expect(page.getByRole("region", { name: "d6 added (default)" })).toBeVisible();
});

test("a user whose adding is turned off is told so", async ({ page }) => {
  await page.goto(open(FAKE_LINKS.noAdding));
  await page.getByLabel("Device name").fill("tablet");
  await page.getByRole("button", { name: "Add device" }).click();
  await expect(page.getByRole("alert")).toHaveText("Adding devices is turned off. Ask the operator.");
  await expect(page.getByLabel("Device name")).toBeDisabled();
  await expect(page.getByRole("button", { name: "Add device" })).toBeDisabled();
  // They can still see and remove what they have.
  await expect(page.getByRole("button", { name: "Remove phone" })).toBeVisible();
});

test("too many additions are told apart from the device limit", async ({ page }) => {
  await page.goto(open(FAKE_LINKS.ordinary));
  // The fake allows ten additions at once; adding and removing the same device keeps the limit out of the way.
  for (let i = 0; i < 10; i++) {
    await page.getByLabel("Device name").fill("loop");
    await page.getByRole("button", { name: "Add device" }).click();
    await expect(page.getByRole("region", { name: "loop added (default)" })).toBeVisible();
    await page.getByRole("button", { name: "Done" }).click();
    await page.getByRole("button", { name: "Remove loop" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Remove device" }).click();
    await expect(page.getByRole("button", { name: "Remove loop" })).toHaveCount(0);
  }
  await page.getByLabel("Device name").fill("loop");
  await page.getByRole("button", { name: "Add device" }).click();
  await expect(page.getByRole("alert")).toHaveText("You added too many devices lately. Try again in a few minutes.");
});

test("a revoked link says it no longer works", async ({ page }) => {
  await page.goto(open(FAKE_LINKS.revoked));
  await expect(page.getByRole("alert")).toHaveText("This link no longer works. Ask the operator for a new one.");
  await expect(page.getByRole("heading", { name: /Hello/ })).toHaveCount(0);
  await expect(page.getByRole("button")).toHaveCount(0);
});

test("a user without devices is invited to add one", async ({ page }) => {
  await page.goto(open(FAKE_LINKS.empty));
  await expect(page.getByRole("heading", { name: "Hello, dave" })).toBeVisible();
  await expect(page.getByText("You have no devices yet")).toBeVisible();
  await expect(page.getByRole("group", { name: "Period" })).toHaveCount(0);
  await expect(page.getByText("How to import a config")).toBeVisible();
  await page.getByLabel("Device name").fill("phone");
  await page.getByRole("button", { name: "Add device" }).click();
  await expect(page.getByRole("img", { name: "QR code of the config for phone, profile default" })).toBeVisible();
});

test("an unreachable daemon can be retried", async ({ page }) => {
  await page.goto(open(FAKE_LINKS.unavailable));
  await expect(page.getByRole("alert")).toContainText("The gateway cannot be reached right now");
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
});

test("the history covers a day or a week of the user's own traffic", async ({ page }) => {
  await page.goto(link);
  const chart = page.getByRole("img", { name: /^Your traffic over 24 hours: .+ received, .+ sent$/ });
  await expect(chart).toBeVisible();
  await expect(page.getByRole("button", { name: "24 hours" })).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("button", { name: "7 days" }).click();
  await expect(page.getByRole("img", { name: /^Your traffic over 7 days: / })).toBeVisible();
  await expect(page.getByRole("button", { name: "7 days" })).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByRole("heading", { name: "By device" })).toBeVisible();
});

test("the page is in Russian for a Russian browser", async ({ browser, baseURL }) => {
  const context = await browser.newContext({ baseURL, locale: "ru-RU" });
  const page = await context.newPage();
  await page.goto(link);
  await expect(page.getByRole("heading", { name: "Привет, alice" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Конфиг и QR-код phone, default" })).toBeVisible();
  await expect(page.getByText("Как загрузить конфиг")).toBeVisible();
  await context.close();
});

test("the page tells the browser never to send the link as a referrer", async ({ page }) => {
  const response = await page.goto(link);
  expect(response?.headers()["referrer-policy"]).toBe("no-referrer");
  await expect(page.locator('meta[name="referrer"]')).toHaveAttribute("content", "no-referrer");
});

test("the link stays in the address for bookmarks and nowhere else", async ({ page }) => {
  const requested: string[] = [];
  page.on("request", (r) => requested.push(r.url()));
  await page.goto(link);
  await page.getByRole("button", { name: "Config and QR code phone, default" }).click();
  await expect(page.getByRole("img", { name: /^QR code/ })).toBeVisible();
  await page.getByLabel("Device name").fill("tablet");
  await page.getByRole("button", { name: "Add device" }).click();
  await expect(page.getByRole("region", { name: "tablet added (default)" })).toBeVisible();

  expect(page.url()).toMatch(/\/u#[A-Za-z0-9_-]{43}$/);
  const token = link.split("#")[1]!;
  expect(requested.length).toBeGreaterThan(1);
  expect(requested.filter((url) => url.includes(token) || url.includes(token.slice(0, 12)))).toEqual([]);

  // Memory only: not in storage, not in cookies.
  const stored = await page.evaluate(() => JSON.stringify([{ ...localStorage }, { ...sessionStorage }, document.cookie]));
  expect(stored).not.toContain(token);
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
