#!/usr/bin/env node
// changelog-format -- put the entry `changeset version` just wrote into the
// house style of CHANGELOG.md.
//
//   node scripts/changelog-format.mjs [npm/CHANGELOG.md] [CHANGELOG.md]
//
// `changeset version` writes its entry beside the package it versioned, which
// is `npm/CHANGELOG.md`, in a shape of its own:
//
//     # @agent-gm/cli
//
//     ## 1.0.2
//
//     ### Patch Changes
//
//     - one sentence
//
// The changelog this project keeps is the one at the root, and it has kept one
// shape since 1.0.0: `## [1.0.2] — 2026-09-08`, bullets, no bump-size
// subheadings, `[Unreleased]` on top and a link reference at the bottom. So
// this moves the entry there and DELETES `npm/CHANGELOG.md`: two changelogs in
// one repository is two answers to one question, and the wrapper package is
// not separately versioned -- there is one version and one history (D39).
//
// The alternative is hand-editing the Version Packages pull request after
// every bump, which is the manual step this whole flow exists to remove.
//
// It is idempotent: with no `npm/CHANGELOG.md` there is nothing to do, and it
// says so and exits 0. The date is today in UTC, or AGENT_GM_CHANGELOG_DATE,
// which is what the tests set.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const from = process.argv[2] ?? path.join(root, "npm", "CHANGELOG.md");
const file = process.argv[3] ?? path.join(root, "CHANGELOG.md");
const repo = process.env.AGENT_GM_RELEASE_REPO ?? "thisnick/agent-gm";
const today = process.env.AGENT_GM_CHANGELOG_DATE ?? new Date().toISOString().slice(0, 10);

if (!fs.existsSync(from)) {
  console.log(`changelog-format: no ${path.relative(root, from)}; nothing to do`);
  process.exit(0);
}
const written = fs.readFileSync(from, "utf8");
const src = fs.readFileSync(file, "utf8");

// The entry changesets wrote: `## 1.0.2`, the newest first.
const raw = /^## (\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)[ \t]*$/m.exec(written);
if (!raw) {
  console.error(`changelog-format: ${path.relative(root, from)} carries no version heading`);
  process.exit(1);
}
const version = raw[1];

// The block runs to the next `## ` heading, or to the end.
const start = raw.index;
const rest = written.slice(start + raw[0].length);
const next = /^## /m.exec(rest);
const end = next ? start + raw[0].length + next.index : written.length;
const block = written.slice(start, end);

if (src.includes(`## [${version}]`)) {
  console.error(`changelog-format: CHANGELOG.md already has an entry for ${version}`);
  process.exit(1);
}

// The bullets, without the bump-size subheadings. The size is already visible
// in the version that came out of it, and `### Patch Changes` above a single
// bullet is a heading with no readers.
const body = block
  .split("\n")
  .slice(1)
  .filter((l) => !/^### (Major|Minor|Patch) Changes[ \t]*$/.test(l))
  .join("\n")
  .replace(/\n{3,}/g, "\n\n")
  .trim();

let out = src;

// Where it belongs: under `## [Unreleased]`, above the previous release.
const unreleased = /^## \[Unreleased\][ \t]*$/m.exec(out);
if (!unreleased) {
  console.error("changelog-format: CHANGELOG.md has no `## [Unreleased]` heading to file under");
  process.exit(1);
}
const after = out.slice(unreleased.index + unreleased[0].length);
const following = /^## \[/m.exec(after);
if (!following) {
  console.error("changelog-format: CHANGELOG.md names no previous release to file above");
  process.exit(1);
}
const at = unreleased.index + unreleased[0].length + following.index;

// Anything a maintainer wrote under `[Unreleased]` by hand is part of THIS
// release: it describes something that has not shipped, and this is the
// release that ships it. So it is carried into the entry, above the bullets
// changesets generated, rather than deleted -- the placeholder is recognised
// and dropped, and nothing else is. Silently emptying the section was a way to
// lose a note that somebody wrote on purpose.
const carried = out
  .slice(unreleased.index + unreleased[0].length, at)
  .split("\n")
  .filter((l) => l.trim() !== "Nothing yet.")
  .join("\n")
  .trim();

const entry = `## [${version}] — ${today}\n\n${carried ? carried + "\n\n" : ""}${body}\n`;

// `[Unreleased]` is emptied by the release that just consumed it. Leaving the
// entries in both places would publish the same sentence twice.
out = out.slice(0, unreleased.index) +
  "## [Unreleased]\n\nNothing yet.\n\n" +
  entry + "\n" +
  out.slice(at);

// The link references at the bottom: the comparison moves to the new version
// and the new version gets its own release link, in the order the file has.
const compare = new RegExp(`^\\[Unreleased\\]: .*$`, "m");
if (!compare.test(out)) {
  console.error("changelog-format: CHANGELOG.md has no [Unreleased] link reference");
  process.exit(1);
}
out = out.replace(
  compare,
  `[Unreleased]: https://github.com/${repo}/compare/v${version}...HEAD\n` +
    `[${version}]: https://github.com/${repo}/releases/tag/v${version}`,
);

fs.writeFileSync(file, out);

// One changelog. `npm/CHANGELOG.md` is where changesets puts the entry and
// nowhere anybody reads it from: leaving it behind would commit a second,
// divergent history of the same versions into the same pull request.
fs.rmSync(from);

console.log(`changelog-format: ${version} filed under [Unreleased] as of ${today}`);
