// Copyright 2024-2026 Defense Unicorns
// SPDX-License-Identifier: Apache-2.0

const { test: setup, expect } = require("@playwright/test");
const { authFile } = require("./playwright.config");

setup("authenticate with UDS SSO", async ({ page, context }) => {
  await page.goto("/");

  await expect(page).toHaveURL(/https:\/\/sso\.uds\.dev\//);
  await page.locator("#username").fill("doug");
  await page.locator("#password").fill("unicorn123!@#UN");
  await page.locator("#kc-login").click();

  await expect(page).toHaveURL(/https:\/\/wordpress\.uds\.dev\//);

  const cookies = await context.cookies();
  const keycloakSession = cookies.find((cookie) => cookie.name === "KEYCLOAK_SESSION");
  expect(keycloakSession).toBeDefined();
  expect(keycloakSession.value).not.toBe("");
  expect(keycloakSession.domain).toContain("sso.");

  await context.storageState({ path: authFile });
});
