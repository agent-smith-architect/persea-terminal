"use strict";

// One browser version for local runs and CI; explicit overrides remain useful
// when checking a particular installed Chromium build.
module.exports = function chromiumPath() {
  const playwright = require(process.env.PERSEA_PLAYWRIGHT_MODULE || "playwright");
  return process.env.CHROME_BIN || playwright.chromium.executablePath();
};
