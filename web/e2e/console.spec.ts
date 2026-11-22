import type { Page } from "@playwright/test";
import { expect, pressIn, test } from "./fixtures";

/** The console fits the window: tables scroll inside their frames, the page never sideways. */
async function noSidewaysScroll(page: Page) {
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
}

test("every page is reachable from the navigation", async ({ page }) => {
  await page.goto("/admin");
  const nav = page.getByRole("navigation", { name: "Operator" });
  for (const [name, path, heading] of [
    ["Users", "/admin/users", "Users"],
    ["Connections", "/admin/connections", "Connections"],
    ["Incidents", "/admin/incidents", "Incidents"],
    ["Passkeys", "/admin/passkeys", "Passkeys"],
    ["Upstreams", "/admin", "Upstreams"],
  ] as const) {
    await nav.getByRole("link", { name }).click();
    await expect(page).toHaveURL(path);
    await expect(page.getByRole("heading", { name: heading, level: 1 })).toBeVisible();
    await expect(nav.getByRole("link", { name })).toHaveAttribute("aria-current", "page");
    await noSidewaysScroll(page);
  }
});

test("add a user and a device, and see its config and QR code", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/admin/users");
  const list = page.getByRole("table");
  await expect(list.getByRole("row")).toHaveCount(4); // header + alice, bob, carol
  await expect(list.getByRole("row", { name: /^bob/ })).toContainText("operator sets routes · 2 exits");

  await page.getByRole("button", { name: "Add user" }).click();
  const addUser = page.getByRole("dialog", { name: "Add a user" });
  await addUser.getByLabel("Name").fill("eve");
  await addUser.getByRole("button", { name: "Add", exact: true }).click();
  await expect(page).toHaveURL("/admin/users/eve");
  await expect(page.getByRole("heading", { name: "eve", level: 1 })).toBeVisible();
  await expect(page.getByRole("link", { name: "Users", exact: true }).first()).toHaveAttribute("aria-current", "page");
  await expect(page.getByText("No devices.")).toBeVisible();

  await page.getByRole("button", { name: "Add device" }).click();
  const dialog = page.getByRole("dialog", { name: "Add a device for eve" });
  await dialog.getByLabel("Device").fill("phone");
  await dialog.getByRole("button", { name: "Add", exact: true }).click();

  const config = page.getByRole("dialog", { name: "Config of eve/phone" });
  await expect(config.getByLabel("Config file")).toHaveValue(/^\[Interface\]\nPrivateKey = .+\nAddress = 10\.8\.0\.8\/32\n/);
  const qr = config.getByRole("img", { name: "QR code of the config of eve/phone" });
  await expect(qr).toBeVisible();
  expect(await qr.evaluate((svg) => svg.tagName + (svg.querySelector("path")?.getAttribute("d")?.length ?? 0))).toMatch(/^svg\d{4,}$/);

  await config.getByRole("button", { name: "Copy" }).click();
  await expect(config.getByRole("button", { name: "Copied" })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toMatch(/^\[Interface\]/);
  const [file] = await Promise.all([page.waitForEvent("download"), config.getByRole("button", { name: "Download eve-phone.conf" }).click()]);
  expect(file.suggestedFilename()).toBe("eve-phone.conf");
  await pressIn(config, "Escape");
  await expect(config).toBeHidden();

  // The device is listed with the default route, and its config can be shown again.
  const devices = page.getByRole("region", { name: "Devices" });
  await expect(devices.getByRole("row")).toHaveCount(2);
  await expect(devices.getByRole("row", { name: /^phone/ })).toContainText("Any, fastest");
  await expect(devices.getByRole("row", { name: /^phone/ })).toContainText("lv-1");
  await devices.getByRole("button", { name: "Config eve/phone" }).click();
  const again = page.getByRole("dialog", { name: "Config of eve/phone" });
  await expect(again.getByLabel("Config file")).toHaveValue(/Address = 10\.8\.0\.8\/32/);
  await pressIn(again, "Escape");
  await expect(again).toBeHidden();

  // The same device twice is refused, with the daemon's reason.
  await page.getByRole("button", { name: "Add device" }).click();
  const twice = page.getByRole("dialog", { name: "Add a device for eve" });
  await twice.getByLabel("Device").fill("phone");
  await twice.getByRole("button", { name: "Add", exact: true }).click();
  await expect(twice.getByRole("alert")).toHaveText(/device eve\/phone already exists/);
  await noSidewaysScroll(page);
});

test("edit a device's route from the console", async ({ page }) => {
  await page.goto("/admin/users/alice");
  const devices = page.getByRole("region", { name: "Devices" });
  await expect(devices.getByRole("row", { name: /^tv/ })).toContainText("Fastest: Latvia (provider-b), Netherlands · strict");
  await expect(page.getByRole("region", { name: "Route of phone" })).toBeVisible(); // the first device's, at first

  await devices.getByRole("button", { name: "Route tv" }).click();
  await expect(devices.getByRole("button", { name: "Route tv" })).toHaveAttribute("aria-pressed", "true");
  const editor = page.getByRole("region", { name: "Route of tv" });
  await editor.getByRole("button", { name: "In order" }).click();
  await editor.getByRole("button", { name: "Netherlands, 2 of 2" }).focus();
  await page.keyboard.press("ArrowUp");
  await editor.getByRole("button", { name: "Apply" }).click();
  await expect(devices.getByRole("row", { name: /^tv/ })).toContainText("Netherlands → Latvia (provider-b) · strict");
  // The Netherlands is degraded, a last resort: Latvia's provider-b is up, so tv stays on lv-2.
  await expect(devices.getByRole("row", { name: /^tv/ })).toContainText("lv-2");
  await expect(editor.getByRole("button", { name: "Apply" })).toBeDisabled();
  await noSidewaysScroll(page);
});

test("a user's rights: who sets routes, the allowed exits, providers", async ({ page }) => {
  await page.goto("/admin/users/alice");
  const rights = page.getByRole("region", { name: "Rights" });
  const editRoutes = rights.getByRole("switch", { name: "Changes routes themselves" });
  await expect(editRoutes).toHaveAttribute("aria-checked", "true");
  await editRoutes.click();
  await expect(editRoutes).toHaveAttribute("aria-checked", "false");

  // Every exit is allowed by default; taking one away takes its country out of the route editor.
  const exits = rights.getByRole("group", { name: "Allowed exits" });
  await expect(exits.getByRole("button")).toHaveText(["lv-1", "lv-2", "al-1", "nl-1", "de-1"]);
  for (const name of ["lv-1", "lv-2", "al-1", "nl-1", "de-1"]) await expect(exits.getByRole("button", { name })).toHaveAttribute("aria-pressed", "true");
  const editor = page.getByRole("region", { name: "Route of phone" });
  await expect(editor.getByRole("switch", { name: "Use Germany" })).toBeVisible();
  await exits.getByRole("button", { name: "de-1" }).click();
  await expect(exits.getByRole("button", { name: "de-1" })).toHaveAttribute("aria-pressed", "false");
  await expect(editor.getByRole("switch", { name: "Use Germany" })).toHaveCount(0);

  const providers = rights.getByRole("switch", { name: "Show providers" });
  await providers.click();
  await expect(providers).toHaveAttribute("aria-checked", "false");

  await page.getByRole("navigation", { name: "Operator" }).getByRole("link", { name: "Users" }).click();
  await expect(page.getByRole("row", { name: /^alice/ })).toContainText("operator sets routes · 4 exits · countries only");
});

test("issue a link, see it once in its /u# form, revoke it", async ({ page }) => {
  await page.goto("/admin/users/bob");
  await expect(page.getByText("No link")).toBeVisible();
  await page.getByRole("button", { name: "Issue link" }).click();
  const dialog = page.getByRole("dialog", { name: "Link for bob" });
  await dialog.getByRole("button", { name: "Issue link" }).click();
  const link = await dialog.getByLabel("Link").inputValue();
  expect(link).toMatch(/^http:\/\/localhost:\d+\/u#[\w-]{43}$/);
  await pressIn(dialog, "Escape");
  await expect(dialog).toBeHidden();
  await expect(page.getByText("Has a link")).toBeVisible();

  // Issuing again rotates it.
  await page.getByRole("button", { name: "New link" }).click();
  const rotate = page.getByRole("dialog", { name: "Link for bob" });
  await expect(rotate).toContainText("The current link stops working");
  await rotate.getByRole("button", { name: "New link" }).click();
  expect(await rotate.getByLabel("Link").inputValue()).not.toBe(link);
  await pressIn(rotate, "Escape");
  await expect(rotate).toBeHidden();

  await page.getByRole("button", { name: "Revoke link" }).click();
  await page.getByRole("dialog", { name: "Revoke bob's link?" }).getByRole("button", { name: "Revoke link" }).click();
  await expect(page.getByText("No link")).toBeVisible();
});

test("remove a device and a user, confirmed from the keyboard", async ({ page }) => {
  await page.goto("/admin/users/alice");
  const devices = page.getByRole("region", { name: "Devices" });
  await expect(devices.getByRole("row")).toHaveCount(4); // header + phone, laptop, tv

  const trigger = devices.getByRole("button", { name: "Remove alice/tv" });
  await trigger.focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "Remove alice/tv?" });
  await expect(dialog.getByRole("button", { name: "Close" })).toBeFocused();
  await pressIn(dialog, "Escape");
  await expect(dialog).toBeHidden();
  await expect(trigger).toBeFocused();
  // A click outside dismisses it too, once Kobalte listens for it (a tick after opening).
  await trigger.click();
  await expect(dialog).toBeVisible();
  await expect(async () => {
    await page.mouse.click(5, 5);
    await expect(dialog).toBeHidden({ timeout: 500 });
  }).toPass();

  await trigger.click();
  await dialog.getByRole("button", { name: "Remove" }).click();
  await expect(dialog).toBeHidden();
  await expect(devices.getByRole("row")).toHaveCount(3);

  await page.goto("/admin/users/carol");
  await page.getByRole("button", { name: "Remove user" }).click();
  const confirm = page.getByRole("dialog", { name: "Remove user carol?" });
  await expect(confirm).toContainText("Removes their only device and their link.");
  await confirm.getByRole("button", { name: "Remove user" }).click();
  await expect(page).toHaveURL("/admin/users");
  await expect(page.getByRole("row", { name: /^carol/ })).toHaveCount(0);

  await page.goto("/admin/users/nobody");
  await expect(page.getByText("There is no user of this name.")).toBeVisible();
});

test("the connections table updates live, sorts and filters by users and devices", async ({ page }) => {
  await page.goto("/admin/connections");
  const table = page.getByRole("table", { name: "Open connections" });
  await expect(table.getByRole("row")).toHaveCount(26); // header + the fake's cap of 25
  await expect(page.getByRole("status")).toHaveText("Only the 25 fastest are shown; the gateway left the rest out.");
  await expect(table.getByRole("columnheader", { name: "Down rate" })).toHaveAttribute("aria-sort", "descending");
  await expect(table.getByRole("columnheader", { name: "Last traffic" })).toBeVisible();
  await expect(table.getByRole("row").nth(1).locator("time")).toHaveAttribute("datetime", /T\d{2}:\d{2}:\d{2}/);

  // Rates arrive from the second update and keep changing.
  const first = table.getByRole("row").nth(1);
  await expect(first).not.toContainText(/\s0 bit\/s\s+0 bit\/s/, { timeout: 5000 });
  await expect(first.locator("time")).toHaveAttribute("datetime", /T\d{2}:\d{2}:\d{2}/);
  const before = await table.textContent();
  await expect(table).not.toHaveText(before!, { timeout: 5000 });

  await table.getByRole("button", { name: "Up rate" }).click();
  await expect(table.getByRole("columnheader", { name: "Up rate" })).toHaveAttribute("aria-sort", "descending");
  await expect(table.getByRole("columnheader", { name: "Down rate" })).not.toHaveAttribute("aria-sort");
  await table.getByRole("button", { name: "Up rate" }).click();
  await expect(table.getByRole("columnheader", { name: "Up rate" })).toHaveAttribute("aria-sort", "ascending");

  await page.locator("summary").filter({ hasText: "All users and devices" }).click();
  const alice = page.getByRole("group", { name: "alice" });
  const bob = page.getByRole("group", { name: "bob" });
  await alice.getByRole("checkbox", { name: "phone", exact: true }).check();
  await expect(page).toHaveURL("/admin/connections?peer=peer-1");
  await expect(page.getByRole("status")).toBeHidden();
  const peers = await table.getByRole("row").locator("td:first-child").allTextContents();
  expect(peers.length).toBeGreaterThan(0);
  expect(new Set(peers)).toEqual(new Set(["alice/phone"]));
  await expect(table.getByRole("row").nth(1)).toContainText("10.8.0.2:");

  await bob.getByRole("checkbox", { name: "All devices for bob" }).check();
  await expect(page.locator("summary")).toContainText("3 selected devices");
  const peerNames = () => table.locator("tbody td:first-child").allTextContents().then((names) => [...new Set(names)].sort());
  await expect.poll(peerNames).toEqual(["alice/phone", "bob/laptop", "bob/phone"]);

  await bob.getByRole("checkbox", { name: "laptop", exact: true }).uncheck();
  await expect.poll(peerNames).toEqual(["alice/phone", "bob/phone"]);
  await page.reload();
  await expect.poll(peerNames).toEqual(["alice/phone", "bob/phone"]);

  await page.locator("summary").filter({ hasText: "2 selected devices" }).click();
  await page.getByRole("button", { name: "Clear filter" }).click();
  await expect(page).toHaveURL("/admin/connections");
  await expect(table.getByRole("row")).toHaveCount(26);
  await noSidewaysScroll(page);
});

test("a device's connections open from its user's page", async ({ page }) => {
  await page.goto("/admin/users/bob");
  await page.getByRole("region", { name: "Devices" }).getByRole("link", { name: "Connections bob/phone" }).click();
  await expect(page).toHaveURL("/admin/connections?peer=peer-4");
  await expect(page.locator("summary")).toHaveText("1 selected device");
  await expect(page.getByRole("table", { name: "Open connections" }).getByRole("row").nth(1)).toContainText("al-1");

  // An unknown peer is the daemon's NotFound; picking another one recovers.
  await page.goto("/admin/connections?peer=peer-99");
  await expect(page.getByRole("alert")).toContainText("peer peer-99 not found");
  await page.locator("summary").filter({ hasText: "1 selected device" }).click();
  await page.getByRole("button", { name: "Clear filter" }).click();
  await expect(page.getByRole("table", { name: "Open connections" })).toBeVisible();
});

test("the incident log: the count in the navigation, open ones first, a filter by upstream", async ({ page }) => {
  await page.goto("/admin");
  const nav = page.getByRole("navigation", { name: "Operator" });
  // nl-1's upload and de-1's outage are open.
  await expect(nav.getByRole("link", { name: "Incidents (2 open)" })).toBeVisible();
  await nav.getByRole("link", { name: "Incidents" }).click();
  await expect(page).toHaveURL("/admin/incidents");

  const table = page.getByRole("table", { name: "Incidents, open first, then newest" });
  const rows = table.getByRole("row");
  await expect(rows).toHaveCount(7); // header + 2 open + 4 closed
  await expect(rows.nth(1)).toContainText("nl-1");
  await expect(rows.nth(1)).toContainText("Upload fails");
  await expect(rows.nth(1)).toContainText("The upload probe fails");
  await expect(rows.nth(1)).toContainText("Ongoing");
  await expect(rows.nth(2)).toContainText("de-1");
  await expect(rows.nth(3)).toContainText("Send errors");
  await expect(rows.nth(3)).toContainText("7 min");
  await expect(rows.nth(3)).toContainText("sendmmsg: message too long");
  await expect(page.getByText("2 open · 6 on this page")).toBeVisible();

  // Refresh fetches it again.
  const updated = page.getByText(/^updated /);
  const before = await updated.textContent();
  await expect(async () => {
    await page.getByRole("button", { name: "Refresh" }).click();
    await expect(updated).not.toHaveText(before!, { timeout: 500 });
  }).toPass();

  await page.getByLabel("Upstream").selectOption("nl-1");
  await expect(page).toHaveURL("/admin/incidents?upstream=nl-1");
  await expect(rows).toHaveCount(3);
  await expect(rows.nth(2)).toContainText("Download fails");
  await page.getByLabel("Upstream").selectOption("al-1");
  await expect(page.getByText("No incidents for al-1.")).toBeVisible();
  await expect(table).toBeHidden();
  await page.getByLabel("Upstream").selectOption({ label: "All upstreams" });
  await expect(page).toHaveURL("/admin/incidents");
  await expect(rows).toHaveCount(7);

  // An upstream's name leads to its page.
  await rows.nth(1).getByRole("link", { name: "nl-1" }).click();
  await expect(page).toHaveURL("/admin/upstreams/nl-1");
  await noSidewaysScroll(page);
});

test("the overview marks upstreams with open incidents and leads to them", async ({ page }) => {
  await page.goto("/admin");
  const upstreams = page.getByRole("table").first();
  await expect(upstreams.getByRole("row", { name: /^nl-1/ }).getByRole("link", { name: "Incident: Upload fails" })).toBeVisible();
  await expect(upstreams.getByRole("row", { name: /^de-1/ }).getByRole("link", { name: "Incident: Down" })).toBeVisible();
  await expect(upstreams.getByRole("row", { name: /^lv-1/ }).getByRole("link", { name: /^Incident/ })).toHaveCount(0);
  await upstreams.getByRole("link", { name: "Incident: Upload fails" }).click();
  await expect(page).toHaveURL("/admin/incidents?upstream=nl-1");
  await expect(page.getByLabel("Upstream")).toHaveValue("nl-1");
  await expect(page.getByRole("table", { name: "Incidents, open first, then newest" }).getByRole("row")).toHaveCount(3);
  await noSidewaysScroll(page);
});

test("an upstream's page: its latest speed, the speed chart and its incidents", async ({ page }) => {
  await page.goto("/admin/upstreams/nl-1");
  await expect(page.getByRole("heading", { name: "Upstream nl-1" })).toBeVisible();
  const speed = page.locator("dl > div", { hasText: "Speed test, down / up" });
  await expect(speed).toContainText(/\d[\d.]* Mb\/s \/ \d[\d.]* kb\/s/);
  await expect(speed).toContainText(/ago|now/);
  await expect(page.locator("figcaption", { hasText: "Speed tests" })).toContainText(/Down: mean \d/);
  await expect(page.locator("figure canvas")).toHaveCount(4); // live, traffic, latency, speed

  const incidents = page.getByRole("region", { name: "Incidents" });
  await expect(incidents.getByRole("row")).toHaveCount(3); // header + upload (open) + download
  await expect(incidents.getByRole("row").nth(1)).toContainText("Ongoing");
  await expect(incidents.getByRole("columnheader", { name: "Upstream" })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Incident: Upload fails" })).toHaveAttribute("href", "/admin/incidents?upstream=nl-1");

  await page.goto("/admin/upstreams/al-1");
  await expect(page.getByRole("region", { name: "Incidents" })).toContainText("No incidents for al-1.");
  await noSidewaysScroll(page);
});
