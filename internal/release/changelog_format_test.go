package release_test

// The changelog the Version Packages pull request arrives with.
//
// `changeset version` writes its entry into `npm/CHANGELOG.md`, in its own
// shape, under a heading of its own. The changelog this repository points a
// reader at is the one at the root, in one shape it has kept since 1.0.0;
// `npm/CHANGELOG.md` stays where changesets put it, because
// `changesets/action` reads it to compose the Version Packages pull request.
// `scripts/changelog-format.mjs` is what reconciles those, and it runs
// unattended inside the action that opens the pull request -- so if it is
// wrong, the wrongness is committed by a bot and reviewed by whoever is in a
// hurry to cut a release.
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
	// The source SURVIVES. `changesets/action` reads the package's own
	// changelog to compose the Version Packages pull request body, and
	// removing it failed the first real run of version.yml
	// (`version_step_test.go` runs that sequence end to end).
	if code == 0 && written != "" {
		if _, err := os.Stat(from); err != nil {
			t.Errorf("%s was removed; changesets/action reads it to build the pull request "+
				"body and would fail with ENOENT: %v", from, err)
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

	// A changeset committed in an earlier pull request comes back from the
	// generator with its short commit in front of the sentence, and one added
	// in this pull request does not -- two shapes in one list, and a commit is
	// not what this file is for.
	withHash := strings.Replace(fixtureWritten,
		"- Versions are managed", "- 7fae2a9: Versions are managed", 1)
	code, log, got = runFormat(t, fixtureChangelog, withHash)
	if code != 0 {
		t.Fatalf("changelog-format failed:\n%s", log)
	}
	if strings.Contains(got, "7fae2a9") {
		t.Errorf("a bullet kept its commit prefix:\n%s", got)
	}
	if !strings.Contains(got, "- Versions are managed with changesets") {
		t.Errorf("stripping the commit prefix damaged the sentence:\n%s", got)
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

// V-5. Anything a maintainer wrote under [Unreleased] by hand describes
// something that has not shipped, and this is the release that ships it, so it
// is carried into the entry rather than deleted. Emptying the section silently
// lost a note somebody wrote on purpose.
func TestHandWrittenUnreleasedNotesSurviveIntoTheEntry(t *testing.T) {
	const note = "- a hand written note that must survive"
	root := strings.Replace(fixtureChangelog,
		"- something that was never released", note, 1)

	code, log, got := runFormat(t, root, fixtureWritten)
	if code != 0 {
		t.Fatalf("changelog-format failed:\n%s", log)
	}
	if !strings.Contains(got, note) {
		t.Fatalf("the hand-written note was deleted:\n%s", got)
	}

	// In the entry for this version, above the generated bullets -- not left
	// behind under [Unreleased], which would publish it again next time.
	iNew := strings.Index(got, "## [1.0.2]")
	iOld := strings.Index(got, "## [1.0.1]")
	iNote := strings.Index(got, note)
	if iNote < iNew || iNote > iOld {
		t.Errorf("the note is not inside the 1.0.2 entry (note %d, 1.0.2 %d, 1.0.1 %d):\n%s",
			iNote, iNew, iOld, got)
	}
	if iNote > strings.Index(got, "- Versions are managed with changesets") {
		t.Errorf("the hand-written note was filed below the generated bullets:\n%s", got)
	}
	if strings.Contains(got[strings.Index(got, "## [Unreleased]"):iNew], note) {
		t.Errorf("the note was left under [Unreleased] as well:\n%s", got)
	}
	// The placeholder is recognised as a placeholder and not carried.
	if strings.Count(got, "Nothing yet.") != 1 {
		t.Errorf("the `Nothing yet.` placeholder was carried into the entry:\n%s", got)
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

// An entry the root changelog already has is left alone, and the run is a
// success rather than a failure. `npm/CHANGELOG.md` is kept now, so its newest
// heading stays the newest heading until the next version bump: a version that
// is already filed is the ordinary shape of a second run, and refusing it
// would fail the action for the ordinary case. What must not happen is a
// second entry for one version in the file people read.
func TestTheFormatterFilesAVersionOnlyOnce(t *testing.T) {
	written := strings.Replace(fixtureWritten, "## 1.0.2", "## 1.0.1", 1)
	code, log, got := runFormat(t, fixtureChangelog, written)
	if code != 0 {
		t.Fatalf("an already-filed version failed the run:\n%s", log)
	}
	if strings.Count(got, "## [1.0.1]") != 1 {
		t.Fatalf("1.0.1 was filed twice:\n%s", got)
	}
	if got != fixtureChangelog {
		t.Errorf("CHANGELOG.md was edited for a version it already carries:\n%s", got)
	}
	if !strings.Contains(log, "already has the entry") {
		t.Errorf("the run does not say why it did nothing:\n%s", log)
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
