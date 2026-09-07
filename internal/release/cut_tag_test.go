package release_test

// The tag that cuts a release, and the two ways cutting it is wrong.
//
// Under D39 nobody types the tag: `version.yml` cuts it when the Version
// Packages pull request merges and npm/package.json's version moves. That
// makes `scripts/cut-tag.sh` the thing standing between an ordinary merge and
// a public, signed, unwithdrawable release, and it is exercised for real
// exactly as often as a release happens -- which is why it is exercised here
// instead.
//
// Nothing here pushes a tag, dispatches a workflow or reaches a network: the
// script's whole job is to answer yes or no, and the answer is what is tested.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runCutTag(t *testing.T, previous string) (int, string) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", "cut-tag.sh"), previous)
	cmd.Dir = root
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running cut-tag.sh: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	return code, string(out)
}

// The ordinary case, which is most pushes to main: the version did not move,
// so this is not a release and nothing is cut.
func TestAnOrdinaryPushToMainCutsNoTag(t *testing.T) {
	current := manifestVersion(t)
	code, out := runCutTag(t, current)
	if code != 0 {
		t.Fatalf("cut-tag.sh failed on an ordinary push:\n%s", out)
	}
	if !strings.Contains(out, "release=0") {
		t.Errorf("cut-tag.sh did not answer release=0 for an unchanged version:\n%s", out)
	}
	if strings.Contains(out, "release=1") {
		t.Errorf("cut-tag.sh would have cut a tag for a push that released nothing:\n%s", out)
	}
}

// The guard that matters most. Every artefact of a released version is already
// public and immutable -- the release page, the cosign signature in a
// transparency log, the GHCR digest, the npm version -- so a second tag of the
// same name would name a commit that disagrees with all four. A revert and a
// re-merge that lands the same version is the way this happens by accident.
//
// The version and the tag are both invented here rather than borrowed from the
// repository's real history: a test that asserted "v1.0.1 is taken" would pass
// today and fail on the very commit that bumps the manifest to a version whose
// tag has not been cut yet -- which is the Version Packages commit, the one
// commit where this check must not be red.
func TestCutTagRefusesAVersionThatIsAlreadyTagged(t *testing.T) {
	const taken = "0.0.0-cuttagtest.1"
	withManifestVersion(t, taken)

	root := repoRoot(t)
	tag := "v" + taken
	// Lightweight, not annotated: an annotated tag needs a committer identity
	// and a CI runner has none, which failed the whole suite rather than this
	// one line. `cut-tag.sh` asks whether the ref exists, and both kinds do.
	if out, err := exec.Command("git", "-C", root, "tag", tag).CombinedOutput(); err != nil {
		t.Fatalf("creating the local tag %s: %v\n%s", tag, err, out)
	}
	t.Cleanup(func() {
		// Local only: it is never pushed, and it is removed whether this test
		// passed or failed.
		_ = exec.Command("git", "-C", root, "tag", "-d", tag).Run()
	})

	code, out := runCutTag(t, "0.0.0-cuttagtest.0") // the version "moved"
	if code == 0 {
		t.Fatalf("cut-tag.sh accepted %s, which is already tagged:\n%s", tag, out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("the refusal does not say the tag exists:\n%s", out)
	}
	if strings.Contains(out, "release=1") {
		t.Errorf("cut-tag.sh said release=1 before refusing; the workflow reads that line:\n%s", out)
	}

	// The other direction, with the same fabricated version and no tag: the
	// refusal above is the tag's doing and not the version's.
	_ = exec.Command("git", "-C", root, "tag", "-d", tag).Run()
	code, out = runCutTag(t, "0.0.0-cuttagtest.0")
	if code != 0 {
		t.Fatalf("cut-tag.sh refused a moved version whose tag is free:\n%s", out)
	}
	if !strings.Contains(out, "release=1") {
		t.Errorf("cut-tag.sh did not answer release=1 for a moved version with a free tag:\n%s", out)
	}
}

// The semver refusal is the manifest's, through `release.sh version`, so that
// there is one answer to "what is the version" and one place that refuses a
// bad one.
func TestCutTagRefusesAVersionThatIsNotSemver(t *testing.T) {
	for _, bad := range []string{"1.0", "01.0.2", "v1.0.2", "1.0.2.1", "next"} {
		t.Run(bad, func(t *testing.T) {
			withManifestVersion(t, bad)
			code, out := runCutTag(t, "1.0.0")
			if code == 0 {
				t.Fatalf("cut-tag.sh accepted the version %q:\n%s", bad, out)
			}
			if !strings.Contains(out, "MAJOR.MINOR.PATCH") {
				t.Errorf("the refusal does not say what shape it wanted:\n%s", out)
			}
		})
	}
}

// withManifestVersion rewrites the version authority for the duration of one
// test and puts it back. It is the only way to ask what happens when the
// manifest says something the release path must refuse -- the alternative is
// asserting the guard's source text, which is what a guard that has stopped
// working also looks like.
func withManifestVersion(t *testing.T, version string) {
	t.Helper()
	manifest := filepath.Join(repoRoot(t), "npm", "package.json")
	original, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(manifest, original, 0o644); err != nil {
			t.Fatalf("restoring npm/package.json: %v", err)
		}
	})
	current := manifestVersion(t)
	rewritten := strings.Replace(string(original),
		`"version": "`+current+`"`, `"version": "`+version+`"`, 1)
	if rewritten == string(original) {
		t.Fatalf("could not rewrite the version in npm/package.json (it says %q)", current)
	}
	if err := os.WriteFile(manifest, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The workflow that calls it, and the decision the task turned on: a tag
// pushed with the workflow's GITHUB_TOKEN starts no other workflow, so the two
// that a human's `git push origin v1.2.3` would have started are dispatched
// explicitly against the tag's own ref. Every guard in release.yml then runs
// against a real tag rather than against an input.
func TestTheVersionWorkflowCutsTheTagAndDispatchesTheReleasePath(t *testing.T) {
	wf := repoFile(t, ".github/workflows/version.yml")

	if !strings.Contains(wf, "changesets/action@") {
		t.Error("version.yml does not run the changesets action, so no Version Packages pull " +
			"request is ever opened and npm/package.json never moves")
	}
	if strings.Contains(wf, "publish:") {
		t.Error("version.yml gives the changesets action a publish command. Publishing is " +
			"release.yml's, off a tag, with the digest and green-ci guards; the changesets " +
			"action has none of them")
	}
	if !strings.Contains(wf, "cut-tag.sh") {
		t.Error("version.yml does not run scripts/cut-tag.sh, so the judgement about whether " +
			"this push is a release lives in YAML nobody can run")
	}
	if !strings.Contains(wf, `git push origin "refs/tags/v${VERSION}"`) {
		t.Error("version.yml does not push the annotated tag it created")
	}
	if !strings.Contains(wf, "git tag -a") {
		t.Error("the tag is not annotated; a lightweight tag records no author and no date")
	}
	// Both dispatches. Without the ci one there is no image for the tag, and
	// release.yml waits twenty minutes for an image that will never exist.
	if !strings.Contains(wf, `gh workflow run ci.yml --ref "v${VERSION}"`) {
		t.Error("version.yml does not dispatch ci.yml against the tag. A tag pushed with " +
			"GITHUB_TOKEN starts nothing, so no image would be built for it and release.yml " +
			"would time out waiting for the GHCR digest")
	}
	if !strings.Contains(wf, `gh workflow run release.yml --ref "v${VERSION}"`) {
		t.Error("version.yml does not dispatch release.yml against the tag, so the tag would " +
			"sit there and no release would be cut")
	}
	if !strings.Contains(wf, "actions: write") {
		t.Error("the tag job cannot dispatch anything without `actions: write`")
	}
	// It waits for ci rather than letting release.yml refuse a commit whose ci
	// is still running.
	if !strings.Contains(wf, "head_sha=") {
		t.Error("version.yml does not wait for this commit's ci run; release.yml refuses a " +
			"commit with no green ci, and seconds after a merge there is none yet")
	}
}

// ci.yml must accept the dispatch and must push the image on it, or the tag's
// image is never built.
func TestCiBuildsTheImageForADispatchedTag(t *testing.T) {
	ci := repoFile(t, ".github/workflows/ci.yml")
	if !strings.Contains(ci, "workflow_dispatch:") {
		t.Fatal("ci.yml has no workflow_dispatch trigger, so version.yml cannot ask it to " +
			"build the image for the tag it just pushed")
	}
	if !strings.Contains(ci, "github.event_name == 'workflow_dispatch'") {
		t.Error("ci.yml's image job does not push on a workflow_dispatch, so a dispatched run " +
			"for the tag builds the image and throws it away")
	}
}

// The emergency path: a maintainer pushing `vX.Y.Z` by hand still reaches
// release.yml with every guard, plus the one D39 adds.
func TestTheManualTagPathStillCarriesEveryGuard(t *testing.T) {
	wf := repoFile(t, ".github/workflows/release.yml")
	for _, want := range []string{
		`tags: ["v*"]`,
		`^v[0-9]+\.[0-9]+\.[0-9]+`,
		`git merge-base --is-ancestor "${GITHUB_SHA}" origin/main`,
		`.conclusion == "success"`,
		"org.opencontainers.image.revision",
		"./scripts/release.sh version",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("release.yml no longer contains %q; the emergency path has lost a guard", want)
		}
	}
	if !strings.Contains(wf, `"v${manifest}"`) {
		t.Error("release.yml does not assert the tag equals npm/package.json's version. On the " +
			"manual path the tag is typed, and a typed tag can name a version the binaries " +
			"were not stamped with")
	}
}

// And the script refuses the same mismatch, so the assertion survives somebody
// running `devbox run release` by hand off a tag.
func TestReleaseScriptRefusesATagThatDisagreesWithTheManifest(t *testing.T) {
	current := manifestVersion(t)
	for _, mode := range []string{"sign", "publish", "npm-publish"} {
		code, out := runRelease(t, []string{
			"GITHUB_REF_TYPE=tag",
			"GITHUB_REF_NAME=v9.9.9",
		}, mode)
		if code == 0 {
			t.Fatalf("release.sh %s accepted v9.9.9 while the manifest says %s:\n%s", mode, current, out)
		}
		if !strings.Contains(out, current) || !strings.Contains(out, "v9.9.9") {
			t.Errorf("%s's refusal does not name both the tag and the manifest version:\n%s", mode, out)
		}
	}
}
