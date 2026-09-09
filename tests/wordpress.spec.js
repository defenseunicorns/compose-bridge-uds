// Copyright 2024-2026 Defense Unicorns
// SPDX-License-Identifier: Apache-2.0

const { test, expect } = require("@playwright/test");

const wordpressUsername = "bridge-admin";
const wordpressPassword = "Bridge-Smoke-Test-119!";

async function installWordPressIfNeeded(page) {
  await page.goto("/wp-admin/install.php");
  await page.waitForLoadState("domcontentloaded");

  const languagePicker = page.locator("#language");
  if (await languagePicker.isVisible()) {
    await languagePicker.selectOption("en_US");
    await page.locator("#language-continue").click();
  }

  const siteTitle = page.locator("#weblog_title");
  const installationFormVisible = await siteTitle
    .waitFor({ state: "visible", timeout: 30_000 })
    .then(() => true)
    .catch(() => false);
  if (!installationFormVisible) {
    return;
  }

  await siteTitle.fill("Compose Bridge Smoke Test");
  await page.locator("#user_login").fill(wordpressUsername);
  await page.locator("#pass1").fill(wordpressPassword);
  await page.locator("#admin_email").fill("bridge-smoke@uds.dev");
  await page.locator("#submit").click();

  await expect(page.getByRole("heading", { name: "Success!" })).toBeVisible();
}

test("deploys a functional WordPress and MySQL application behind UDS SSO", async ({ page }) => {
  await installWordPressIfNeeded(page);

  await page.goto("/wp-login.php");
  await page.locator("#user_login").fill(wordpressUsername);
  await page.locator("#user_pass").fill(wordpressPassword);
  await page.locator("#wp-submit").click();

  await expect(page).toHaveURL(/\/wp-admin\/?$/);
  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();
});
