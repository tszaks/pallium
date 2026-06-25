"use strict";

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const https = require("node:https");
const crypto = require("node:crypto");
const { spawnSync } = require("node:child_process");

const packageJson = require("../package.json");

const REPO = "tszaks/pallium";
const VERSION = packageJson.version;
const TAG = `v${VERSION}`;
const BINARY_NAME = process.platform === "win32" ? "pallium.exe" : "pallium";

function platformKey() {
  const arch = process.arch === "x64" ? "amd64" : process.arch;
  return `${process.platform}-${arch}`;
}

function assetName() {
  const assets = {
    "darwin-arm64": `pallium_${VERSION}_darwin_arm64.tar.gz`,
    "darwin-amd64": `pallium_${VERSION}_darwin_amd64.tar.gz`,
    "linux-arm64": `pallium_${VERSION}_linux_arm64.tar.gz`,
    "linux-amd64": `pallium_${VERSION}_linux_amd64.tar.gz`
  };
  return assets[platformKey()] || null;
}

function installDir() {
  if (process.env.PALLIUM_INSTALL_DIR) {
    return path.resolve(process.env.PALLIUM_INSTALL_DIR);
  }
  return path.join(os.homedir(), ".pallium", "npm", TAG);
}

function binaryPath() {
  return path.join(installDir(), BINARY_NAME);
}

async function ensureBinary(options = {}) {
  if (!process.env.PALLIUM_FORCE_INSTALL && isExecutable(binaryPath())) {
    return binaryPath();
  }
  return installBinary(options);
}

async function installBinary(options = {}) {
  fs.mkdirSync(installDir(), { recursive: true });
  const asset = assetName();
  if (asset) {
    try {
      await installFromRelease(asset, options);
      return binaryPath();
    } catch (error) {
      log(options, `release binary unavailable, trying go install (${error.message})`);
    }
  } else {
    log(options, `no prebuilt binary for ${platformKey()}, trying go install`);
  }

  installWithGo();
  return binaryPath();
}

async function installFromRelease(asset, options) {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "pallium-npm-"));
  const archivePath = path.join(tmpDir, asset);
  try {
    const baseUrl = `https://github.com/${REPO}/releases/download/${TAG}`;
    log(options, `downloading ${baseUrl}/${asset}`);
    await download(`${baseUrl}/${asset}`, archivePath);
    await verifyChecksum(baseUrl, asset, archivePath, options);

    const tar = spawnSync("tar", ["-xzf", archivePath, "-C", tmpDir], { stdio: "pipe" });
    if (tar.status !== 0) {
      throw new Error(`tar failed: ${String(tar.stderr || tar.stdout).trim()}`);
    }

    const extracted = findFile(tmpDir, BINARY_NAME);
    if (!extracted) {
      throw new Error(`archive did not contain ${BINARY_NAME}`);
    }
    fs.copyFileSync(extracted, binaryPath());
    fs.chmodSync(binaryPath(), 0o755);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
}

async function verifyChecksum(baseUrl, asset, archivePath, options) {
  const checksumsPath = path.join(path.dirname(archivePath), "checksums.txt");
  await download(`${baseUrl}/checksums.txt`, checksumsPath);
  const expected = parseChecksum(fs.readFileSync(checksumsPath, "utf8"), asset);
  if (!expected) {
    throw new Error(`checksums.txt did not include ${asset}`);
  }
  const actual = crypto.createHash("sha256").update(fs.readFileSync(archivePath)).digest("hex");
  if (actual !== expected) {
    throw new Error(`checksum mismatch for ${asset}`);
  }
  log(options, "checksum verified");
}

function installWithGo() {
  const go = spawnSync("go", ["install", `github.com/${REPO}@${TAG}`], {
    env: { ...process.env, GOBIN: installDir() },
    stdio: "inherit"
  });
  if (go.error) {
    throw new Error(`go install failed to start: ${go.error.message}`);
  }
  if (go.status !== 0) {
    throw new Error(`go install exited with ${go.status}`);
  }
  if (!isExecutable(binaryPath())) {
    throw new Error(`go install did not create ${binaryPath()}`);
  }
}

function download(url, destination, redirects = 0) {
  return new Promise((resolve, reject) => {
    const request = https.get(
      url,
      {
        headers: {
          "User-Agent": `pallium-npm/${VERSION}`,
          "Accept": "application/octet-stream"
        }
      },
      (response) => {
        if ([301, 302, 303, 307, 308].includes(response.statusCode)) {
          response.resume();
          if (!response.headers.location || redirects >= 5) {
            reject(new Error(`redirect failed for ${url}`));
            return;
          }
          download(response.headers.location, destination, redirects + 1).then(resolve, reject);
          return;
        }

        if (response.statusCode !== 200) {
          response.resume();
          reject(new Error(`HTTP ${response.statusCode} for ${url}`));
          return;
        }

        const file = fs.createWriteStream(destination, { mode: 0o644 });
        response.pipe(file);
        file.on("finish", () => file.close(resolve));
        file.on("error", reject);
      }
    );
    request.on("error", reject);
  });
}

function parseChecksum(contents, asset) {
  for (const line of contents.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const parts = trimmed.split(/\s+/);
    if (parts.length >= 2 && path.basename(parts[parts.length - 1]) === asset) {
      return parts[0];
    }
  }
  return "";
}

function findFile(root, filename) {
  for (const entry of fs.readdirSync(root, { withFileTypes: true })) {
    const fullPath = path.join(root, entry.name);
    if (entry.isFile() && entry.name === filename) {
      return fullPath;
    }
    if (entry.isDirectory()) {
      const found = findFile(fullPath, filename);
      if (found) return found;
    }
  }
  return "";
}

function isExecutable(filePath) {
  try {
    fs.accessSync(filePath, fs.constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

function log(options, message) {
  if (!options.quiet) {
    console.log(`pallium: ${message}`);
  }
}

module.exports = {
  VERSION,
  TAG,
  assetName,
  binaryPath,
  ensureBinary,
  installBinary,
  installDir,
  parseChecksum,
  platformKey
};
