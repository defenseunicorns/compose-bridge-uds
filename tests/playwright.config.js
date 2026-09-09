// Copyright 2024-2026 Defense Unicorns
// SPDX-License-Identifier: Apache-2.0

const { defineConfig, devices } = require("@playwright/test");

const playwrightDir = ".playwright";
const authFile = `${playwrightDir}/auth/user.json`;

const config = defineConfig({
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: [
    ["html", { outputFolder: `${playwrightDir}/report`, open: "never" }],
    ["json", { outputFile: `${playwrightDir}/report/results.json` }],
    ["list"],
  ],
  outputDir: `${playwrightDir}/output`,
  use: {
    baseURL: process.env.BASE_URL || "https://wordpress.uds.dev",
    trace: "on-first-retry",
  },
  projects: [
    {
      name: "setup",
      testMatch: "*.setup.js",
    },
    {
      name: "chromium",
      dependencies: ["setup"],
      testMatch: "*.spec.js",
      use: {
        ...devices["Desktop Chrome"],
        storageState: authFile,
      },
    },
  ],
});

module.exports = config;
module.exports.authFile = authFile;
