package release_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tag path, RUN rather than read.
//
// `internal/release/matrix_consistency_test.go` asserts the guards exist by
// reading `scripts/release.sh` as text, which is what a reviewer could write
// without a tag to push. These run the script, because a guard that is
// present in the source and unreachable at run time reads exactly like a
// guard that works -- and this is the one path CI can never exercise for
// real, so the closest thing to exercising it is worth having.
//
// Nothing here builds, signs, publishes or reaches a network: every case is
// expected to DIE, and dies before the first artefact.

// runRelease invokes scripts/release.sh with a forged ref and returns its exit
// code and combined output.
func runRelease(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", "release.sh"), args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running release.sh %v: %v\n%s", args, err, out)
		}
		code = ee.ExitCode()
	}
	return code, string(out)
}

// R-1, executed. `v*` is what the workflow fires on, so every one of these
// reaches the script. Each must be refused before cosign is invoked: a
// keyless signature goes into a public transparency log and cannot be taken
// back, and `gh release create vtest` succeeds long before `npm publish`
// finally objects to the version.
func TestReleaseScriptRefusesATagThatIsNotAVersion(t *testing.T) {
	notVersions := []string{
		"vnonsense",
		"vtest",
		"v1",
		"v1.0",
		"v1.0.0.1",
		"v1.0.0 ",
		"v01.0.0-",
		"vlatest",
		"v-1.0.0",
	}
	for _, tag := range notVersions {
		t.Run(tag, func(t *testing.T) {
			for _, mode := range []string{"sign", "publish", "npm-publish"} {
				code, out := runRelease(t, []string{
					"GITHUB_REF_TYPE=tag",
					"GITHUB_REF_NAME=" + tag,
				}, mode)
				if code == 0 {
					t.Fatalf("release.sh %s accepted the tag %q\n%s", mode, tag, out)
				}
				if !strings.Contains(out, tag) {
					t.Errorf("%s's refusal does not name the tag:\n%s", mode, out)
				}
				if !strings.Contains(out, "vMAJOR.MINOR.PATCH") {
					t.Errorf("%s's refusal does not say what shape it wanted:\n%s", mode, out)
				}
			}
		})
	}
}

// The other direction, so the test above is known to be refusing for the
// right reason rather than refusing everything. A real version gets PAST the
// tag check -- and then dies on the next guard, which is the digest, not on
// the tag.
//
// "A real version" is now exactly one string: `v` plus what npm/package.json
// says, because D39 made the manifest the version authority and a tag that
// disagrees with it is refused between the shape check and the digest check
// (`cut_tag_test.go` covers that refusal). Before D39 this listed five
// well-shaped tags; the shapes themselves are still asserted, against the
// pattern, in `prerelease_test.go`.
func TestReleaseScriptAcceptsARealVersionAndThenAsksForTheDigest(t *testing.T) {
	for _, tag := range []string{"v" + manifestVersion(t)} {
		t.Run(tag, func(t *testing.T) {
			code, out := runRelease(t, []string{
				"GITHUB_REF_TYPE=tag",
				"GITHUB_REF_NAME=" + tag,
			}, "publish")
			if code == 0 {
				t.Fatalf("publish succeeded with no digest and no artefacts:\n%s", out)
			}
			if strings.Contains(out, "vMAJOR.MINOR.PATCH") {
				t.Fatalf("%s was refused as a bad version; it is a good one:\n%s", tag, out)
			}
			if !strings.Contains(out, "AGENT_GM_IMAGE_DIGEST") {
				t.Errorf("a valid tag did not reach the digest guard:\n%s", out)
			}
		})
	}
}

// R-2, executed. `do_notes` defaults the digest to the literal `unknown`, and
// `devbox run release` -- the documented local path -- sets none at all. Both
// must die before a release exists.
func TestPublishRefusesADigestThatIsAbsentOrMalformed(t *testing.T) {
	cases := []struct {
		name   string
		digest string
		want   string
	}{
		{"unset", "", "unset"},
		{"the literal unknown", "AGENT_GM_IMAGE_DIGEST=unknown", "not a sha256"},
		{"a bare hex string", "AGENT_GM_IMAGE_DIGEST=" + strings.Repeat("a", 64), "not a sha256"},
		{"too short", "AGENT_GM_IMAGE_DIGEST=sha256:abc123", "not a sha256"},
		{"the wrong algorithm", "AGENT_GM_IMAGE_DIGEST=sha512:" + strings.Repeat("a", 64), "not a sha256"},
		{"a tag, not a digest", "AGENT_GM_IMAGE_DIGEST=v1.0.0", "not a sha256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{"GITHUB_REF_TYPE=tag", "GITHUB_REF_NAME=v" + manifestVersion(t)}
			if tc.digest != "" {
				env = append(env, tc.digest)
			}
			code, out := runRelease(t, env, "publish")
			if code == 0 {
				t.Fatalf("publish accepted the digest %q\n%s", tc.digest, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the refusal does not say %q:\n%s", tc.want, out)
			}
			// And it dies before it can create anything.
			if strings.Contains(out, "published") {
				t.Errorf("something was published:\n%s", out)
			}
		})
	}
}

// A well-shaped digest passes the shape check and is then asked to RESOLVE,
// which is the second half of R-2 and the whole of R-6. Without docker on the
// machine the resolution cannot be attempted, and the script says so rather
// than skipping the check -- "docker is missing" must not read as "the digest
// is fine".
func TestPublishAsksAWellShapedDigestToResolve(t *testing.T) {
	code, out := runRelease(t, []string{
		"GITHUB_REF_TYPE=tag",
		"GITHUB_REF_NAME=v" + manifestVersion(t),
		"AGENT_GM_IMAGE_DIGEST=sha256:" + strings.Repeat("0", 64),
	}, "publish")
	if code == 0 {
		t.Fatalf("publish accepted a digest that names no real image:\n%s", out)
	}
	// Either it resolved and failed, or docker is absent and it said so.
	if !strings.Contains(out, "does not resolve") && !strings.Contains(out, "docker is not on PATH") {
		t.Errorf("the refusal is neither a failed resolution nor a missing docker:\n%s", out)
	}
	if strings.Contains(out, "published") {
		t.Errorf("something was published:\n%s", out)
	}
}

// R-7, executed. `devbox run release-npm-publish` against a stale `dist/`
// must not publish a tarball from another build. The check is on the NAME,
// which carries the version, so a leftover `0.0.0-dev.*` cannot win a
// lexicographic sort the way `ls | head -1` let it.
func TestNpmPublishRefusesATarballFromAnotherVersion(t *testing.T) {
	root := repoRoot(t)
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dist, "npm-tarball.txt")
	stale := filepath.Join(dist, "agent-gm-cli-0.0.0-dev.deadbee.tgz")

	// Leave the tree as it was found: dist/ is gitignored but a developer may
	// have a real build in it.
	prev, hadPrev := os.ReadFile(record)
	t.Cleanup(func() {
		if hadPrev == nil {
			_ = os.WriteFile(record, prev, 0o644)
		} else {
			_ = os.Remove(record)
		}
		_ = os.Remove(stale)
	})

	if err := os.WriteFile(stale, []byte("not really a tarball"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, []byte(stale+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	version := manifestVersion(t)
	code, out := runRelease(t, []string{
		"GITHUB_REF_TYPE=tag",
		"GITHUB_REF_NAME=v" + version,
	}, "npm-publish")
	if code == 0 {
		t.Fatalf("npm-publish accepted a 0.0.0-dev tarball while releasing %s:\n%s", version, out)
	}
	if !strings.Contains(out, "0.0.0-dev.deadbee") || !strings.Contains(out, version) {
		t.Errorf("the refusal does not name both versions:\n%s", out)
	}
}

// The image label and the guard that reads it, tied together.
//
// `require_digest` refuses a digest whose image was not built from the commit
// being released, and it asks that question by reading
// `org.opencontainers.image.revision` off the image. If the Dockerfile stops
// setting the label, every release fails at publish time -- loudly, which is
// better than silently, but on the one day of the year when a loud failure is
// most expensive and least welcome.
//
// Found by a plant of my own: dropping the LABEL line survived the whole
// suite and the dry run. Planted 2026-09-07.
func TestTheImageCarriesTheRevisionLabelTheDigestGuardReads(t *testing.T) {
	dockerfile := repoFile(t, "Dockerfile")
	sh := repoFile(t, "scripts/release.sh")

	const label = "org.opencontainers.image.revision"
	if !strings.Contains(sh, label) {
		t.Fatalf("scripts/release.sh no longer reads %s, so this test is asserting "+
			"a label nothing uses", label)
	}
	if !strings.Contains(dockerfile, label) {
		t.Errorf("the Dockerfile does not set %s, but scripts/release.sh refuses to publish "+
			"without it: every release would die at the digest guard", label)
	}
	// And it must be set from the COMMIT build argument rather than to a
	// literal, or the guard would compare the commit against a constant and
	// pass for every image ever built.
	if !strings.Contains(dockerfile, label+`="${COMMIT}"`) {
		t.Errorf("%s is not set from the COMMIT build argument; the digest guard would "+
			"compare the released commit against a constant", label)
	}
	// The label lives in the RUNTIME stage. Set in the build stage it would
	// never reach the published image, and the guard would fail on every
	// release with a message about a missing label.
	runtime := dockerfile[strings.LastIndex(dockerfile, "FROM gcr.io/distroless"):]
	if !strings.Contains(runtime, label) {
		t.Errorf("%s is set before the final FROM, so it does not reach the published image", label)
	}
}

// --- the YAML, which nothing was asserting ------------------------------------
//
// R-10. `tag_guards_test.go` runs `scripts/release.sh` for real, which is a
// genuine advance over reading it -- but the workflow that CALLS it is a file
// no test had ever opened, and R-9 was sitting in exactly that gap: a jq
// expression that could never return `success`, in a step that would have
// refused every release, discovered by a reviewer reading it rather than by
// anything here.
//
// A workflow cannot be executed off GitHub, so these are text assertions.
// That is weaker than running it and it is what there is; the point is that
// the decisions are encoded rather than remembered.

// The three questions the release job asks before it builds anything, and the
// shape of the third one, which is the one that was wrong.
func TestTheReleaseWorkflowGatesTheTagOnMainAndOnGreen(t *testing.T) {
	wf := repoFile(t, ".github/workflows/release.yml")

	// The semver check, in the workflow as well as in the script: the script
	// refuses too, but only after devbox has installed and the build has run.
	if !strings.Contains(wf, `^v[0-9]+\.[0-9]+\.[0-9]+`) &&
		!strings.Contains(wf, `^v(0|[1-9][0-9]*)\.`) {
		t.Error("the release workflow does not check the tag's shape before building; " +
			"`vtest` would spend six minutes building before scripts/release.sh refused it")
	}

	// R-5, asserted as ONE string rather than three (R-12).
	//
	// Two shapes got past the looser version, and the reviewer found both.
	// `origin/main` was checked independently of the ancestry check, so
	// pointing the check at `origin/any-branch` survived -- `origin/main` was
	// still on the next line, in the error message. And `if false && ! git
	// merge-base …` survived, because "does the text contain it" is still yes
	// when the text is there and unreachable.
	//
	// The whole condition, matched as written, closes both: the check, its
	// subject and its target are one fact and are asserted as one.
	const ancestry = `if ! git merge-base --is-ancestor "${GITHUB_SHA}" origin/main; then`
	if !strings.Contains(wf, ancestry) {
		t.Error("the release workflow does not assert, exactly, that the tagged commit is an " +
			"ancestor of origin/main. A tag on an unmerged branch would build, sign and " +
			"publish (spec section 16 makes the tag an act on a reviewed commit), and a " +
			"check aimed at another branch, or short-circuited, reads the same from a " +
			"distance:\n  want: " + ancestry)
	}
	if !strings.Contains(wf, "git fetch --no-tags origin main") {
		t.Error("the workflow does not fetch origin/main before asking whether the commit " +
			"is on it; on a fresh checkout the ref would not be there to compare against")
	}

	// R-9: the question must be "did ANY ci push run for this sha go green",
	// not "what did the newest one conclude". `ci.yml` fires on `v*` tags
	// too, so the tag starts a SECOND ci push run for the same head_sha which
	// is still in progress while this step asks. Sorting by start time picks
	// that run, reads null, and refuses every release -- including a green
	// one, and including the first one anybody tries.
	if !strings.Contains(wf, `head_sha=`) {
		t.Fatal("the release workflow does not look up any run by head_sha")
	}
	if strings.Contains(wf, "sort_by(.run_started_at)") {
		t.Error("the ci gate takes the NEWEST run for the sha. `ci.yml` also fires on the " +
			"tag, so that run is the tag's own and is still in progress: the gate reads " +
			"null and no release can ever be cut (R-9)")
	}
	if !strings.Contains(wf, `.conclusion == "success"`) {
		t.Error(`the ci gate does not select on .conclusion == "success"; it must ask ` +
			`whether ANY ci push run for this sha went green`)
	}
	if !strings.Contains(wf, `select(.name == "ci" and .event == "push"`) {
		t.Error("the ci gate does not restrict to the `ci` workflow's push runs")
	}
}

// The label the digest guard reads is set by the Dockerfile from `${COMMIT}`,
// and `${COMMIT}` is set by whoever builds the image. If `image.sh` stops
// passing it, the label ships EMPTY: the Dockerfile test still passes, the
// dry run still passes, and `require_digest` dies on release day with a
// message about a missing label, which is the last place anybody wants to be
// debugging a build argument. R-10, from a plant of the reviewer's that
// survived the whole suite.
func TestTheImageBuildPassesTheCommitTheLabelRecords(t *testing.T) {
	sh := repoFile(t, "scripts/image.sh")

	if !strings.Contains(sh, `--build-arg "COMMIT=$commit"`) &&
		!strings.Contains(sh, `--build-arg COMMIT=$commit`) {
		t.Error("scripts/image.sh does not pass COMMIT=$commit as a build argument. " +
			"The Dockerfile sets org.opencontainers.image.revision from ${COMMIT}, so the " +
			"published image would carry an empty revision and scripts/release.sh would " +
			"refuse its digest on release day")
	}
	// And $commit must be the real thing rather than a placeholder.
	if !strings.Contains(sh, `commit="${GITHUB_SHA:-$(git rev-parse HEAD)}"`) {
		t.Error("scripts/image.sh no longer derives $commit from GITHUB_SHA or the working " +
			"tree; the revision label would name something that is not the built commit")
	}
	if !strings.Contains(sh, `--build-arg "VERSION=$version"`) &&
		!strings.Contains(sh, `--build-arg VERSION=$version`) {
		t.Error("scripts/image.sh does not pass VERSION as a build argument")
	}
}
