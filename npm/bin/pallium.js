#!/usr/bin/env node
"use strict";

const { spawn } = require("node:child_process");
const { ensureBinary } = require("../scripts/lib");

async function main() {
  const binPath = await ensureBinary({ quiet: true });
  const child = spawn(binPath, process.argv.slice(2), { stdio: "inherit" });

  for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(signal, () => {
      child.kill(signal);
    });
  }

  child.on("error", (error) => {
    console.error(`pallium: failed to start CLI: ${error.message}`);
    process.exit(1);
  });

  child.on("exit", (code, signal) => {
    if (signal) {
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code ?? 1);
  });
}

main().catch((error) => {
  console.error(`pallium: ${error.message}`);
  process.exit(1);
});
