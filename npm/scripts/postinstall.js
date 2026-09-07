// postinstall -- download the `agm` release archive for this platform and
// verify it against the checksums.txt pinned inside this package
// (spec section 14.3).
//
// Three things this script will not do:
//
//   * install something that cannot run. An unsupported platform fails here,
//     naming the platform, rather than leaving a shim that dies later with a
//     ENOENT nobody can read.
//   * trust the release page about what it served. The SHA-256 is checked
//     against `checksums.txt` as it was at publish time, which travels inside
//     the npm tarball. A release page that is edited afterwards cannot change
//     what an already-published version accepts.
//   * fetch anything when the caller says not to. AGENT_GM_CLI_SKIP_DOWNLOAD=1
//     and AGENT_GM_CLI_BINARY=<path> both mean "the binary is my problem".
"use strict";

const fs = require("fs");
const os = require("os");
const path = require("path");
const crypto = require("crypto");
const { execFileSync } = require("child_process");

const {
  RELEASES,
  UnsupportedPlatformError,
  target,
  packageRoot,
  vendorBinary,
  readChecksums,
} = require("./platform.js");

function log(msg) {
  process.stdout.write(`@agent-gm/cli: ${msg}\n`);
}

async function download(url, dest) {
  // file:// is how the release dry run and the tests serve archives without a
  // network; https:// is every real install.
  if (url.startsWith("file://")) {
    fs.copyFileSync(new URL(url), dest);
    return;
  }
  const res = await fetch(url, { redirect: "follow" });
  if (!res.ok) {
    throw new Error(
      `downloading ${url} failed with HTTP ${res.status} ${res.statusText}.\n` +
        `The release assets are listed at ${RELEASES}.`
    );
  }
  fs.writeFileSync(dest, Buffer.from(await res.arrayBuffer()));
}

function sha256(file) {
  return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
}

async function main() {
  if (process.env.AGENT_GM_CLI_SKIP_DOWNLOAD === "1") {
    log("AGENT_GM_CLI_SKIP_DOWNLOAD=1, so no binary was downloaded.");
    log("Set AGENT_GM_CLI_BINARY=<path> or place one at vendor/agm before running agm.");
    return;
  }
  if (process.env.AGENT_GM_CLI_BINARY) {
    log(`AGENT_GM_CLI_BINARY=${process.env.AGENT_GM_CLI_BINARY}, so no binary was downloaded.`);
    return;
  }

  const root = packageRoot();
  const version = JSON.parse(
    fs.readFileSync(path.join(root, "package.json"), "utf8")
  ).version;

  const want = target(process.platform, process.arch, version);

  const checksumsFile = path.join(root, "checksums.txt");
  if (!fs.existsSync(checksumsFile)) {
    throw new Error(
      "checksums.txt is missing from this package. It is written at publish " +
        "time and is what the download is verified against; without it there " +
        "is nothing to verify, so the install stops here."
    );
  }
  const sums = readChecksums(checksumsFile);
  const expected = sums.get(want.asset);
  if (!expected) {
    throw new Error(
      `checksums.txt in this package lists no entry for ${want.asset}, so the ` +
        `download could not be verified. Known entries: ${[...sums.keys()].join(", ")}`
    );
  }

  const base = process.env.AGENT_GM_CLI_BASE_URL;
  const url = base ? `${base.replace(/\/$/, "")}/${want.asset}` : want.url;

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "agent-gm-cli-"));
  try {
    const archive = path.join(tmp, want.asset);
    log(`downloading ${want.asset}`);
    await download(url, archive);

    const got = sha256(archive);
    if (got !== expected) {
      throw new Error(
        `checksum mismatch for ${want.asset}\n` +
          `  expected ${expected}  (from the checksums.txt pinned in this package)\n` +
          `  actual   ${got}       (from ${url})\n` +
          "The download does not match the release this package was published " +
          "against. Nothing was installed."
      );
    }
    log(`verified ${want.asset} against the pinned checksums.txt`);

    execFileSync("tar", ["-xzf", archive, "-C", tmp, "agm"], { stdio: "inherit" });
    const vendorDir = path.join(root, "vendor");
    fs.mkdirSync(vendorDir, { recursive: true });
    const dest = vendorBinary();
    fs.copyFileSync(path.join(tmp, "agm"), dest);
    fs.chmodSync(dest, 0o755);
    log(`installed ${dest}`);
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

main().catch((err) => {
  const label =
    err instanceof UnsupportedPlatformError ? "unsupported platform" : "install failed";
  process.stderr.write(`@agent-gm/cli: ${label}\n\n${err.message}\n`);
  process.exit(1);
});
