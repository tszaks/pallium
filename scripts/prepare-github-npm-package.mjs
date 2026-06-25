#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const sourceDir = path.join(root, "npm");
const outDir = path.join(root, "dist", "github-npm");

fs.rmSync(outDir, { recursive: true, force: true });
fs.mkdirSync(outDir, { recursive: true });

for (const entry of ["bin", "scripts", "README.md", "package.json"]) {
  fs.cpSync(path.join(sourceDir, entry), path.join(outDir, entry), { recursive: true });
}

const packagePath = path.join(outDir, "package.json");
const packageJson = JSON.parse(fs.readFileSync(packagePath, "utf8"));
packageJson.name = "@tszaks/pallium";
packageJson.publishConfig = {
  registry: "https://npm.pkg.github.com"
};
packageJson.repository = {
  type: "git",
  url: "git+https://github.com/tszaks/pallium.git"
};
fs.writeFileSync(packagePath, `${JSON.stringify(packageJson, null, 2)}\n`);

console.log(`Prepared ${packageJson.name}@${packageJson.version} in ${outDir}`);
