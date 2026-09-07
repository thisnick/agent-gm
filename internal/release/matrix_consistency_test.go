package release_test

// Reviewer tests, slice 4. Everything in `scripts/release.sh` past `build` --
// the target matrix, the release-note template, the publish upload set -- is
// exercised only on release day, and release day is a tag, which is a thing
// that happens once. A reviewer's planted mutations proved it: dropping
// `agm:darwin:amd64` from the matrix, renaming the workflow in the cosign
// verify command the notes print, and uploading only `checksums.txt` instead
// of the archives all left `devbox run check` and `devbox run release-dry-run`
// green.
//
// Each of those is a release that ships and is wrong in a way nobody can fix
// after the fact: an Intel Mac installing `@agent-gm/cli` 404s, a reader who
// follows the printed `cosign verify-blob` gets a failure that looks like a
// forged binary, and the release page has no binaries on it.
//
// These tests read the shipped files as text, which is the only authority
// there is for a shell script.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// Spec section 14.3 lists the release assets by name. Six archives: the
// server on linux only, the CLI on all four platforms.
func TestReleaseScriptBuildsExactlyTheSixArchivesOfSection143(t *testing.T) {
	sh := repoFile(t, "scripts/release.sh")

	block := regexp.MustCompile(`(?s)targets=\((.*?)\n\)`).FindStringSubmatch(sh)
	if block == nil {
		t.Fatal("scripts/release.sh declares no targets=( … ) array")
	}
	got := map[string]bool{}
	for _, line := range strings.Split(block[1], "\n") {
		line = strings.TrimSpace(strings.Trim(strings.TrimSpace(line), `"`))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		got[line] = true
	}

	want := []string{
		"agent-gm:linux:amd64",
		"agent-gm:linux:arm64",
		"agm:linux:amd64",
		"agm:linux:arm64",
		"agm:darwin:amd64",
		"agm:darwin:arm64",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("scripts/release.sh does not build %s; section 14.3 lists "+
				"%s_X.Y.Z_%s.tar.gz as a release asset", w,
				strings.SplitN(w, ":", 2)[0],
				strings.ReplaceAll(strings.SplitN(w, ":", 2)[1], ":", "_"))
		}
		delete(got, w)
	}
	for extra := range got {
		t.Errorf("scripts/release.sh builds %s, which section 14.3 does not list", extra)
	}
}

// The npm wrapper asks the release page for `agm_<version>_<goos>_<goarch>`.
// If the release did not build that pair, an install on that platform 404s at
// postinstall -- after npm has already told the user it succeeded in fetching
// the package. The two lists have to be the same list.
func TestEveryPlatformTheWrapperSupportsIsAPlatformTheReleaseBuilds(t *testing.T) {
	sh := repoFile(t, "scripts/release.sh")
	js := repoFile(t, "npm/scripts/platform.js")

	pairs := regexp.MustCompile(`goos:\s*"(\w+)",\s*goarch:\s*"(\w+)"`).FindAllStringSubmatch(js, -1)
	if len(pairs) == 0 {
		t.Fatal("npm/scripts/platform.js declares no SUPPORTED goos/goarch pairs")
	}
	for _, p := range pairs {
		target := "agm:" + p[1] + ":" + p[2]
		if !strings.Contains(sh, `"`+target+`"`) {
			t.Errorf("npm/scripts/platform.js offers %s/%s but scripts/release.sh does not "+
				"build %s: `npm i -g @agent-gm/cli` on that platform downloads a URL "+
				"that is not on the release page", p[1], p[2], target)
		}
	}
}

// The release note prints a `cosign verify-blob` for the reader to run. Its
// --certificate-identity-regexp has to name the workflow that actually did
// the signing, or the command the notes hand out fails against a perfectly
// good signature -- which reads exactly like a compromised release.
func TestTheNotesCosignIdentityNamesTheWorkflowThatSigns(t *testing.T) {
	sh := repoFile(t, "scripts/release.sh")

	m := regexp.MustCompile(`--certificate-identity-regexp '([^']+)'`).FindStringSubmatch(sh)
	if m == nil {
		t.Fatal("the release note prints no --certificate-identity-regexp")
	}
	identity := m[1]

	// The workflow whose job runs `release.sh sign`.
	var signing string
	for _, name := range []string{"release.yml", "ci.yml"} {
		if strings.Contains(repoFile(t, ".github/workflows/"+name), "release.sh sign") {
			signing = name
		}
	}
	if signing == "" {
		t.Fatal("no workflow in .github/workflows runs `release.sh sign`")
	}
	// The regexp is a regexp, so `.` is escaped in the shipped string.
	wantFragment := ".github/workflows/" + strings.ReplaceAll(signing, ".", `\.`)
	if !strings.Contains(identity, wantFragment) {
		t.Errorf("the release note tells readers to verify against\n  %s\nbut the signature is "+
			"made by .github/workflows/%s. Every verification the notes ask for would fail.",
			identity, signing)
	}
	if !strings.Contains(identity, "@refs/tags/v") {
		t.Errorf("the identity %q does not pin the ref to a v tag, so a signature made by "+
			"this workflow on any branch would verify", identity)
	}
}

// `release.sh publish` is what puts the assets on the release page. Section
// 14.3 says the page carries the six archives, checksums.txt and the
// signature; a publish that uploads a subset is a release with nothing to
// download and no way to notice until somebody tries.
func TestPublishUploadsTheArchivesTheSignatureAndTheChecksums(t *testing.T) {
	sh := repoFile(t, "scripts/release.sh")
	m := regexp.MustCompile(`(?s)gh release create.*?\n\s*note "published`).FindString(sh)
	if m == "" {
		t.Fatal("scripts/release.sh has no `gh release create` in do_publish")
	}
	for _, asset := range []string{
		`"$dist"/*.tar.gz`,
		`"$dist/checksums.txt"`,
		`"$dist/checksums.txt.sig"`,
		`"$dist/checksums.txt.pem"`,
	} {
		if !strings.Contains(m, asset) {
			t.Errorf("`gh release create` does not upload %s; section 14.3 requires it "+
				"on the release page", asset)
		}
	}
}

// --- the reviewer's two plants, un-skipped as R-1 and R-2 landed --------------

// PLANT (reviewer, slice 4). `do_notes` defaults AGENT_GM_IMAGE_DIGEST to the
// literal string "unknown" and `do_publish` publishes it without complaint, so
// `devbox run release` -- build, sign, npm-pack, publish, which is the
// documented local release path and sets no digest -- writes a release note
// telling every deployment to pin `ghcr.io/thisnick/agent-gm@unknown`. The
// workflow's own comment says "a release note that says @unknown is worse than
// a release that waits", but that judgement lives in a YAML step and not in
// the script the note comes from.
//
// Unblocked by: `do_publish` refusing when the digest is absent or not a
// `sha256:` digest. Wire assumption: the variable stays AGENT_GM_IMAGE_DIGEST.
func TestPublishRefusesToRecordAnUnknownImageDigest(t *testing.T) {
	sh := repoFile(t, "scripts/release.sh")
	pub := regexp.MustCompile(`(?s)do_publish\(\) \{.*?\n\}`).FindString(sh)
	if !strings.Contains(pub, "AGENT_GM_IMAGE_DIGEST") || !strings.Contains(pub, "sha256:") {
		t.Error("do_publish does not refuse an absent or malformed image digest; a release " +
			"note that pins @unknown is a deployment instruction nobody can follow")
	}
}

// PLANT (reviewer, slice 4). `require_tag` only checks that the ref is a tag
// beginning with `v`, while every comment in the file says "only a vX.Y.Z tag
// publishes". `GITHUB_REF_TYPE=tag GITHUB_REF_NAME=vnonsense ./scripts/release.sh notes`
// produces "## agent-gm vnonsense", and `release.yml` fires on `tags: ["v*"]`,
// so a mistyped tag creates a real GitHub release before npm rejects the
// version.
//
// Unblocked by: `require_tag` matching the version against
// `^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`.
func TestOnlyASemverTagPublishes(t *testing.T) {
	sh := repoFile(t, "scripts/release.sh")
	req := regexp.MustCompile(`(?s)require_tag\(\) \{.*?\n\}`).FindString(sh)
	if !strings.Contains(req, "[0-9]") {
		t.Error("require_tag accepts any tag beginning with `v`; `vnonsense` would cut a release")
	}
}
