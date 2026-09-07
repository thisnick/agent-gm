package release_test

// The version step, run the way `changesets/action` runs it, and asserted on
// the thing the action does NEXT.
//
// The first real run of `version.yml` on main failed here (run 34167397882).
// `npm run version-packages` succeeded, both changelogs looked right, and then
// the action read the changed package's own `CHANGELOG.md` -- which is how it
// composes the body of the Version Packages pull request -- and got `ENOENT`,
// because the formatter deleted it. Every test at the time passed: they drove
// the formatter over a fixture and asked what the files SAID, and none of them
// asked whether the file the next step opens was still there.
//
// So this runs `changeset version` for real, then the formatter, then reads
// `npm/CHANGELOG.md` the way the action does. It runs in a workspace of its
// own -- the repository's own manifest, changesets and changelog are not
// touched -- so a failure here damages nothing and a developer's uncommitted
// work is never restored over.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// changesetCLI is the binary `npm run version-packages` invokes. It is present
// after `npm install`, which the `version-packages` job in ci.yml does before
// running this test; `devbox run check` does not install node modules, and a
// test that failed for that reason would be a test that fails on a clean
// checkout for no reason.
func changesetCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(repoRoot(t), "node_modules", ".bin", "changeset")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("@changesets/cli is not installed here; run " +
			"`devbox run -- npm install --ignore-scripts` (ci.yml's `version-packages` job does)")
	}
	return bin
}

const workspaceChangelog = `# Changelog

Agent GM follows [semantic versioning](https://semver.org).

## [Unreleased]

Nothing yet.

## [1.0.1] — 2026-09-07

- the previous release

[Unreleased]: https://github.com/thisnick/agent-gm/compare/v1.0.1...HEAD
[1.0.1]: https://github.com/thisnick/agent-gm/releases/tag/v1.0.1
`

// versionWorkspace builds a miniature of this repository: a private root with
// `npm` as its one workspace, the package at 1.0.1, the house changelog, and
// one pending changeset.
func versionWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("package.json", `{
  "name": "agent-gm-repo-fixture",
  "private": true,
  "version": "0.0.0",
  "workspaces": ["npm"]
}
`)
	write("npm/package.json", `{
  "name": "@agent-gm/cli",
  "version": "1.0.1",
  "license": "AGPL-3.0-or-later"
}
`)
	write("CHANGELOG.md", workspaceChangelog)

	// The real config, so the changelog generator and the bump behaviour are
	// the ones the action gets rather than a fixture's.
	cfg, err := os.ReadFile(filepath.Join(repoRoot(t), ".changeset", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	write(".changeset/config.json", string(cfg))
	write(".changeset/a-fixture-change.md", "---\n\"@agent-gm/cli\": patch\n---\n\na sentence for the changelog\n")
	return dir
}

// The sequence, and the file the action opens after it.
func TestTheVersionStepLeavesTheChangelogTheActionReads(t *testing.T) {
	cli := changesetCLI(t)
	dir := versionWorkspace(t)

	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "AGENT_GM_CHANGELOG_DATE=2026-09-08")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, out)
		}
	}

	// Exactly what `npm run version-packages` is: `changeset version && node
	// scripts/changelog-format.mjs`.
	run(cli, "version")
	run("node", filepath.Join(repoRoot(t), "scripts", "changelog-format.mjs"),
		filepath.Join(dir, "npm", "CHANGELOG.md"), filepath.Join(dir, "CHANGELOG.md"))

	// 1. The version moved, which is what everything else is stamped from.
	pkg, err := os.ReadFile(filepath.Join(dir, "npm", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pkg), `"version": "1.0.2"`) {
		t.Fatalf("the package was not bumped to 1.0.2:\n%s", pkg)
	}

	// 2. THE REGRESSION. `changesets/action` reads the changed package's own
	// CHANGELOG.md to compose the pull request body. Deleting it turned a
	// green version step into a failed job, on the first real run.
	pkgLog, err := os.ReadFile(filepath.Join(dir, "npm", "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("npm/CHANGELOG.md is not readable after the version step, which is where "+
			"changesets/action looks to build the Version Packages pull request body "+
			"(run 34167397882 failed with exactly this): %v", err)
	}
	if !strings.Contains(string(pkgLog), "## 1.0.2") {
		t.Errorf("npm/CHANGELOG.md carries no 1.0.2 entry, so the pull request body would be "+
			"empty:\n%s", pkgLog)
	}
	if !strings.Contains(string(pkgLog), "a sentence for the changelog") {
		t.Errorf("npm/CHANGELOG.md lost the changeset's sentence:\n%s", pkgLog)
	}

	// 3. And the root changelog is the house format, with the same entry.
	root, err := os.ReadFile(filepath.Join(dir, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(root)
	if !strings.Contains(got, "## [1.0.2] — 2026-09-08") {
		t.Errorf("the root entry is not in the house format:\n%s", got)
	}
	if !strings.Contains(got, "- a sentence for the changelog") {
		t.Errorf("the root entry lost the changeset's sentence:\n%s", got)
	}
	if strings.Contains(got, "### Patch Changes") {
		t.Errorf("the bump-size subheading reached the root changelog:\n%s", got)
	}
	if !strings.Contains(got, "[1.0.2]: https://github.com/thisnick/agent-gm/releases/tag/v1.0.2") {
		t.Errorf("the root entry has no link reference:\n%s", got)
	}
	iUnreleased := strings.Index(got, "## [Unreleased]")
	iNew := strings.Index(got, "## [1.0.2]")
	iOld := strings.Index(got, "## [1.0.1]")
	if iUnreleased >= iNew || iNew >= iOld {
		t.Errorf("the sections are out of order:\n%s", got)
	}

	// 4. Both files carry the version, which is the whole claim: one version,
	// two files, neither of them missing.
	if !strings.Contains(string(pkgLog), "1.0.2") || !strings.Contains(got, "1.0.2") {
		t.Error("the two changelogs do not agree on the version that was just cut")
	}
}

// A second run must be a no-op rather than a failure. `npm/CHANGELOG.md` now
// survives from one release to the next, so its newest heading stays the
// newest heading until something bumps the version again -- and the formatter
// refusing at that point would fail the action for the ordinary case.
func TestTheFormatterIsANoOpWhenTheEntryIsAlreadyFiled(t *testing.T) {
	cli := changesetCLI(t)
	dir := versionWorkspace(t)

	version := exec.Command(cli, "version")
	version.Dir = dir
	if out, err := version.CombinedOutput(); err != nil {
		t.Fatalf("changeset version: %v\n%s", err, out)
	}

	format := func() (int, string) {
		cmd := exec.Command("node", filepath.Join(repoRoot(t), "scripts", "changelog-format.mjs"),
			filepath.Join(dir, "npm", "CHANGELOG.md"), filepath.Join(dir, "CHANGELOG.md"))
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "AGENT_GM_CHANGELOG_DATE=2026-09-08")
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("running changelog-format.mjs: %v\n%s", err, out)
			}
			code = ee.ExitCode()
		}
		return code, string(out)
	}

	if code, out := format(); code != 0 {
		t.Fatalf("the first run failed:\n%s", out)
	}
	first, err := os.ReadFile(filepath.Join(dir, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}

	code, out := format()
	if code != 0 {
		t.Fatalf("a second run failed instead of doing nothing:\n%s", out)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("the second run did not say it had nothing to do:\n%s", out)
	}
	second, err := os.ReadFile(filepath.Join(dir, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("the second run edited CHANGELOG.md:\n%s", second)
	}
	if strings.Count(string(second), "## [1.0.2]") != 1 {
		t.Errorf("1.0.2 was filed twice:\n%s", second)
	}
	// And it still left the package's changelog where the action reads it.
	if _, err := os.Stat(filepath.Join(dir, "npm", "CHANGELOG.md")); err != nil {
		t.Errorf("npm/CHANGELOG.md was removed by the second run: %v", err)
	}
}
