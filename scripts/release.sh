#!/usr/bin/env bash
# release -- the artefacts of spec section 14.3, built by a script anybody can
# run locally, exactly as CI runs them (section 13.6).
#
#   scripts/release.sh build       cross-compile the six archives into dist/,
#                                  write dist/checksums.txt, verify what can
#                                  be verified on this machine
#   scripts/release.sh npm-pack    stage npm/ at the release version with the
#                                  release checksums.txt pinned inside it and
#                                  `npm pack` it into dist/
#   scripts/release.sh npm-check   npm-pack, then install the tarball into a
#                                  throwaway prefix and assert the section
#                                  11.2 exit codes survive the shim
#   scripts/release.sh dry-run     build + npm-check, publishing nothing.
#                                  This runs on every push to main so the
#                                  release path cannot rot between releases.
#   scripts/release.sh sign        cosign keyless over dist/checksums.txt
#   scripts/release.sh publish     create the GitHub release and upload
#                                  dist/*; refuses off a vX.Y.Z tag
#   scripts/release.sh npm-publish publish the packed tarball to npm;
#                                  refuses off a vX.Y.Z tag
#   scripts/release.sh version     print the one version, and nothing else
#
# The version comes from `npm/package.json` and from nowhere else (D39).
# changesets bumps that field in the "Version Packages" pull request, and
# every artefact reads it from there: the archive names, the `-X main.version`
# stamp in both binaries, `GET /v1/health`, the image's
# `org.opencontainers.image.version` label and the npm package. The tag is a
# LABEL for the version rather than its source, so a tag that disagrees with
# the manifest is refused instead of believed.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

die() { echo "release: $*" >&2; exit 1; }
note() { echo "release: $*"; }

dist="$root/dist"
repo="${AGENT_GM_RELEASE_REPO:-thisnick/agent-gm}"

# --- what is being built ------------------------------------------------------

commit="${GITHUB_SHA:-$(git rev-parse HEAD)}"
short="$(printf '%s' "$commit" | cut -c1-7)"
ref_name="${GITHUB_REF_NAME:-$(git rev-parse --abbrev-ref HEAD)}"
ref_type="${GITHUB_REF_TYPE:-branch}"

# SemverTagPattern is the ONLY tag shape that publishes. `v*` is not it: the
# workflow fires on `v*`, and `vnonsense` built, signed with a real OIDC
# certificate and created a public GitHub release before `npm publish` finally
# rejected the version -- by which time the signature was in a transparency
# log that cannot be unpublished. R-1.
# No leading zeros in a numeric identifier: semver says `01.0.0` is not a
# version, and npm agrees, so `v01.0.0` would have been refused at the very
# last step of a release instead of the very first. R-11.
# DigestShape is the one spelling of a digest, beside the one spelling of a
# tag, so the two guards that check it cannot drift apart (R-12). It was
# written out twice -- once in do_publish, once in require_digest -- which is
# two places for one fact and one of the two ways a rule quietly stops
# applying.
digest_shape='^sha256:[0-9a-f]{64}$'

semver_tag='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'

# The one version. Read with sed rather than with node, because the workflow
# that cuts the tag asks this question before it has installed anything.
version="$(sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' npm/package.json | head -1)"
[ -n "$version" ] || die "npm/package.json declares no version, and it is the version authority (D39)"
if [[ ! "$version" =~ $semver ]]; then
  die "npm/package.json says the version is '$version', which is not MAJOR.MINOR.PATCH" \
      "with an optional prerelease. Every artefact is stamped with it and npm would refuse" \
      "it at the last step of a release instead of the first."
fi
tag="v$version"

tagged=0
if [ "$ref_type" = tag ]; then tagged=1; fi

# The two pins a release note has to record (sections 3.6 and 14.2). They are
# read out of the tree rather than typed, so a release note cannot claim a pin
# the binary was not built against.
libgm_pin="$(sed -n 's/^const PinnedUpstreamCommitFull = "\([0-9a-f]\{40\}\)".*/\1/p' internal/gm/pin.go)"
[ -n "$libgm_pin" ] || die "internal/gm/pin.go declares no PinnedUpstreamCommitFull"
sdk_pin="$(sed -n 's|^[[:space:]]*github.com/modelcontextprotocol/go-sdk \(v[^[:space:]]*\).*|\1|p' go.mod | head -1)"
[ -n "$sdk_pin" ] || die "go.mod does not require github.com/modelcontextprotocol/go-sdk"

# The six archives of section 14.3: the server on linux only, the CLI on all
# four platforms. Windows is out of the v1 matrix; `devbox run vet` keeps the
# cross-compile green anyway, so a Windows build is a decision rather than a
# repair job.
targets=(
  "agent-gm:linux:amd64"
  "agent-gm:linux:arm64"
  "agm:linux:amd64"
  "agm:linux:arm64"
  "agm:darwin:amd64"
  "agm:darwin:arm64"
)

# A test that only wants to read the version off a real artefact does not want
# to wait for six cross compiles. The narrowing is refused on a tag, so a
# release always builds the whole matrix of section 14.3 -- the one thing this
# knob must never be able to do is publish five archives out of six.
if [ -n "${AGENT_GM_RELEASE_TARGETS:-}" ]; then
  [ "$tagged" = 0 ] || die "AGENT_GM_RELEASE_TARGETS narrows the build matrix and is refused" \
      "on a tag: section 14.3 lists six archives and a release publishes all six"
  IFS=' ' read -r -a targets <<<"$AGENT_GM_RELEASE_TARGETS"
  note "matrix narrowed to ${targets[*]} by AGENT_GM_RELEASE_TARGETS"
fi

# --- build --------------------------------------------------------------------

do_build() {
  command -v go >/dev/null 2>&1 || die "go is not on PATH; run this under devbox"
  rm -rf "$dist"
  mkdir -p "$dist"

  note "version   $version"
  note "commit    $commit"
  note "ref       $ref_type/$ref_name"
  note "libgm     $libgm_pin"
  note "go-sdk    $sdk_pin"

  local ldflags="-s -w -X main.version=$version -X main.commit=$commit"
  local t bin goos goarch stage
  for t in "${targets[@]}"; do
    IFS=: read -r bin goos goarch <<<"$t"
    stage="$(mktemp -d)"
    note "building ${bin}_${version}_${goos}_${goarch}"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags "$ldflags" -o "$stage/$bin" "./cmd/$bin"
    cp LICENSE "$stage/LICENSE"
    cp README.md "$stage/README.md"
    # A deterministic archive: fixed mtime, fixed owner, sorted names. Two
    # builds of the same commit produce the same bytes, so a checksum is a
    # statement about the source rather than about the clock.
    tar --sort=name --owner=0 --group=0 --numeric-owner \
        --mtime="@$(git log -1 --format=%ct "$commit" 2>/dev/null || echo 0)" \
        -czf "$dist/${bin}_${version}_${goos}_${goarch}.tar.gz" \
        -C "$stage" "$bin" LICENSE README.md
    rm -rf "$stage"
  done

  ( cd "$dist" && shasum -a 256 ./*.tar.gz 2>/dev/null || sha256sum ./*.tar.gz ) \
    | sed 's|\./||' > "$dist/checksums.txt"
  note "checksums.txt"
  cat "$dist/checksums.txt"

  do_verify
}

# Acceptance test 1. Everything the runner can execute is executed; the darwin
# archives, which it cannot, are checked for architecture and for loading only
# libraries macOS itself ships, because a CGO-enabled darwin build would run on
# the owner's Mac only by accident.
do_verify() {
  local t bin goos goarch stage f
  for t in "${targets[@]}"; do
    IFS=: read -r bin goos goarch <<<"$t"
    stage="$(mktemp -d)"
    tar -xzf "$dist/${bin}_${version}_${goos}_${goarch}.tar.gz" -C "$stage"
    [ -x "$stage/$bin" ] || die "${bin}_${version}_${goos}_${goarch}.tar.gz has no executable $bin"
    [ -f "$stage/LICENSE" ] || die "${bin}_${version}_${goos}_${goarch}.tar.gz ships no LICENSE (section 1.4)"

    if [ "$goos" = "$(go env GOHOSTOS)" ] && can_execute "$goarch"; then
      local out
      out="$("$stage/$bin" version)" || die "$bin $goos/$goarch could not run 'version'"
      case "$out" in
        *"$version"*) ;;
        *) die "$bin $goos/$goarch reports '$out', which does not carry $version" ;;
      esac
      note "ran ${bin}_${goos}_${goarch}: $out"
    else
      # No runner for this one. `file` is the evidence that is left.
      f="$(file -b "$stage/$bin")"
      note "file ${bin}_${goos}_${goarch}: $f"
      case "$goos/$goarch:$f" in
        darwin/amd64:*"x86_64"*) ;;
        darwin/arm64:*"arm64"*) ;;
        linux/arm64:*"ARM aarch64"*|linux/arm64:*"aarch64"*) ;;
        linux/amd64:*"x86-64"*) ;;
        *) die "$bin $goos/$goarch is '$f', which is not the architecture it claims" ;;
      esac
      case "$goos:$f" in
        linux:*"dynamically linked"*) die "$bin linux/$goarch is dynamically linked; CGO_ENABLED=0 was supposed to prevent that" ;;
      esac
      if [ "$goos" = darwin ]; then
        # The `otool -L` question, asked on a machine with no otool. A Go
        # darwin binary always loads libSystem -- that is how a syscall is
        # made on macOS -- so "statically linked" is the wrong assertion and
        # would be a green light that meant nothing. The assertion that
        # matters is that every dylib it names is one macOS ships: a CGO
        # build that picked up /opt/homebrew or /usr/local would install on
        # the owner's Mac and die on a library that is not there.
        local dylibs bad
        dylibs="$(grep -ao '/[A-Za-z0-9_./+-]*\.dylib' "$stage/$bin" | sort -u || true)"
        [ -n "$dylibs" ] || die "$bin darwin/$goarch names no dylib at all; that is not a Mach-O this build produced"
        bad="$(printf '%s\n' "$dylibs" | grep -v '^/usr/lib/' | grep -v '^/System/Library/' || true)"
        [ -z "$bad" ] || die "$bin darwin/$goarch loads dylibs macOS does not ship: $bad"
        note "otool-equivalent ${bin}_${goos}_${goarch}: $(printf '%s' "$dylibs" | tr '\n' ' ')"
      fi
    fi
    rm -rf "$stage"
  done
  note "all $(( ${#targets[@]} )) archives verified"
}

# The host runs its own architecture always, and a foreign linux architecture
# when binfmt/qemu is registered for it -- which the release workflow arranges
# with docker/setup-qemu-action.
can_execute() {
  local goarch="$1"
  [ "$goarch" = "$(go env GOHOSTARCH)" ] && return 0
  [ "$(go env GOHOSTOS)" = linux ] || return 1
  case "$goarch" in
    arm64) [ -e /proc/sys/fs/binfmt_misc/qemu-aarch64 ] ;;
    amd64) [ -e /proc/sys/fs/binfmt_misc/qemu-x86_64 ] ;;
    *) return 1 ;;
  esac
}

# --- npm ----------------------------------------------------------------------

# Stage npm/ at the release version with the release's own checksums.txt
# copied INSIDE the package. That copy is the thing the postinstall verifies
# against, so a later compromise of the release page cannot serve a different
# binary to an already-published package version (section 14.3).
do_npm_pack() {
  command -v npm >/dev/null 2>&1 || die "npm is not on PATH; run this under devbox"
  [ -f "$dist/checksums.txt" ] || die "dist/checksums.txt is missing; run 'release.sh build' first"

  local stage="$dist/npm-stage"
  rm -rf "$stage"
  mkdir -p "$stage"
  cp -R npm/. "$stage/"
  cp LICENSE "$stage/LICENSE"
  cp "$dist/checksums.txt" "$stage/checksums.txt"

  # No rewrite. The manifest is COMMITTED at the released version (D39), so
  # what is packed is what is in the tree; the assertion is that the copy
  # agrees, which is the whole of the "one version" claim at the one place the
  # npm package is made.
  local staged
  staged="$(node -e 'process.stdout.write(require(process.argv[1]).version)' "$stage/package.json")"
  [ "$staged" = "$version" ] || die "the staged package.json says $staged and the release is" \
      "$version; npm/package.json is the version authority and nothing rewrites it"

  ( cd "$stage" && npm pack --pack-destination "$dist" >/dev/null )
  # Named, not globbed: `ls | head -1` sorts lexicographically, so a leftover
  # `agent-gm-cli-0.0.0-dev.abc1234.tgz` would win over `1.0.0` (R-7).
  local tarball="$dist/agent-gm-cli-$version.tgz"
  [ -f "$tarball" ] || die "npm pack produced no $(basename "$tarball"); it made:" \
      "$(ls "$dist"/agent-gm-cli-*.tgz 2>/dev/null | xargs -r -n1 basename | tr '\n' ' ')"
  note "packed $(basename "$tarball")"

  # The pinned checksums must actually be in the tarball. A `files` list that
  # quietly drops it would leave the postinstall verifying against nothing.
  tar -tzf "$tarball" | grep -qx 'package/checksums.txt' \
    || die "$(basename "$tarball") does not contain checksums.txt; the pin of section 14.3 is not in the package"
  note "checksums.txt is pinned inside the tarball"
  echo "$tarball" > "$dist/npm-tarball.txt"
}

# Acceptance tests 2 and 4, through a real `npm install` rather than through
# node alone: the download is served from dist/ over file:// so the run needs
# no release page, and the exit codes of section 11.2 are asserted through the
# installed shim.
do_npm_check() {
  do_npm_pack
  local tarball prefix
  tarball="$(cat "$dist/npm-tarball.txt")"
  prefix="$(mktemp -d)"

  note "installing $(basename "$tarball") into a throwaway prefix"
  AGENT_GM_CLI_BASE_URL="file://$dist" \
    npm install --prefix "$prefix" --no-audit --no-fund "$tarball" >/dev/null

  local shim="$prefix/node_modules/.bin/agm"
  [ -x "$shim" ] || die "npm install left no agm shim at $shim"

  local out rc
  out="$("$shim" version)" || die "'agm version' failed through the shim"
  case "$out" in
    *"$version"*) ;;
    *) die "'agm version' through the shim says '$out', not $version" ;;
  esac
  note "agm version through the shim: $out"

  # Section 11.2: 2 is a usage error and 1 is deliberately unassigned, so a 1
  # here would mean the shim swallowed the code and reported its own failure.
  set +e
  "$shim" --nonsense >/dev/null 2>&1
  rc=$?
  set -e
  [ "$rc" = 2 ] || die "'agm --nonsense' exited $rc through the shim, want 2"
  note "agm --nonsense exits 2 through the shim"

  # Cleaned up here rather than in a RETURN trap: a RETURN trap is not scoped
  # to the function that sets it, so it fired again when the script itself
  # returned, with $prefix long out of scope, and `set -u` turned a passing
  # dry run into a failure after every assertion had already succeeded.
  rm -rf "$prefix"
}

do_dry_run() {
  [ "$tagged" = 0 ] || note "note: this is a dry run of $tag; nothing will be published"
  do_build
  do_npm_check
  note "dry run complete -- built everything, published nothing"
}

# --- signing and publishing ---------------------------------------------------

# require_tag is the gate on every mode that leaves a permanent trace: a
# signature, a GitHub release, an npm version. R-1.
#
# It matches the VERSION, not merely the `v` prefix. `v1.0`, `v1.0.0.1`,
# `vnonsense` and `vtest` are all refused, and refused BEFORE cosign is
# invoked, because a keyless signature is written to a public transparency log
# and there is no way to take one back.
require_tag() {
  if [ "$ref_type" != tag ]; then
    die "refusing to $1 from $ref_type/$ref_name; only a vX.Y.Z tag publishes"
  fi
  # The VERSION, matched literally here rather than only through the
  # parse-time branch above. `$version` is the string that becomes the tag in
  # the notes, the `-X main.version` stamp and the npm version, so it is the
  # one worth asserting directly; `vnonsense` never reaches this line with a
  # `$version` that passes.
  if [[ ! "$ref_name" =~ $semver_tag ]] || [[ ! "$version" =~ $semver ]]; then
    die "refusing to $1 from the tag '$ref_name': it is not vMAJOR.MINOR.PATCH" \
        "with an optional prerelease. A tag that is not a version cuts a real," \
        "signed, public release that cannot be withdrawn from the sigstore log."
  fi
  # The tag AGREES with the manifest, which is the version authority (D39).
  # The artefacts are stamped from npm/package.json, so a tag that names a
  # different version would put `v1.0.3` on a release page full of `1.0.2`
  # binaries, and `npm publish` would then publish 1.0.2 under a 1.0.3
  # release. In the ordinary flow the tag is cut BY the manifest and cannot
  # disagree; this is the guard on the emergency path, where a human types it.
  if [ "$ref_name" != "$tag" ]; then
    die "refusing to $1 from the tag '$ref_name': npm/package.json says the version is" \
        "$version, so the tag for it is $tag. The manifest is the version authority (D39):" \
        "merge the Version Packages pull request, or fix the tag -- do not fix the binaries."
  fi
  [ "$tagged" = 1 ] || die "refusing to $1 from $ref_type/$ref_name"
}

do_sign() {
  require_tag sign
  command -v cosign >/dev/null 2>&1 || die "cosign is not on PATH"
  [ -f "$dist/checksums.txt" ] || die "dist/checksums.txt is missing"
  # Keyless, over GitHub's OIDC token. There is no private key anywhere in
  # this repository and there is not meant to be one.
  COSIGN_EXPERIMENTAL=1 cosign sign-blob --yes \
    --output-signature "$dist/checksums.txt.sig" \
    --output-certificate "$dist/checksums.txt.pem" \
    "$dist/checksums.txt"
  note "signed checksums.txt (keyless, GitHub OIDC)"
}

# The release note records the GHCR digest and both pins, because a deployment
# pins by digest and a reader has to be able to tell which upstream a binary
# was built against without cloning anything (sections 14.2, 14.3).
# require_digest is R-2 and R-6, in the script the note comes from rather than
# in the YAML step that happens to call it.
#
# Section 14.2 makes the digest the thing a deployment pins by, so a note that
# names none is a deployment instruction nobody can follow -- and `unknown` is
# worse than a missing line, because it looks like an answer. Three things are
# checked, in order of how badly each one fails:
#
#   1. it is a `sha256:<64 hex>` digest at all;
#   2. it RESOLVES on the registry, so a digest typed by hand or carried over
#      from a previous run is caught here rather than by whoever tries to
#      deploy it;
#   3. the image it resolves to was built from THIS commit. A tag that was
#      pushed, built, moved and re-pushed resolves immediately to the previous
#      image, and the note would then pin a digest built from different source
#      (R-6). The image carries `org.opencontainers.image.revision`, so the
#      question can simply be asked.
require_digest() {
  local image="${AGENT_GM_IMAGE:-ghcr.io/thisnick/agent-gm}"
  local digest="${AGENT_GM_IMAGE_DIGEST:-}"

  [ -n "$digest" ] || die "AGENT_GM_IMAGE_DIGEST is unset. A release note pins a deployment" \
      "by digest (section 14.2); publish it without one and there is nothing to deploy." \
      "The release workflow reads it back from the image the ci workflow pushed for this tag."
  # Hex, not 64 wildcards. `sha256:zzz…` passed a `?`-glob check and was then
  # caught by the registry, so a typo was diagnosed as "the image is not
  # there" -- which sends the reader looking in the wrong place. R-11.
  if [[ ! "$digest" =~ $digest_shape ]]; then
    die "AGENT_GM_IMAGE_DIGEST is '$digest', which is not a sha256:<64 lowercase hex> digest"
  fi

  if [ "${AGENT_GM_SKIP_DIGEST_RESOLVE:-0}" = 1 ]; then
    note "digest $digest (resolution skipped by AGENT_GM_SKIP_DIGEST_RESOLVE)"
    return 0
  fi
  command -v docker >/dev/null 2>&1 || die "docker is not on PATH, so $digest cannot be resolved"

  docker manifest inspect "$image@$digest" >/dev/null 2>&1 \
    || die "$image@$digest does not resolve on the registry; the note would pin an image" \
           "that is not there"

  # The revision label, read off one platform's config. A manifest list has no
  # labels of its own, so this descends to the linux/amd64 image.
  local revision
  revision="$(docker buildx imagetools inspect "$image@$digest" \
      --format '{{ range .Image }}{{ index .Config.Labels "org.opencontainers.image.revision" }}{{ end }}' \
      2>/dev/null | tr -d ' \n' || true)"
  if [ -z "$revision" ]; then
    die "$image@$digest carries no org.opencontainers.image.revision label, so the digest" \
        "cannot be tied to the commit being released. Rebuild the image with the label" \
        "(the Dockerfile sets it from the COMMIT build argument)."
  fi
  case "$revision" in
    *"$commit"*) ;;
    *) die "$image@$digest was built from $revision, but this release is $commit." \
           "A tag that was moved and re-pushed resolves to the PREVIOUS image, and the" \
           "note would pin a digest built from different source." ;;
  esac
  note "digest $digest resolves and was built from $commit"
}

do_notes() {
  local digest="${AGENT_GM_IMAGE_DIGEST:-unknown}"
  local image="${AGENT_GM_IMAGE:-ghcr.io/thisnick/agent-gm}"
  # A prerelease is tagged `vX.Y.Z-rc.N` and NOTHING else: `latest` and `vX.Y`
  # are the tags a `docker pull` with no tag lands on, and a release candidate
  # is not what anyone asking for either of those means (D39). The npm side of
  # the same rule is the `next` dist-tag in do_npm_publish.
  local base="${version%%-*}"
  local minor="${base%.*}"
  local tagline="Tags \`$tag\`, \`v$minor\` and \`latest\` point at that digest today."
  case "$version" in
    *-*) tagline="Tag \`$tag\` points at that digest, and nothing else does. This is a **prerelease**: it is not tagged \`latest\` or \`v$minor\`, and \`@agent-gm/cli\` publishes it under the \`next\` dist-tag." ;;
  esac
  cat <<EOF
## agent-gm $tag

Source commit: \`$commit\`

### Container image

Deployments pin by **digest**, not by tag:

    $image@$digest

$tagline
They are labels; the digest is the evidence.

### Pins

| Dependency | Pin |
|---|---|
| \`go.mau.fi/mautrix-gmessages\` (libgm) | \`$libgm_pin\` |
| \`github.com/modelcontextprotocol/go-sdk\` | \`$sdk_pin\` |

### Artefacts

\`checksums.txt\` is signed with cosign keyless over GitHub OIDC. Verify with:

    cosign verify-blob checksums.txt \\
      --signature checksums.txt.sig \\
      --certificate checksums.txt.pem \\
      --certificate-identity-regexp 'https://github.com/$repo/\.github/workflows/release\.yml@refs/tags/v.*' \\
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

The npm wrapper \`@agent-gm/cli@$version\` downloads the \`agm\` archive for
your platform from this release and verifies it against a copy of
\`checksums.txt\` pinned inside the npm tarball.

Agent GM is AGPL-3.0-or-later. The corresponding source for these binaries is
this repository at \`$commit\`.
EOF
}

do_publish() {
  require_tag publish
  # The digest's SHAPE, here, because this is the function that writes the
  # note; whether it resolves and whether it was built from this commit is
  # require_digest's. `unknown` -- do_notes' default -- fails on the first
  # line, which is the point: `devbox run release` sets no digest at all, and
  # the documented local release path used to publish `@unknown` in silence.
  if [ -z "${AGENT_GM_IMAGE_DIGEST:-}" ]; then
    die "AGENT_GM_IMAGE_DIGEST is unset; a release note pins a deployment by digest" \
        "(section 14.2) and a note that names none is an instruction nobody can follow"
  fi
  if [[ ! "${AGENT_GM_IMAGE_DIGEST}" =~ $digest_shape ]]; then
    die "AGENT_GM_IMAGE_DIGEST is '${AGENT_GM_IMAGE_DIGEST}', not a sha256:<64 lowercase hex> digest"
  fi
  require_digest
  command -v gh >/dev/null 2>&1 || die "gh is not on PATH"
  [ -f "$dist/checksums.txt" ] || die "dist/checksums.txt is missing; run 'release.sh build' first"
  [ -f "$dist/checksums.txt.sig" ] || die "dist/checksums.txt.sig is missing; run 'release.sh sign' first"
  local notes="$dist/release-notes.md"
  do_notes > "$notes"
  gh release create "$tag" --repo "$repo" --title "agent-gm $tag" --notes-file "$notes" \
    "$dist"/*.tar.gz "$dist/checksums.txt" "$dist/checksums.txt.sig" "$dist/checksums.txt.pem"
  note "published $tag"
}

do_npm_publish() {
  require_tag npm-publish
  local tarball want
  # The tarball for THIS version, named rather than "whatever npm-pack left
  # behind" (R-7). `do_build` clears dist/, so in the workflow the two agree --
  # but `devbox run release-npm-publish` by hand against a stale dist/ would
  # otherwise publish a leftover, and `ls … | head -1` sorts
  # `0.0.0-dev.abc1234` above `1.0.0`.
  want="$dist/agent-gm-cli-$version.tgz"
  tarball="$(cat "$dist/npm-tarball.txt" 2>/dev/null || true)"
  [ -n "$tarball" ] || die "no packed tarball; run 'release.sh npm-pack' first"
  [ "$tarball" = "$want" ] || die "the packed tarball is $(basename "$tarball"), but this" \
      "release is $version. Run 'release.sh build' and 'release.sh npm-pack' again;" \
      "publishing a tarball from another run publishes another build."
  [ -f "$tarball" ] || die "$tarball is recorded but does not exist"
  # Trusted publishing: npm authenticates with the workflow's OIDC identity
  # (id-token: write) and provenance is part of that identity, so there is no
  # token to fall back to and a publish without provenance is not a publish
  # this project makes. A failure here is a failure, not a retry.
  # npm < 11.5.1 has no trusted-publishing (OIDC) support: it would publish
  # unauthenticated and the registry would answer 404. Refuse early instead.
  npm() { "${AGENT_GM_NPM:-npm}" "$@"; }
  npmv="$(npm --version)"
  if [ "$(printf '%s\n11.5.1\n' "$npmv" | sort -V | head -1)" != "11.5.1" ]; then
    die "npm $npmv cannot do trusted publishing; need npm >= 11.5.1 (the workflow installs it)"
  fi
  # The dist-tag. `latest` is what a bare `npm i @agent-gm/cli` resolves to,
  # so a prerelease must NOT take it: `changeset pre enter rc` exists to hand
  # a release candidate to somebody who asked for one, and npm's way of asking
  # is `@next`. Passed explicitly in both cases, because npm's default is
  # `latest` and a default is not a decision (D39).
  local dist_tag=latest
  case "$version" in *-*) dist_tag=next ;; esac
  npm publish --access public --provenance --tag "$dist_tag" "$tarball" \
    || die "npm publish failed; trusted publishing must be enabled on npm for this repository and workflow (docs/operations.md)"
  note "published @agent-gm/cli@$version under the '$dist_tag' dist-tag"
}

case "${1:-build}" in
  version)     printf '%s\n' "$version" ;;
  build)       do_build ;;
  verify)      do_verify ;;
  npm-pack)    do_npm_pack ;;
  npm-check)   do_npm_check ;;
  dry-run)     do_dry_run ;;
  sign)        do_sign ;;
  notes)       do_notes ;;
  publish)     do_publish ;;
  npm-publish) do_npm_publish ;;
  *) die "unknown mode '$1'; use version, build, verify, npm-pack, npm-check, dry-run, sign, notes, publish or npm-publish" ;;
esac
