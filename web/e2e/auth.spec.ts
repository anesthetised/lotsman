import type { Page } from "@playwright/test";
import { expect, test } from "./fixtures";

/**
 * Gives the page Chromium's virtual authenticator: a platform passkey
 * provider with user verification that answers every prompt by itself.
 */
async function virtualAuthenticator(page: Page) {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  return async () => (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials;
}

test("enroll a passkey, sign out, sign in with it", async ({ page }) => {
  const credentials = await virtualAuthenticator(page);

  // The fake panel starts signed in; sign out first.
  await page.goto("/admin");
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();

  await page.goto("/enroll#dev");
  await page.getByLabel("Passkey name").fill("Laptop");
  await page.getByRole("button", { name: "Create passkey" }).click();
  await expect(page.getByRole("heading", { name: "Upstreams" })).toBeVisible();
  await expect(page).toHaveURL("/admin");
  await expect(page.getByText("Signed in with Laptop")).toBeVisible();
  const [passkey] = await credentials();
  expect(passkey?.isResidentCredential).toBe(true);
  expect(passkey?.rpId).toBe("localhost");

  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL("/login");

  // Signed out, the console sends you to sign in and back.
  await page.goto("/admin/passkeys");
  await expect(page).toHaveURL("/login?next=%2Fadmin%2Fpasskeys");
  await page.getByRole("button", { name: "Sign in with a passkey" }).click();
  await expect(page).toHaveURL("/admin/passkeys");
  await expect(page.getByRole("row", { name: /Laptop \(this session\)/ })).toBeVisible();
  expect((await credentials())[0]?.signCount).toBeGreaterThan(passkey?.signCount ?? 0);

  await page.getByRole("link", { name: "Upstreams" }).click();
  await expect(page.getByRole("table").first().getByRole("row")).toHaveCount(5);
});

test("an enrollment link works once", async ({ page }) => {
  await virtualAuthenticator(page);
  await page.goto("/enroll#dev");
  await page.getByLabel("Passkey name").fill("Laptop");
  await page.getByRole("button", { name: "Create passkey" }).click();
  await expect(page.getByRole("heading", { name: "Upstreams" })).toBeVisible();

  await page.goto("/enroll#dev");
  await page.getByLabel("Passkey name").fill("Phone");
  await page.getByRole("button", { name: "Create passkey" }).click();
  await expect(page.getByRole("alert")).toHaveText(/has been used or has expired/);

  await page.goto("/enroll");
  await expect(page.getByRole("alert")).toHaveText(/incomplete/);
});

test("issue an enrollment link and revoke passkeys", async ({ page }) => {
  await virtualAuthenticator(page);
  await page.goto("/admin/passkeys");
  await expect(page.getByRole("heading", { name: "Passkeys" })).toBeVisible();

  await page.getByRole("button", { name: "Issue enrollment link" }).click();
  const dialog = page.getByRole("dialog", { name: "Enrollment link" });
  const link = await dialog.getByLabel("Link").inputValue();
  expect(link).toMatch(/^http:\/\/localhost:\d+\/enroll#[\w-]{43}$/);
  await page.keyboard.press("Escape");

  // The link enrolls a second passkey, which signs this browser in as it.
  await page.goto(new URL(link).pathname + new URL(link).hash);
  await page.getByLabel("Passkey name").fill("Phone");
  await page.getByRole("button", { name: "Create passkey" }).click();
  await expect(page.getByText("Signed in with Phone")).toBeVisible();

  await page.getByRole("link", { name: "Passkeys" }).click();
  await expect(page.getByRole("row")).toHaveCount(3);
  await page.getByRole("button", { name: "Revoke dev" }).click();
  const confirm = page.getByRole("dialog", { name: "Revoke dev?" });
  await confirm.getByRole("button", { name: "Revoke" }).click();
  await expect(confirm).toBeHidden();
  await expect(page.getByRole("row")).toHaveCount(2);

  // Revoking the passkey of this session signs out.
  await page.getByRole("button", { name: "Revoke Phone" }).click();
  const self = page.getByRole("dialog", { name: "Revoke Phone?" });
  await expect(self).toContainText("you will be signed out");
  await expect(self).toContainText("last passkey");
  await self.getByRole("button", { name: "Revoke" }).click();
  await expect(page).toHaveURL("/login");
});
