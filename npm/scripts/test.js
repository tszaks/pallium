#!/usr/bin/env node
"use strict";

const assert = require("node:assert/strict");
const { assetName, parseChecksum, platformKey } = require("./lib");

assert.match(platformKey(), /^(darwin|linux|win32|freebsd|openbsd|aix|sunos)-/);
if (["darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"].includes(platformKey())) {
  assert.match(assetName(), /^pallium_0\.9\.3_(darwin|linux)_(arm64|amd64)\.tar\.gz$/);
}
assert.equal(parseChecksum("abc123  pallium_0.9.3_darwin_arm64.tar.gz\n", "pallium_0.9.3_darwin_arm64.tar.gz"), "abc123");

console.log("pallium npm wrapper tests passed");
