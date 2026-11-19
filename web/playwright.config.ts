import { defineConfig, devices } from "@playwright/test";

// Smoke tests against `vite preview` of a production build that serves the
// fake daemon (`--mode e2e`), with the CSP the panel will send.
const port = 4173;

export default defineConfig({
  testDir: "e2e",
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  use: { baseURL: `http://localhost:${port}`, trace: "retain-on-failure" },
  projects: [
    { name: "desktop", testMatch: "admin.spec.ts", use: { ...devices["Desktop Chrome"] } },
    { name: "phone", testMatch: "user.spec.ts", use: { ...devices["Pixel 7"] } },
  ],
  webServer: {
    command: "pnpm build:e2e && pnpm preview:e2e",
    url: `http://localhost:${port}/admin`,
    reuseExistingServer: !process.env.CI,
  },
});
