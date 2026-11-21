import { FAKE_LINKS } from "../src/api/fakeSelf";
import type { Page } from "@playwright/test";
import { expect, test } from "./fixtures";

// A link as `lotsman user link` prints it; the fake daemon takes any token as
// the ordinary user and gives a few well-known ones a scenario of their own.
const link = "/u#q2V8Zp0nY3xW_4b-Lk9sTq1RmA7cJd5eHf6gUi0oPzE";
const open = (token: string) => `/u#${token}`;

/** The new-device form, opened first when the user has devices and it waits behind its button. */
async function addForm(page: Page) {
  const button = page.getByRole("button", { name: "Add a device" });
  if (await button.isVisible()) await button.click();
  return page.getByRole("region", { name: "New device" });
}

/** A device's card on the user page. */
const card = (page: Page, device: string) => page.locator("#devices ~ ul > li").filter({ has: page.getByRole("heading", { name: device, exact: true }) });

test("the user page shows live speed, devices, their exits now and the exits in use", async ({ page }) => {
  await page.goto(link);
  await expect(page.getByRole("heading", { name: "Hello, alice" })).toBeVisible();

  const speed = page.getByRole("region", { name: "Speed now" });
  const before = await speed.textContent();
  await expect(speed).not.toHaveText(before!, { timeout: 5000 });

  await expect(page.locator("#devices ~ ul > li > div h3")).toHaveText(["phone", "laptop", "tv"]);
  await expect(card(page, "phone")).toContainText("Now: Latvia · provider-a");
  await expect(card(page, "tv")).toContainText("Now: Latvia · provider-b");

  // Every allowed exit, by country and provider, each with its 7-day availability, latency and the devices using it.
  const exits = page.getByRole("region", { name: "Exits" });
  await expect(exits.getByRole("listitem")).toHaveCount(5);
  await expect(exits.getByRole("img", { name: /^Availability of Latvia · provider-a over 7 days: \d/ })).toBeVisible();
  await expect(exits.getByRole("img", { name: /^Availability of Latvia · provider-b over 7 days: \d/ })).toBeVisible();
  await expect(exits).toContainText("Used by phone, laptop");
  await expect(exits.getByRole("listitem").filter({ hasText: "Albania" })).toContainText("No device uses it now");
  await expect(exits).toContainText("Latency:");
  await expect(exits).not.toContainText("lv-1"); // never the upstream's name

  // Mobile-first: nothing scrolls sideways.
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});

test("every control is a large touch target", async ({ page }) => {
  await page.goto(link);
  await expect(page.getByRole("heading", { name: "Hello, alice" })).toBeVisible();
  const controls = page.locator("main button, main summary, main input, main select, header select");
  const heights = await controls.evaluateAll((els) =>
    els.map((el) => [el.getAttribute("aria-label") || el.textContent?.trim() || el.id, el.getBoundingClientRect().height]),
  );
  expect(heights.length).toBeGreaterThan(30); // the route editors' switches, chips and handles among them
  for (const [name, height] of heights) expect(height, String(name)).toBeGreaterThanOrEqual(43.5);
});

test("a device's config shows as a large QR code, a file and text", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto(link);
  await page.getByRole("button", { name: "Config and QR code phone" }).click();

  const qr = page.getByRole("img", { name: "QR code of the config for phone" });
  await expect(qr).toBeVisible();
  const box = (await qr.boundingBox())!;
  expect(box.width).toBeGreaterThanOrEqual(280);
  expect(Math.abs(box.width - box.height)).toBeLessThan(1);
  expect(await qr.locator("path").getAttribute("d")).toMatch(/^M\d+ \d+L/);
  // Dark modules on white with a 4-module quiet zone, in either theme.
  expect(await qr.locator("rect").getAttribute("fill")).toBe("#fff");
  expect(await qr.getAttribute("viewBox")).toMatch(/^-4 -4 (\d+) \1$/);

  // Chromium keeps the suggested name even for text/plain, but some Safari
  // versions use the MIME type to add .txt to a .conf download.
  await page.evaluate(() => {
    const create = URL.createObjectURL;
    URL.createObjectURL = (object) => {
      if (object instanceof Blob) document.documentElement.dataset.configDownloadType = object.type;
      return create.call(URL, object);
    };
  });
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download .conf" }).click()]);
  expect(download.suggestedFilename()).toBe("phone.conf");
  await expect(page.locator("html")).toHaveAttribute("data-config-download-type", "application/octet-stream");
  const { createReadStream } = await import("node:fs");
  const chunks: Buffer[] = [];
  for await (const chunk of createReadStream((await download.path())!)) chunks.push(chunk as Buffer);
  const file = Buffer.concat(chunks).toString();
  expect(file).toMatch(/^\[Interface\]\nPrivateKey = /);

  await page.getByRole("button", { name: "Copy config" }).click();
  await expect(page.getByRole("status")).toHaveText("Copied");
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(file);

  await page.getByRole("button", { name: "Hide config phone" }).click();
  await expect(qr).toBeHidden();
});

test("a route reorders from the keyboard and applies without a new config", async ({ page }) => {
  await page.goto(link);
  const phone = card(page, "phone");
  const apply = phone.getByRole("button", { name: "Apply" });
  await expect(phone.getByRole("button", { name: "In order" })).toHaveAttribute("aria-pressed", "true");
  await expect(apply).toBeDisabled();

  // Albania joins last; two presses of the up arrow make it first, and each move is announced.
  await phone.getByRole("switch", { name: "Use Albania" }).click();
  await expect(phone.getByRole("switch", { name: "Use Albania" })).toHaveAttribute("aria-checked", "true");
  await phone.getByRole("button", { name: "Albania, 3 of 3" }).focus();
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("ArrowUp");
  await expect(phone.getByRole("button", { name: "Albania, 1 of 3" })).toBeFocused();
  await expect(phone.getByText("Albania moved to 1 of 3")).toBeAttached();
  await expect(phone.getByText("The config does not change, the tunnel does not reconnect.")).toBeVisible();

  const requests: string[] = [];
  page.on("request", (r) => requests.push(r.url()));
  await apply.click();
  await expect(phone).toContainText("Now: Albania · provider-b");
  await expect(apply).toBeDisabled(); // stored: nothing left to apply
  expect(requests.some((url) => url.includes("GetConfig"))).toBe(false);
});

test("a route reorders by dragging its handle", async ({ page }) => {
  await page.goto(link);
  const phone = card(page, "phone");
  const handle = phone.getByRole("button", { name: "Netherlands, 2 of 2" });
  await handle.scrollIntoViewIfNeeded();
  const target = (await phone.getByRole("button", { name: "Latvia, 1 of 2" }).boundingBox())!;
  const from = (await handle.boundingBox())!;
  await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
  await page.mouse.down();
  await page.mouse.move(target.x + target.width / 2, target.y, { steps: 5 });
  await page.mouse.up();
  await expect(phone.getByRole("button", { name: "Netherlands, 1 of 2" })).toBeVisible();
  await expect(phone.getByRole("button", { name: "Apply" })).toBeEnabled();
  await phone.getByRole("button", { name: "Cancel" }).click();
  await expect(phone.getByRole("button", { name: "Latvia, 1 of 2" })).toBeVisible();
});

test("the fastest mode, providers, and the strict switch", async ({ page }) => {
  await page.goto(link);
  const tv = card(page, "tv");
  await expect(tv.getByRole("button", { name: "Fastest" })).toHaveAttribute("aria-pressed", "true");
  await expect(tv.getByText("Of the checked countries, the one with the lowest latency now.")).toBeVisible();
  const latvia = tv.getByRole("group", { name: "Providers in Latvia" });
  await expect(latvia.getByRole("button", { name: "provider-a" })).toHaveAttribute("aria-pressed", "false");
  await expect(latvia.getByRole("button", { name: "provider-b" })).toHaveAttribute("aria-pressed", "true");
  await latvia.getByRole("button", { name: "provider-a" }).click();
  await tv.getByRole("button", { name: "Apply" }).click();
  await expect(tv).toContainText("Now: Latvia · provider-a"); // now the faster of the two
  await expect(tv.getByRole("switch", { name: "Only the selected countries" })).toHaveAttribute("aria-checked", "true");
});

test("adding a device shows its config and QR code at once, with the route chosen", async ({ page }) => {
  await page.goto(link);
  // With devices there, the form waits behind its button.
  await expect(page.getByRole("region", { name: "New device" })).toHaveCount(0);
  const add = await addForm(page);
  await add.getByLabel("Name").fill("tablet");
  // The route starts as any exit, the fastest; this one is Latvia only.
  await expect(add.getByRole("button", { name: "Fastest" })).toHaveAttribute("aria-pressed", "true");
  await add.getByRole("button", { name: "In order" }).click();
  for (const country of ["Albania", "Germany", "Netherlands"]) await add.getByRole("switch", { name: `Use ${country}` }).click();
  await add.getByRole("button", { name: "Create" }).click();

  const issued = page.getByRole("region", { name: "tablet added" });
  await expect(issued).toBeVisible();
  await expect(issued.getByRole("img", { name: "QR code of the config for tablet" })).toBeVisible();
  await expect(issued.getByRole("heading")).toBeFocused();
  await expect(page.locator("#devices ~ ul > li > div h3")).toHaveText(["phone", "laptop", "tv", "tablet"]);
  await expect(add).toHaveCount(0); // closed again
  await expect(page.getByRole("button", { name: "Add a device" })).toBeVisible();
  const tablet = card(page, "tablet");
  await expect(tablet.getByRole("button", { name: "Latvia, 1 of 1" })).toBeVisible();
  await expect(tablet).toContainText("Now: Latvia · provider-a");

  const [download] = await Promise.all([page.waitForEvent("download"), issued.getByRole("button", { name: "Download .conf" }).click()]);
  expect(download.suggestedFilename()).toBe("tablet.conf");

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
  // Its exit stays listed, now used by none of ours.
  await expect(page.getByRole("region", { name: "Exits" }).getByRole("listitem").filter({ hasText: "Latvia · provider-b" })).toContainText("No device uses it now");
});

test("a blocked device says so, and turning on another country lifts it", async ({ page }) => {
  await page.goto(open(FAKE_LINKS.countriesOnly));
  const phone = card(page, "phone");
  await expect(phone).toContainText("No exit");
  await expect(phone).toContainText("Now: traffic blocked");
  await expect(phone.getByRole("alert")).toHaveText(
    "No available exit. The selected countries are unavailable now, so traffic is blocked. Turn on another country or turn off “Only the selected countries”.",
  );
  await expect(phone.getByText("unavailable", { exact: true })).toBeVisible();
  // Countries only: no provider anywhere on the page.
  await expect(page.getByRole("group", { name: /^Providers in/ })).toHaveCount(0);
  await expect(page.locator("main")).not.toContainText("provider-");

  await phone.getByRole("switch", { name: "Use Netherlands" }).click();
  await expect(phone.getByRole("alert")).toHaveCount(0);
  await phone.getByRole("button", { name: "Apply" }).click();
  await expect(phone).toContainText("Now: Netherlands");
  await expect(phone).toContainText("Degraded");
});

test("a user whose routes the operator sets sees them read-only", async ({ page }) => {
  await page.goto(open(FAKE_LINKS.fixedRoutes));
  const phone = card(page, "phone");
  await expect(phone.getByText("Only the operator changes this route.")).toBeVisible();
  await expect(phone.getByRole("list", { name: "Countries" }).getByRole("listitem")).toHaveText([/^1\.Albania/]);
  await expect(phone.getByRole("switch")).toHaveCount(0);
  await expect(phone.getByRole("button", { name: "Apply" })).toHaveCount(0);
  // A new device gets the default route: no choice to make.
  await expect((await addForm(page)).getByRole("group")).toHaveCount(0);
  // Only his two exits exist for him.
  await expect(card(page, "laptop").getByRole("list", { name: "Countries" }).getByRole("listitem")).toHaveCount(2);
});

test("the device limit is explained, and removing a device lifts it", async ({ page }) => {
  await page.goto(link);
  const add = page.getByRole("region", { name: "New device" });
  for (const name of ["d4", "d5"]) {
    await addForm(page);
    await add.getByLabel("Name").fill(name);
    await add.getByRole("button", { name: "Create" }).click();
    await expect(page.getByRole("region", { name: `${name} added` })).toBeVisible();
  }
  await addForm(page);
  await add.getByLabel("Name").fill("d6");
  await add.getByRole("button", { name: "Create" }).click();
  await expect(add.getByRole("alert")).toHaveText("You already have the most devices allowed (5). Remove one first, or ask the operator for more.");
  await expect(page.locator("#devices ~ ul > li > div h3")).toHaveCount(5);

  await page.getByRole("button", { name: "Remove d5" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Remove device" }).click();
  await add.getByRole("button", { name: "Create" }).click();
  await expect(page.getByRole("region", { name: "d6 added" })).toBeVisible();
});

test("a user whose adding is turned off is told so", async ({ page }) => {
  await page.goto(open(FAKE_LINKS.noAdding));
  const add = await addForm(page);
  await add.getByLabel("Name").fill("tablet");
  await add.getByRole("button", { name: "Create" }).click();
  await expect(add.getByRole("alert")).toHaveText("Adding devices is turned off. Ask the operator.");
  await expect(add.getByLabel("Name")).toBeDisabled();
  await expect(add.getByRole("button", { name: "Create" })).toBeDisabled();
  // They can still see and remove what they have.
  await expect(page.getByRole("button", { name: "Remove phone" })).toBeVisible();
});

test("too many additions are told apart from the device limit", async ({ page }) => {
  await page.goto(open(FAKE_LINKS.ordinary));
  const add = page.getByRole("region", { name: "New device" });
  // The fake allows ten additions at once; adding and removing the same device keeps the limit out of the way.
  for (let i = 0; i < 10; i++) {
    await addForm(page);
    await add.getByLabel("Name").fill("loop");
    await add.getByRole("button", { name: "Create" }).click();
    await expect(page.getByRole("region", { name: "loop added" })).toBeVisible();
    await page.getByRole("button", { name: "Done" }).click();
    await page.getByRole("button", { name: "Remove loop" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Remove device" }).click();
    await expect(page.getByRole("button", { name: "Remove loop" })).toHaveCount(0);
  }
  await addForm(page);
  await add.getByLabel("Name").fill("loop");
  await add.getByRole("button", { name: "Create" }).click();
  await expect(add.getByRole("alert")).toHaveText("You added too many devices lately. Try again in 6 min.");
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
  await page.getByLabel("Name").fill("phone");
  await page.getByRole("button", { name: "Create" }).click();
  await expect(page.getByRole("img", { name: "QR code of the config for phone" })).toBeVisible();
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
  await expect(page.getByRole("button", { name: "Конфиг и QR-код phone" })).toBeVisible();
  await expect(card(page, "phone").getByRole("button", { name: "По порядку" })).toHaveAttribute("aria-pressed", "true");
  await expect(card(page, "phone")).toContainText("Сейчас: Латвия · provider-a");
  await expect(card(page, "phone").getByRole("switch", { name: "Только выбранные страны" })).toBeVisible();
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
  await page.getByRole("button", { name: "Config and QR code phone" }).click();
  await expect(page.getByRole("img", { name: /^QR code/ })).toBeVisible();
  await (await addForm(page)).getByLabel("Name").fill("tablet");
  await page.getByRole("button", { name: "Create" }).click();
  await expect(page.getByRole("region", { name: "tablet added" })).toBeVisible();

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

test("display preferences preserve a draft and the private link, and carry over to the console", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });
  await page.goto(link);
  const phone = card(page, "phone");
  await phone.getByRole("button", { name: "Fastest" }).click();
  await page.getByRole("button", { name: "7 days" }).click();
  await (await addForm(page)).getByLabel("Name").fill("tablet");

  await page.getByRole("combobox", { name: "Theme" }).selectOption("light");
  await expect(page.locator("body")).toHaveCSS("background-color", "rgb(245, 246, 248)");
  await page.getByRole("combobox", { name: "Language" }).selectOption("ru");
  await expect(page.locator("html")).toHaveAttribute("lang", "ru");
  await expect(page.getByRole("heading", { name: "Привет, alice" })).toBeVisible();
  await expect(phone.getByRole("button", { name: "Самая быстрая" })).toHaveAttribute("aria-pressed", "true");
  await expect(phone.getByRole("button", { name: "Применить" })).toBeEnabled();
  await expect(phone).toContainText("Латвия");
  await expect(page.getByRole("button", { name: "7 дней" })).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator("#device-name")).toHaveValue("tablet");
  await expect(page).toHaveURL(link);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

  await page.reload();
  await expect(page.getByRole("heading", { name: "Привет, alice" })).toBeVisible();
  await expect(page).toHaveURL(link);
  await expect(page.getByRole("combobox", { name: "Язык" })).toHaveValue("ru");
  await page.goto("/admin");
  await expect(page.getByRole("heading", { name: "Выходы" })).toBeVisible();
  await expect(page.locator("body")).toHaveCSS("background-color", "rgb(245, 246, 248)");
  await expect(page.getByRole("combobox", { name: "Тема" })).toHaveValue("light");
  await page.getByRole("combobox", { name: "Тема" }).selectOption("system");
  await expect(page.locator("body")).toHaveCSS("background-color", "rgb(11, 15, 23)");
});

for (const storage of ["invalid", "unavailable"] as const) {
  test(`display settings still work when browser storage is ${storage}`, async ({ page }) => {
    await page.addInitScript((state) => {
      if (state === "invalid") {
        localStorage.setItem("lotsman.language", "constructor");
        localStorage.setItem("lotsman.theme", "invalid");
      } else {
        Object.defineProperty(window, "localStorage", {
          get() { throw new DOMException("Storage is blocked", "SecurityError"); },
        });
      }
    }, storage);
    await page.emulateMedia({ colorScheme: "dark" });
    await page.goto("/u");
    await expect(page.getByRole("alert")).toContainText("link is missing or invalid");
    await expect(page.locator("body")).toHaveCSS("background-color", "rgb(11, 15, 23)");
    await page.getByRole("combobox", { name: "Language" }).selectOption("ru");
    await page.getByRole("combobox", { name: "Тема" }).selectOption("light");
    await expect(page.locator("html")).toHaveAttribute("lang", "ru");
    await expect(page.locator("body")).toHaveCSS("background-color", "rgb(245, 246, 248)");
  });
}
