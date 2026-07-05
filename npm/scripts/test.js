#!/usr/bin/env node
"use strict";

const assert = require("node:assert/strict");
const { assetName, parseChecksum, platformKey } = require("./lib");
const packageJson = require("../package.json");

const escapedVersion = packageJson.version.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const expectedAsset = new RegExp(`^pallium_${escapedVersion}_(darwin|linux)_(arm64|amd64)\\.tar\\.gz$`);
const checksumAsset = `pallium_${packageJson.version}_darwin_arm64.tar.gz`;

assert.match(platformKey(), /^(darwin|linux|win32|freebsd|openbsd|aix|sunos)-/);
if (["darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"].includes(platformKey())) {
  assert.match(assetName(), expectedAsset);
}
assert.equal(parseChecksum(`abc123  ${checksumAsset}\n`, checksumAsset), "abc123");

console.log("pallium npm wrapper tests passed");
