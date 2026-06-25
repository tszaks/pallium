#!/usr/bin/env node
"use strict";

const { installBinary } = require("./lib");

installBinary({ quiet: false }).catch((error) => {
  console.error(`pallium install failed: ${error.message}`);
  process.exit(1);
});
