package release_test

// The changeset check, tested as the thing it is: a rule about which pull
// requests must carry a changeset. The rule is enforced on every pull request
// and is therefore the check most likely to be quietly loosened -- "it kept
// failing on docs" is how a required changeset becomes an optional one -- so
// each half of it is asserted separately: the docs-only pass, the code-change
// refusal, the label bypass, and the pass when a changeset is actually there.
//
// It runs `scripts/changeset-check.sh`, because the workflow runs that script
// and nothing else. Nothing here reaches GitHub: the two facts the workflow
// supplies (the changed paths, the labels) are supplied here instead, which is
// exactly why the judgement was put in a script rather than in YAML.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCheck feeds a set of changed paths and labels to the script.
func runCheck(t *testing.T, labels string, paths ...string) (int, string) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", "changeset-check.sh"))
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	cmd.Env = append(os.Environ(), "AGENT_GM_PR_LABELS="+labels)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running changeset-check.sh: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	return code, string(out)
}

// Prose does not ship in an archive, an image or the npm tarball. Requiring a
// version bump for a typo would either cut releases nobody wants or teach
// everyone to reach for the bypass label, which is how a bypass stops meaning
// anything.
func TestADocumentationOnlyPullRequestNeedsNoChangeset(t *testing.T) {
	docsOnly := [][]string{
		{"docs/operations.md"},
		{"README.md", "CONTRIBUTING.md", "CHANGELOG.md"},
		{"plans/AGENT_GM_SPEC.md", "docs/api.md", "docs/mcp/tools.md"},
	}
	for _, paths := range docsOnly {
		t.Run(strings.Join(paths, ","), func(t *testing.T) {
			code, out := runCheck(t, "", paths...)
			if code != 0 {
				t.Fatalf("a documentation-only pull request was refused:\n%s", out)
			}
			if !strings.Contains(out, "documentation only") {
				t.Errorf("the pass does not say why:\n%s", out)
			}
		})
	}
}

// The rule itself. Every one of these changes what is shipped or how it is
// shipped, and a release that omits it is a release whose changelog is wrong.
func TestAPullRequestThatShipsSomethingIsRefusedWithoutAChangeset(t *testing.T) {
	shipping := [][]string{
		{"internal/api/health.go"},
		{"cmd/agm/main.go", "docs/cli.md"},              // mixed: the code half decides
		{"Dockerfile"},                                  // how it is shipped
		{"devbox.json"},                                 // ditto
		{"scripts/release.sh"},                          // ditto
		{".github/workflows/release.yml"},               // ditto
		{"npm/scripts/postinstall.js"},                  // the wrapper is shipped
		{"npm/README.md"},                               // and so is its README
		{"LICENSE"},                                     // it is in every archive and in the tarball
		// V-3: a rename OUT of code and into docs. git collapses a rename to
		// its destination, so without --no-renames the pull request that
		// deleted a Go file would read as documentation. Both paths are what
		// the workflow now passes.
		{"internal/a.go", "docs/a.md"}, // the deleted source names itself in the refusal
		{"go.mod", "go.sum"},                            // a dependency bump ships
		{"internal/gm/pin.go", "plans/AGENT_GM_SPEC.md"}, // the pin most of all
	}
	for _, paths := range shipping {
		t.Run(strings.Join(paths, ","), func(t *testing.T) {
			code, out := runCheck(t, "", paths...)
			if code == 0 {
				t.Fatalf("a pull request changing %v was accepted with no changeset:\n%s", paths, out)
			}
			if !strings.Contains(out, "devbox run changeset") {
				t.Errorf("the refusal does not say how to fix it:\n%s", out)
			}
			// It names the paths that need one, so the author does not have to
			// guess which of their files tripped it.
			if !strings.Contains(out, paths[0]) {
				t.Errorf("the refusal does not name %s:\n%s", paths[0], out)
			}
		})
	}
}

// The bypass, which is deliberate and is meant to be visible: it says what it
// let through, in the log of the run that let it through.
func TestTheNoReleaseLabelIsTheWayPast(t *testing.T) {
	for _, labels := range []string{"no-release", "bug,no-release", "no-release,documentation"} {
		t.Run(labels, func(t *testing.T) {
			code, out := runCheck(t, labels, "internal/api/health.go")
			if code != 0 {
				t.Fatalf("the no-release label did not bypass the check:\n%s", out)
			}
			if !strings.Contains(out, "no-release") {
				t.Errorf("the bypass does not record itself:\n%s", out)
			}
		})
	}
	// And no other label does. `norelease`, `release` and an empty label set
	// are not the bypass, or the bypass is whatever anyone happens to type.
	for _, labels := range []string{"", "bug", "release", "norelease", "no release", "No-Release"} {
		t.Run("not:"+labels, func(t *testing.T) {
			code, out := runCheck(t, labels, "internal/api/health.go")
			if code == 0 {
				t.Fatalf("the label %q bypassed the check; only 'no-release' does:\n%s", labels, out)
			}
		})
	}
}

// The other direction: a changeset is what the check is asking for, so a pull
// request that has one passes -- whatever else it changed.
func TestAChangesetIsWhatTheCheckAccepts(t *testing.T) {
	code, out := runCheck(t, "", "internal/api/health.go", ".changeset/olive-planes-invent.md")
	if code != 0 {
		t.Fatalf("a pull request with a changeset was refused:\n%s", out)
	}
	if !strings.Contains(out, "carries a changeset") {
		t.Errorf("the pass does not say why:\n%s", out)
	}

	// The plumbing is not a changeset. Editing the config or the README of
	// `.changeset/` is a change to the release machinery, and passing on it
	// would let any code change through beside it.
	for _, plumbing := range []string{".changeset/config.json", ".changeset/README.md"} {
		t.Run(plumbing, func(t *testing.T) {
			code, out := runCheck(t, "", "internal/api/health.go", plumbing)
			if code == 0 {
				t.Fatalf("%s was accepted as a changeset:\n%s", plumbing, out)
			}
		})
	}
}

// The check is wired into ci, on pull requests, or it is a script nobody runs.
func TestTheChangesetCheckIsAJobOnEveryPullRequest(t *testing.T) {
	ci := repoFile(t, ".github/workflows/ci.yml")
	if !strings.Contains(ci, "changeset-check.sh") {
		t.Fatal("no job in ci.yml runs scripts/changeset-check.sh, so nothing enforces D39 " +
			"on a pull request")
	}
	if !strings.Contains(ci, "github.event.pull_request.labels") {
		t.Error("the changeset job does not pass the pull request's labels to the script, so " +
			"the documented 'no-release' bypass does not exist in practice")
	}
	if !strings.Contains(ci, "github.event.pull_request.base.sha") {
		t.Error("the changeset job does not diff against the pull request's base, so it is " +
			"asking about the wrong set of files")
	}
	// V-3. Without this, `git mv internal/a.go docs/a.md` reports `docs/a.md`
	// alone and the check calls a deleted Go file documentation.
	if !strings.Contains(ci, "--no-renames") {
		t.Error("the changeset job does not pass --no-renames, so git collapses a rename to " +
			"its destination: moving code under docs/ would classify as documentation-only")
	}
	// V-4. Three dots, the merge base -- the query the script documents and
	// the one these tests describe. A two-dot diff also carries everything
	// merged into main since the branch started.
	if !strings.Contains(ci, `"${base}...${head}"`) {
		t.Error("the changeset job uses a two-dot diff; it asks how the branch differs from " +
			"main today rather than what the branch changed")
	}
}
