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
func TestReleaseScriptAcceptsARealVersionAndThenAsksForTheDigest(t *testing.T) {
	for _, tag := range []string{"v1.0.0", "v0.1.2", "v10.20.30", "v1.0.0-rc.1", "v1.0.0-alpha1"} {
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
			env := []string{"GITHUB_REF_TYPE=tag", "GITHUB_REF_NAME=v1.0.0"}
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
		"GITHUB_REF_NAME=v1.0.0",
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

	code, out := runRelease(t, []string{
		"GITHUB_REF_TYPE=tag",
		"GITHUB_REF_NAME=v1.0.0",
	}, "npm-publish")
	if code == 0 {
		t.Fatalf("npm-publish accepted a 0.0.0-dev tarball while releasing 1.0.0:\n%s", out)
	}
	if !strings.Contains(out, "0.0.0-dev.deadbee") || !strings.Contains(out, "1.0.0") {
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
