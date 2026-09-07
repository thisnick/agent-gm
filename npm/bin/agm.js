#!/usr/bin/env node
// The shim. It runs the real `agm` and gets out of the way.
//
// The one contract it has to keep is the exit code. Spec section 11.2 leaves
// `1` deliberately unassigned so that a `1` is the wrapper's, the shell's or
// the runtime's -- never Agent GM's. A shim that caught a failure and exited
// `1` would therefore turn every one of the ten codes into "something went
// wrong somewhere", which is the whole reason the table exists. So: the
// child's status is this process's status, exactly, and a child killed by a
// signal exits 128+n the way a shell reports it.
"use strict";

const fs = require("fs");
const { spawnSync } = require("child_process");
const { RELEASES, vendorBinary } = require("../scripts/platform.js");

const binary = process.env.AGENT_GM_CLI_BINARY || vendorBinary();

if (!fs.existsSync(binary)) {
  process.stderr.write(
    `@agent-gm/cli: no agm binary at ${binary}\n\n` +
      (process.env.AGENT_GM_CLI_BINARY
        ? "AGENT_GM_CLI_BINARY points at a path that does not exist.\n"
        : "The postinstall download did not run or did not finish. Reinstall the\n" +
          "package, or set AGENT_GM_CLI_BINARY=<path> to an agm binary you already\n" +
          `have. The release archives are at ${RELEASES}.\n`)
  );
  // 9 is section 11.2's "local configuration or credential-store failure",
  // which is what a missing local binary is.
  process.exit(9);
}

const res = spawnSync(binary, process.argv.slice(2), { stdio: "inherit" });

if (res.error) {
  process.stderr.write(`@agent-gm/cli: could not run ${binary}: ${res.error.message}\n`);
  process.exit(9);
}
if (res.signal) {
  process.exit(128 + (require("os").constants.signals[res.signal] || 0));
}
process.exit(res.status === null ? 9 : res.status);
