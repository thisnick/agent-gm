package release_test

// Prereleases, and the two tags that must not move for one.
//
// `changeset pre enter rc` makes the next version `X.Y.Z-rc.N`, and everything
// downstream keeps working by accident right up to the two places where a
// default is wrong:
//
//   - `docker pull ghcr.io/thisnick/agent-gm` with no tag resolves `latest`,
//     and `:vX.Y` is what a deployment that follows a minor series pins. A
//     release candidate must take neither, or every such puller is upgraded to
//     a candidate they did not ask for -- silently, because a tag move leaves
//     no trace on the page anybody reads.
//   - `npm i @agent-gm/cli` with no version resolves the `latest` dist-tag.
//     npm's DEFAULT for `npm publish` is `latest`, so doing nothing is exactly
//     the wrong thing; `next` is what npm callers use to ask for a candidate.
//
// Neither can be tested by publishing one, so the tests are on the two scripts
// that decide it -- and the arithmetic bug that hid underneath is worth stating
// plainly: `${version%.*}` computes the minor series by chopping the last dot
// segment, and on `1.0.2-rc.0` it produces `1.0.2-rc`, which is not a series at
// all and is a tag nobody would ever pull.

import (
	"regexp"
	"strings"
	"testing"
)

// The image tags, out of scripts/image.sh's own case statement.
func TestAPrereleaseImageIsNotTaggedLatestOrByMinor(t *testing.T) {
	sh := repoFile(t, "scripts/image.sh")

	block := regexp.MustCompile(`(?s)case "\$ref_type:\$ref_name" in(.*?)\nesac`).FindStringSubmatch(sh)
	if block == nil {
		t.Fatal("scripts/image.sh no longer decides its tags in a `case $ref_type:$ref_name` block")
	}
	tagging := block[1]

	// The prerelease arm exists and is the narrow one.
	if !strings.Contains(tagging, `*-*) tags=("$ref_name") ;;`) {
		t.Error("scripts/image.sh has no arm that tags a prerelease with the tag alone. " +
			"`vX.Y.Z-rc.N` would then also be pushed as `latest` and as `vX.Y`, and every " +
			"deployment that pulls either would be moved onto a release candidate")
	}
	// And the wide one is reachable only for a release.
	release := regexp.MustCompile(`\*\)\s+tags=\("\$ref_name" "v\$\{version%\.\*\}" "latest"\)`)
	if !release.MatchString(tagging) {
		t.Error("scripts/image.sh no longer tags an ordinary release `vX.Y.Z`, `vX.Y` and " +
			"`latest` (section 14.2)")
	}
	// The order matters: a `*)` arm before `*-*)` would swallow every
	// prerelease, and the file would still read as though the rule were there.
	if strings.Index(tagging, `*-*)`) > strings.Index(tagging, `"latest")`) {
		t.Error("the prerelease arm comes after the arm that adds `latest`, so it never runs")
	}
}

// The npm dist-tag, out of scripts/release.sh.
func TestAPrereleaseIsPublishedUnderTheNextDistTag(t *testing.T) {
	sh := repoFile(t, "scripts/release.sh")

	if !strings.Contains(sh, `case "$version" in *-*) dist_tag=next ;; esac`) {
		t.Error("scripts/release.sh does not switch the npm dist-tag to `next` for a " +
			"prerelease. `npm publish` defaults to `latest`, so `npm i @agent-gm/cli` would " +
			"install a release candidate")
	}
	if !strings.Contains(sh, `npm publish --access public --provenance --tag "$dist_tag"`) {
		t.Error("scripts/release.sh does not pass --tag to `npm publish`; the dist-tag it " +
			"computed is not the one it uses")
	}
	// Provenance stays mandatory alongside the dist-tag.
	if !strings.Contains(sh, "--provenance") {
		t.Error("`npm publish` no longer passes --provenance")
	}
}

// The release note tells the reader which tags moved, and for a prerelease the
// honest answer is "one". A note that claims `latest` points at a release
// candidate is worse than no note: it is an instruction to deploy one.
func TestTheReleaseNoteDoesNotClaimLatestForAPrerelease(t *testing.T) {
	sh := repoFile(t, "scripts/release.sh")
	if !strings.Contains(sh, `case "$version" in
    *-*) tagline=`) {
		t.Error("scripts/release.sh's release note has no prerelease form; it would tell every " +
			"reader of an rc that `latest` and `vX.Y` point at it")
	}
	if !strings.Contains(sh, "$tagline") {
		t.Error("the note template does not use the tagline that was computed for it")
	}

	// And the note it prints for a real version still names all three.
	code, out := runRelease(t, []string{
		"AGENT_GM_IMAGE_DIGEST=sha256:" + strings.Repeat("a", 64),
	}, "notes")
	if code != 0 {
		t.Fatalf("release.sh notes failed:\n%s", out)
	}
	if !strings.Contains(out, "`latest`") {
		t.Errorf("the note for a release version does not mention `latest`:\n%s", out)
	}
}

// The tag pattern that publishes already allows `-rc.N`; this is the assertion
// that it still does, because a tightening of the semver regex to "digits only"
// would refuse every release candidate at the one moment somebody needs one.
func TestAPrereleaseTagIsAVersionTheScriptAccepts(t *testing.T) {
	sh := repoFile(t, "scripts/release.sh")
	m := regexp.MustCompile(`semver_tag='([^']+)'`).FindStringSubmatch(sh)
	if m == nil {
		t.Fatal("scripts/release.sh declares no semver_tag pattern")
	}
	re, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatalf("the semver_tag pattern does not compile as a regexp: %v", err)
	}
	for _, tag := range []string{"v1.0.2-rc.0", "v1.0.2-rc.11", "v2.0.0-rc.1", "v1.0.2"} {
		if !re.MatchString(tag) {
			t.Errorf("%s is not accepted by semver_tag, so no release candidate could be cut", tag)
		}
	}
	for _, tag := range []string{"v1.0", "vrc.1", "v1.0.2-", "v01.0.2-rc.1"} {
		if re.MatchString(tag) {
			t.Errorf("%s is accepted by semver_tag and should not be", tag)
		}
	}
}
