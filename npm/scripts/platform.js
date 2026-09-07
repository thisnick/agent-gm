// The platform map, and nothing else, so that the postinstall and the tests
// cannot come to disagree about which platforms exist (spec section 14.3).
"use strict";

const path = require("path");
const fs = require("fs");

const REPO = "thisnick/agent-gm";
const RELEASES = `https://github.com/${REPO}/releases`;

// process.platform x process.arch -> the Go pair the release archives are
// named for. Windows is deliberately absent: it is not in the v1 release
// matrix, so an install there fails loudly here rather than installing
// something that cannot run.
const SUPPORTED = {
  "linux:x64": { goos: "linux", goarch: "amd64" },
  "linux:arm64": { goos: "linux", goarch: "arm64" },
  "darwin:x64": { goos: "darwin", goarch: "amd64" },
  "darwin:arm64": { goos: "darwin", goarch: "arm64" },
};

class UnsupportedPlatformError extends Error {}

// target answers what to download, or throws a message that NAMES the
// platform. "unsupported platform" on its own tells the reader nothing they
// can act on; the pair and the release page together do.
function target(platform, arch, version) {
  const key = `${platform}:${arch}`;
  const hit = SUPPORTED[key];
  if (!hit) {
    const known = Object.keys(SUPPORTED).sort().join(", ");
    throw new UnsupportedPlatformError(
      `@agent-gm/cli does not ship a binary for ${platform}/${arch}.\n` +
        `Supported platforms are: ${known}.\n` +
        `See ${RELEASES} for the archives that do exist, or build agm from source:\n` +
        `  go build -o agm ./cmd/agm    (from ${"https://github.com/" + REPO})`
    );
  }
  const asset = `agm_${version}_${hit.goos}_${hit.goarch}.tar.gz`;
  return {
    ...hit,
    asset,
    url: `${RELEASES}/download/v${version}/${asset}`,
  };
}

// packageRoot is the directory holding package.json, whether this file is
// loaded from bin/ or from scripts/.
function packageRoot() {
  return path.resolve(__dirname, "..");
}

function vendorBinary() {
  return path.join(packageRoot(), "vendor", "agm");
}

// readChecksums parses the `sha256␠␠name` lines of the checksums.txt that is
// PINNED INSIDE this package. It is deliberately not fetched: a compromised
// release page must not be able to hand an already-published package version
// a different binary and a matching checksum.
function readChecksums(file) {
  const raw = fs.readFileSync(file, "utf8");
  const out = new Map();
  for (const line of raw.split("\n")) {
    const m = line.trim().match(/^([0-9a-f]{64})\s+\*?(.+)$/);
    if (m) out.set(m[2].trim(), m[1]);
  }
  return out;
}

module.exports = {
  REPO,
  RELEASES,
  SUPPORTED,
  UnsupportedPlatformError,
  target,
  packageRoot,
  vendorBinary,
  readChecksums,
};
