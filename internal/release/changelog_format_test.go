package release_test

// The changelog the Version Packages pull request arrives with.
//
// `changeset version` writes its entry into `npm/CHANGELOG.md`, in its own
// shape, under a heading of its own. This repository keeps ONE changelog, at
// the root, in one shape it has kept since 1.0.0. `scripts/changelog-format.mjs`
// is what reconciles those, and it runs unattended inside the action that opens
// the pull request -- so if it is wrong, the wrongness is committed by a bot
// and reviewed by whoever is in a hurry to cut a release.
//
// These run it against a fixture rather than against the real CHANGELOG.md,
// with the date pinned, so the assertions are about the transformation and not
// about what happens to be released this week.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureChangelog = `# Changelog

Agent GM follows [semantic versioning](https://semver.org).

## [Unreleased]

- something that was never released

## [1.0.1] — 2026-09-07

- the previous release

[Unreleased]: https://github.com/thisnick/agent-gm/compare/v1.0.1...HEAD
[1.0.1]: https://github.com/thisnick/agent-gm/releases/tag/v1.0.1
`

const fixtureWritten = `# @agent-gm/cli

## 1.0.2

### Patch Changes

- Versions are managed with changesets
- A second sentence
`

// runFormat runs the formatter over a pair of temporary files and returns the
// root changelog it produced.
func runFormat(t *testing.T, root, written string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "CHANGELOG.md")
	from := filepath.Join(dir, "npm-CHANGELOG.md")
	if err := os.WriteFile(target, []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}
	if written != "" {
		if err := os.WriteFile(from, []byte(written), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("node", filepath.Join(repoRoot(t), "scripts", "changelog-format.mjs"), from, target)
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
	produced, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	// The source is consumed: one repository, one changelog.
	if code == 0 && written != "" {
		if _, err := os.Stat(from); !os.IsNotExist(err) {
			t.Errorf("%s survived; two changelogs would be committed for one version", from)
		}
	}
	return code, string(out), string(produced)
}

func TestTheVersionEntryLandsInTheHouseFormat(t *testing.T) {
	code, log, got := runFormat(t, fixtureChangelog, fixtureWritten)
	if code != 0 {
		t.Fatalf("changelog-format failed:\n%s", log)
	}

	if !strings.Contains(got, "## [1.0.2] — 2026-09-08") {
		t.Errorf("the entry is not in the house heading format:\n%s", got)
	}
	if strings.Contains(got, "### Patch Changes") {
		t.Errorf("the bump-size subheading survived; the file has never used one:\n%s", got)
	}
	if !strings.Contains(got, "- Versions are managed with changesets") ||
		!strings.Contains(got, "- A second sentence") {
		t.Errorf("an entry was dropped:\n%s", got)
	}

	// Order: [Unreleased], then the new release, then the previous one. The
	// action inserts its entry after the FIRST LINE of the file, which is
	// above the preamble and above [Unreleased] -- so getting this wrong is
	// the default rather than an accident.
	iUnreleased := strings.Index(got, "## [Unreleased]")
	iNew := strings.Index(got, "## [1.0.2]")
	iOld := strings.Index(got, "## [1.0.1]")
	if iUnreleased >= iNew || iNew >= iOld {
		t.Errorf("the sections are out of order (Unreleased %d, 1.0.2 %d, 1.0.1 %d):\n%s",
			iUnreleased, iNew, iOld, got)
	}
	// The preamble stays above everything.
	if strings.Index(got, "semantic versioning") > iUnreleased {
		t.Errorf("the entry was inserted above the preamble:\n%s", got)
	}

	// [Unreleased] is emptied by the release that consumed it, or the same
	// sentence is published twice.
	unreleased := got[iUnreleased:iNew]
	if strings.Contains(unreleased, "something that was never released") {
		t.Errorf("[Unreleased] still lists what this release just took:\n%s", got)
	}
	if !strings.Contains(unreleased, "Nothing yet.") {
		t.Errorf("[Unreleased] was left without its placeholder:\n%s", got)
	}

	// The link references at the bottom: the comparison moves and the new
	// version gets its own release link.
	if !strings.Contains(got, "[Unreleased]: https://github.com/thisnick/agent-gm/compare/v1.0.2...HEAD") {
		t.Errorf("the [Unreleased] comparison still points at the old version:\n%s", got)
	}
	if !strings.Contains(got, "[1.0.2]: https://github.com/thisnick/agent-gm/releases/tag/v1.0.2") {
		t.Errorf("the new version has no link reference, so `## [1.0.2]` renders as literal "+
			"brackets:\n%s", got)
	}
	if !strings.Contains(got, "[1.0.1]: https://github.com/thisnick/agent-gm/releases/tag/v1.0.1") {
		t.Errorf("an older link reference was lost:\n%s", got)
	}
}

// A prerelease is filed the same way, because `changeset pre enter rc` is a
// mode of the same flow and not a separate one.
func TestAPrereleaseEntryIsFiledTheSameWay(t *testing.T) {
	written := strings.Replace(fixtureWritten, "## 1.0.2", "## 1.0.2-rc.0", 1)
	code, log, got := runFormat(t, fixtureChangelog, written)
	if code != 0 {
		t.Fatalf("changelog-format failed on a prerelease:\n%s", log)
	}
	if !strings.Contains(got, "## [1.0.2-rc.0] — 2026-09-08") {
		t.Errorf("the prerelease entry is not in the house format:\n%s", got)
	}
	if !strings.Contains(got, "[1.0.2-rc.0]: https://github.com/thisnick/agent-gm/releases/tag/v1.0.2-rc.0") {
		t.Errorf("the prerelease has no link reference:\n%s", got)
	}
}

// Run twice, or run on a push that versioned nothing, it must do nothing
// rather than something.
func TestTheFormatterIsANoOpWithNothingToFile(t *testing.T) {
	code, log, got := runFormat(t, fixtureChangelog, "")
	if code != 0 {
		t.Fatalf("changelog-format failed with nothing to do:\n%s", log)
	}
	if got != fixtureChangelog {
		t.Errorf("it edited CHANGELOG.md with no entry to file:\n%s", got)
	}
	if !strings.Contains(log, "nothing to do") {
		t.Errorf("it did not say it had nothing to do:\n%s", log)
	}
}

// And it refuses rather than duplicating: an entry for a version the changelog
// already has means something has gone wrong upstream, and appending a second
// one would leave two entries for one version in a file people read to find
// out what changed.
func TestTheFormatterRefusesToFileAVersionTwice(t *testing.T) {
	written := strings.Replace(fixtureWritten, "## 1.0.2", "## 1.0.1", 1)
	code, log, got := runFormat(t, fixtureChangelog, written)
	if code == 0 {
		t.Fatalf("it filed 1.0.1 twice:\n%s", got)
	}
	if !strings.Contains(log, "already has an entry") {
		t.Errorf("the refusal does not say why:\n%s", log)
	}
}

// The formatter is wired into the command the action runs, or it never runs.
func TestTheVersionCommandRunsTheFormatter(t *testing.T) {
	pkg := repoFile(t, "package.json")
	if !strings.Contains(pkg, "changeset version && node scripts/changelog-format.mjs") {
		t.Error("the `version-packages` script does not run changelog-format.mjs after " +
			"`changeset version`; the Version Packages pull request would arrive with two " +
			"changelogs in two shapes")
	}
	wf := repoFile(t, ".github/workflows/version.yml")
	if !strings.Contains(wf, "version: npm run version-packages") {
		t.Error("version.yml does not point the changesets action at `npm run version-packages`, " +
			"so it runs its own `changeset version` and the formatter never runs")
	}
}
