import type { Page } from "@playwright/test";
import { expect, pressIn, settled, test } from "./fixtures";

/** The console fits the window: tables scroll inside their frames, the page never sideways. */
async function noSidewaysScroll(page: Page) {
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
}

test("every page is reachable from the navigation", async ({ page }) => {
  await page.goto("/admin");
  const nav = page.getByRole("navigation", { name: "Operator" });
  for (const [name, path, heading] of [
    ["Profiles", "/admin/profiles", "Profiles"],
    ["Users", "/admin/users", "Users"],
    ["Connections", "/admin/connections", "Connections"],
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

test("create, edit and delete a profile", async ({ page }) => {
  await page.goto("/admin/profiles");
  const table = page.getByRole("table");
  await expect(table.getByRole("row")).toHaveCount(5); // header + default, streaming, backup, fi
  // The daemon's warnings show with the profile they are about.
  const fi = table.getByRole("row", { name: /^fi/ });
  await expect(fi).toContainText("Tier 1 (geo=FI) matches no upstream and is ignored.");
  await expect(table.getByRole("row", { name: /^default/ })).toContainText("Tier 2 (geo=LV) never applies: tier 1 tag=near already takes");

  await page.getByRole("button", { name: "New profile" }).click();
  const dialog = page.getByRole("dialog", { name: "New profile" });
  await expect(dialog.getByRole("button", { name: "Close" })).toBeFocused();
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog.getByText("Give the profile a name.")).toBeVisible();
  await expect(dialog.getByLabel("Name")).toBeFocused();
  await expect(dialog.getByLabel("Name")).toHaveAttribute("aria-invalid", "true");
  await dialog.getByLabel("Name").fill("default");
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog.getByText("A profile of this name exists, ignoring case.")).toBeVisible();

  await dialog.getByLabel("Name").fill("eu");
  await dialog.getByRole("button", { name: "Add tier" }).click();
  await expect(dialog.getByLabel("Tier 1 geo")).toBeFocused();
  await dialog.getByLabel("Tier 1 geo").fill("FI");
  await dialog.getByRole("button", { name: "Add tier" }).click();
  await dialog.getByLabel("Tier 2 tag").fill("eu");
  await expect(dialog.getByText("lotsman profile add eu geo=FI tag=eu")).toBeVisible();
  // No upstream is in FI: the form flags the tier and sends nothing.
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog.getByText("Tier 1 matches no upstream. The fields suggest the values that do.")).toBeVisible();
  await expect(dialog.getByLabel("Tier 1 geo")).toBeFocused();
  await expect(dialog.getByLabel("Tier 1 geo")).toHaveAttribute("aria-invalid", "true");
  await expect(dialog.locator("#profile-new-tier-0-geo-options option")).toHaveCount(4);

  await dialog.getByLabel("Tier 1 geo").fill("LV");
  await expect(dialog.getByText("Tier 1 matches no upstream.")).toBeHidden();
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog).toBeHidden();
  const eu = table.getByRole("row", { name: /^eu/ });
  const tiers = eu.getByRole("list", { name: "Tiers of eu" }).getByRole("listitem");
  await expect(tiers).toHaveText(["1. geo=LV", "2. tag=eu"]);
  await expect(eu).not.toContainText("Warning");

  // Reorder from the keyboard: focus follows the moved tier.
  await page.getByRole("button", { name: "Edit eu" }).click();
  const edit = page.getByRole("dialog", { name: "Edit profile eu" });
  await expect(edit.getByLabel("Name")).toHaveAttribute("readonly", "");
  await expect(edit.getByRole("button", { name: "Close" })).toBeFocused();
  await settled(edit);
  await edit.getByRole("button", { name: "Move tier 2 up" }).focus();
  await page.keyboard.press("Enter");
  await expect(edit.getByLabel("Tier 1 tag")).toHaveValue("eu");
  await expect(edit.getByRole("button", { name: "Move tier 1 down" })).toBeFocused();
  await edit.getByRole("button", { name: "Remove tier 2" }).click();
  await expect(edit.getByRole("button", { name: "Add tier" })).toBeFocused();
  await edit.getByRole("button", { name: "Save" }).click();
  await expect(edit).toBeHidden();
  await expect(tiers).toHaveText(["1. tag=eu"]);
  await expect(eu).not.toContainText("matches no upstream");

  // A profile in use cannot go.
  await page.getByRole("button", { name: "Delete default" }).click();
  const refuse = page.getByRole("dialog", { name: "Delete profile default?" });
  await refuse.getByRole("button", { name: "Delete" }).click();
  await expect(refuse.getByRole("alert")).toHaveText("default is in use. Remove its devices or give them another profile first.");
  await pressIn(refuse, "Escape");
  await expect(refuse).toBeHidden();

  await page.getByRole("button", { name: "Delete eu" }).click();
  await page.getByRole("dialog", { name: "Delete profile eu?" }).getByRole("button", { name: "Delete" }).click();
  await expect(eu).toBeHidden();
  await expect(table.getByRole("row")).toHaveCount(5);
  await noSidewaysScroll(page);
});

test("add a device and see its config and QR code", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/admin/users");
  await expect(page.getByRole("heading", { name: "alice", level: 2 })).toBeVisible();

  await page.getByRole("button", { name: "Add device", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Add a device" });
  await dialog.getByLabel("User").fill("dave");
  await dialog.getByLabel("Device").fill("phone");
  await expect(dialog.getByLabel("Profile")).toHaveValue("default");
  await dialog.getByRole("button", { name: "Add", exact: true }).click();

  const config = page.getByRole("dialog", { name: "Config of dave/phone · default" });
  await expect(config.getByLabel("Config file")).toHaveValue(/^\[Interface\]\nPrivateKey = .+\nAddress = 10\.8\.0\.8\/32\n/);
  const qr = config.getByRole("img", { name: "QR code of the config of dave/phone · default" });
  await expect(qr).toBeVisible();
  expect(await qr.evaluate((svg) => svg.tagName + (svg.querySelector("path")?.getAttribute("d")?.length ?? 0))).toMatch(/^svg\d{4,}$/);

  await config.getByRole("button", { name: "Copy" }).click();
  await expect(config.getByRole("button", { name: "Copied" })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toMatch(/^\[Interface\]/);
  const [file] = await Promise.all([page.waitForEvent("download"), config.getByRole("button", { name: "Download dave-phone.conf" }).click()]);
  expect(file.suggestedFilename()).toBe("dave-phone.conf");
  await pressIn(config, "Escape");
  await expect(config).toBeHidden();

  // The new user is listed, and the config can be shown again.
  const dave = page.getByRole("region", { name: "dave" });
  await expect(dave.getByRole("row")).toHaveCount(2);
  await dave.getByRole("button", { name: "Config dave/phone · default" }).click();
  const again = page.getByRole("dialog", { name: "Config of dave/phone · default" });
  await expect(again.getByLabel("Config file")).toHaveValue(/Address = 10\.8\.0\.8\/32/);
  await pressIn(again, "Escape");
  await expect(again).toBeHidden();

  // The same device and profile twice is refused, with the daemon's reason.
  await dave.getByRole("button", { name: "Add device for dave" }).click();
  const twice = page.getByRole("dialog", { name: "Add a device" });
  await expect(twice.getByLabel("User")).toHaveValue("dave");
  await twice.getByLabel("Device").fill("phone");
  await twice.getByRole("button", { name: "Add", exact: true }).click();
  await expect(twice.getByRole("alert")).toHaveText(/dave\/phone already has profile default/);
  await noSidewaysScroll(page);
});

test("issue a link, see it once in its /u# form, revoke it", async ({ page }) => {
  await page.goto("/admin/users");
  const bob = page.getByRole("region", { name: "bob" });
  await expect(bob).toContainText("No link");
  await bob.getByRole("button", { name: "Issue link bob" }).click();
  const dialog = page.getByRole("dialog", { name: "Link for bob" });
  await dialog.getByRole("button", { name: "Issue link" }).click();
  const link = await dialog.getByLabel("Link").inputValue();
  expect(link).toMatch(/^http:\/\/localhost:\d+\/u#[\w-]{43}$/);
  await pressIn(dialog, "Escape");
  await expect(dialog).toBeHidden();
  await expect(bob).toContainText("Has a link");

  // Issuing again rotates it.
  await bob.getByRole("button", { name: "New link bob" }).click();
  const rotate = page.getByRole("dialog", { name: "Link for bob" });
  await expect(rotate).toContainText("The current link stops working");
  await rotate.getByRole("button", { name: "New link" }).click();
  expect(await rotate.getByLabel("Link").inputValue()).not.toBe(link);
  await pressIn(rotate, "Escape");
  await expect(rotate).toBeHidden();

  await bob.getByRole("button", { name: "Revoke link bob" }).click();
  await page.getByRole("dialog", { name: "Revoke bob's link?" }).getByRole("button", { name: "Revoke link" }).click();
  await expect(bob).toContainText("No link");
});

test("remove a device and a user, confirmed from the keyboard", async ({ page }) => {
  await page.goto("/admin/users");
  const alice = page.getByRole("region", { name: "alice" });
  await expect(alice.getByRole("row")).toHaveCount(4); // header + phone, laptop, tv

  const trigger = alice.getByRole("button", { name: "Remove alice/tv · streaming" });
  await trigger.focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "Remove alice/tv · streaming?" });
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
  await expect(alice.getByRole("row")).toHaveCount(3);

  const carol = page.getByRole("region", { name: "carol" });
  await carol.getByRole("button", { name: "Remove user carol" }).click();
  const confirm = page.getByRole("dialog", { name: "Remove user carol?" });
  await expect(confirm).toContainText("Removes their only config and their link.");
  await confirm.getByRole("button", { name: "Remove user" }).click();
  await expect(carol).toBeHidden();
});

test("the connections table updates live, sorts and filters by peer", async ({ page }) => {
  await page.goto("/admin/connections");
  const table = page.getByRole("table", { name: "Open connections" });
  await expect(table.getByRole("row")).toHaveCount(26); // header + the fake's cap of 25
  await expect(page.getByRole("status")).toHaveText("Only the 25 fastest are shown; the gateway left the rest out.");
  await expect(table.getByRole("columnheader", { name: "Down rate" })).toHaveAttribute("aria-sort", "descending");

  // Rates arrive from the second update and keep changing.
  const first = table.getByRole("row").nth(1);
  await expect(first).not.toContainText(/\s0 bit\/s\s+0 bit\/s/, { timeout: 5000 });
  const before = await table.textContent();
  await expect(table).not.toHaveText(before!, { timeout: 5000 });

  await table.getByRole("button", { name: "Up rate" }).click();
  await expect(table.getByRole("columnheader", { name: "Up rate" })).toHaveAttribute("aria-sort", "descending");
  await expect(table.getByRole("columnheader", { name: "Down rate" })).not.toHaveAttribute("aria-sort");
  await table.getByRole("button", { name: "Up rate" }).click();
  await expect(table.getByRole("columnheader", { name: "Up rate" })).toHaveAttribute("aria-sort", "ascending");

  await page.getByLabel("Peer").selectOption({ label: "alice/phone · default" });
  await expect(page).toHaveURL("/admin/connections?peer=peer-1");
  await expect(page.getByRole("status")).toBeHidden();
  const peers = await table.getByRole("row").locator("td:first-child").allTextContents();
  expect(peers.length).toBeGreaterThan(0);
  expect(new Set(peers)).toEqual(new Set(["alice/phone"]));
  await expect(table.getByRole("row").nth(1)).toContainText("10.8.0.2:");
  await noSidewaysScroll(page);
});

test("a peer's connections open from the users page", async ({ page }) => {
  await page.goto("/admin/users");
  await page.getByRole("region", { name: "bob" }).getByRole("link", { name: "Connections bob/phone · default" }).click();
  await expect(page).toHaveURL("/admin/connections?peer=peer-4");
  await expect(page.getByLabel("Peer")).toHaveValue("peer-4");
  await expect(page.getByRole("table", { name: "Open connections" }).getByRole("row").nth(1)).toContainText("al-1");

  // An unknown peer is the daemon's NotFound; picking another one recovers.
  await page.goto("/admin/connections?peer=peer-99");
  await expect(page.getByRole("alert")).toContainText("peer peer-99 not found");
  await page.getByLabel("Peer").selectOption({ label: "All peers" });
  await expect(page.getByRole("table", { name: "Open connections" })).toBeVisible();
});
