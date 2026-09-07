#!/usr/bin/env bash
# image -- build (and on main, push) the container image. Spec 13.6, 14.1, 14.2.
#
# CI runs this through `devbox run image` / `devbox run image-smoke`, which is
# the point: the image CI publishes is built by a script anybody can run
# locally, with the same tags and the same build arguments.
#
#   scripts/image.sh build   multi-arch build, no push (the default)
#   scripts/image.sh push    multi-arch build and push, with the tags below
#   scripts/image.sh smoke   single-arch build, loaded locally, then the
#                            assertions of slice-3 acceptance tests 26 and 28
#
# Tags (section 14.2). On a push to `main`: `sha-<short>` and `main`. On a
# `vX.Y.Z` tag: `vX.Y.Z`, `vX.Y` and `latest` -- on a `vX.Y.Z-rc.N` tag,
# `vX.Y.Z-rc.N` alone. Anywhere else: `sha-<short>`, and `push` refuses,
# because a tag that moves is a tag nobody can pin.
#
# **Deployments pin by digest, not by tag.** The digest is printed at the end
# of a push for exactly that reason.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

image="${AGENT_GM_IMAGE:-ghcr.io/thisnick/agent-gm}"
platforms="${AGENT_GM_IMAGE_PLATFORMS:-linux/amd64,linux/arm64}"
builder="${AGENT_GM_IMAGE_BUILDER:-agent-gm}"

cmd="${1:-build}"
if [ "${AGENT_GM_IMAGE_PUSH:-}" = "1" ] && [ "$cmd" = "build" ]; then
  cmd=push
fi

die() { echo "image: $*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || die "docker is not on PATH"

# --- what is being built ------------------------------------------------------

# The commit reaches the binary as a build argument because the build context
# has no .git: a container build cannot read a VCS stamp, and section 1.4
# makes the commit an AGPL section 13 obligation rather than a nicety.
commit="${GITHUB_SHA:-$(git rev-parse HEAD)}"
short="$(printf '%s' "$commit" | cut -c1-7)"

ref_name="${GITHUB_REF_NAME:-$(git rev-parse --abbrev-ref HEAD)}"
ref_type="${GITHUB_REF_TYPE:-branch}"

# The version is npm/package.json's and nobody else's (D39). It is the same
# question `scripts/release.sh version` answers, asked the same way, so the
# `org.opencontainers.image.version` label on the image, the archives on the
# release page and `@agent-gm/cli` cannot disagree. The label is the VERSION;
# `org.opencontainers.image.revision` is what tells two builds of the same
# version apart, and that is the label the release guard reads.
version="$(sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' npm/package.json | head -1)"
[ -n "$version" ] || die "npm/package.json declares no version, and it is the version authority (D39)"

tags=()
case "$ref_type:$ref_name" in
  tag:v*)
    # The tag is a label FOR the version, so it has to be the label for THIS
    # version: an image tagged v1.0.3 whose binary answers `1.0.2` is a lie
    # that survives every later check.
    [ "$ref_name" = "v$version" ] || die "refusing to build $ref_name: npm/package.json says" \
        "the version is $version, so the tag for it is v$version"
    case "$version" in
      # A prerelease is tagged vX.Y.Z-rc.N and NOTHING else. `latest` and
      # `vX.Y` are where a pull with no tag and a pull by minor land, and
      # neither of those callers asked for a release candidate. `${version%.*}`
      # on `1.0.2-rc.0` is also `1.0.2-rc`, which is not a minor series at all.
      *-*) tags=("$ref_name") ;;
      *)   tags=("$ref_name" "v${version%.*}" "latest") ;;
    esac
    ;;
  *:main)
    tags=("sha-$short" "main")
    ;;
  *)
    tags=("sha-$short")
    ;;
esac

echo "image: $image"
echo "  version   $version"
echo "  commit    $commit"
echo "  ref       $ref_type/$ref_name"
echo "  tags      ${tags[*]}"

tag_args=()
for t in "${tags[@]}"; do tag_args+=(--tag "$image:$t"); done

build_args=(
  --build-arg "VERSION=$version"
  --build-arg "COMMIT=$commit"
  --file Dockerfile
)

# --- the builder --------------------------------------------------------------

# A multi-platform build needs the docker-container driver; the default
# `docker` driver cannot emit a manifest list. QEMU is registered so that the
# arm64 image can also be RUN locally -- the build stage itself cross compiles
# natively (`FROM --platform=$BUILDPLATFORM`, CGO_ENABLED=0), so emulation is
# not on the build's critical path.
ensure_builder() {
  if ! docker buildx inspect "$builder" >/dev/null 2>&1; then
    docker buildx create --name "$builder" --driver docker-container --bootstrap >/dev/null
  fi
}

ensure_qemu() {
  case "$platforms" in
    *,*) ;;
    *) return 0 ;;
  esac
  if docker buildx inspect "$builder" 2>/dev/null | grep -qi 'linux/arm64'; then
    return 0
  fi
  docker run --privileged --rm tonistiigi/binfmt:qemu-v8.1.5 --install all >/dev/null
}

# --- the three modes ----------------------------------------------------------

case "$cmd" in
  build)
    ensure_builder
    ensure_qemu
    # No push and no --load: a manifest list cannot be loaded into the local
    # daemon. The build still proves both architectures compile, which is
    # what a pull request needs to know.
    docker buildx build --builder "$builder" --platform "$platforms" \
      "${build_args[@]}" "${tag_args[@]}" .
    echo "image: built ${platforms} (not pushed)"
    ;;

  push)
    case "$ref_type:$ref_name" in
      tag:v*|*:main) ;;
      *) die "refusing to push from $ref_type/$ref_name; only main and vX.Y.Z tags are published" ;;
    esac
    ensure_builder
    ensure_qemu
    docker buildx build --builder "$builder" --platform "$platforms" \
      "${build_args[@]}" "${tag_args[@]}" --push .
    echo "image: pushed ${tags[*]}"
    # The digest, which is what a deployment pins (section 14.2).
    docker buildx imagetools inspect "$image:${tags[0]}" --format '{{println .Manifest.Digest}}' || true
    ;;

  smoke)
    # Slice-3 acceptance tests 26 and 28, as far as one machine can assert
    # them: the image runs as nonroot, `agent-gm healthcheck` succeeds inside
    # it with no shell and no curl, and the running server reports the commit
    # that was built.
    smoke_platform="${AGENT_GM_IMAGE_SMOKE_PLATFORM:-linux/amd64}"
    ensure_builder
    local_tag="agent-gm:smoke-$short"
    docker buildx build --builder "$builder" --platform "$smoke_platform" \
      "${build_args[@]}" --tag "$local_tag" --load .

    user="$(docker image inspect "$local_tag" --format '{{.Config.User}}')"
    [ "$user" = "nonroot:nonroot" ] || die "image runs as '$user', want nonroot:nonroot"
    echo "image: USER is $user"

    # There is no shell in the image, so this is the proof rather than a
    # claim: `sh` cannot be executed, and neither can curl.
    if docker run --rm --entrypoint /bin/sh "$local_tag" -c 'echo shell' >/dev/null 2>&1; then
      die "the runtime image has a shell; the base is supposed to be distroless static"
    fi
    if docker run --rm --entrypoint /usr/bin/curl "$local_tag" --version >/dev/null 2>&1; then
      die "the runtime image has curl; the healthcheck is a subcommand for a reason"
    fi
    echo "image: no shell, no curl"

    name="agent-gm-smoke-$$"
    cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
    trap cleanup EXIT

    # A throwaway admin secret and data key. They are generated here, are
    # never written to disk, and belong to a container that is deleted at the
    # end of this function.
    docker run -d --name "$name" \
      -e AGENT_GM_PUBLIC_URL=https://gm.example.test \
      -e "AGENT_GM_ADMIN_SECRET=$(head -c 48 /dev/urandom | base64 | tr -d '\n=' | cut -c1-48)" \
      -e "AGENT_GM_DATA_KEY=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
      "$local_tag" >/dev/null

    ok=""
    for _ in $(seq 1 30); do
      if docker exec "$name" /usr/local/bin/agent-gm healthcheck; then ok=1; break; fi
      sleep 1
    done
    if [ -z "$ok" ]; then
      docker logs "$name" >&2 || true
      die "agent-gm healthcheck never succeeded inside the container"
    fi
    echo "image: agent-gm healthcheck exited 0 inside the container"

    reported="$(docker run --rm "$local_tag" version)"
    echo "image: $reported"
    case "$reported" in
      *"$commit"*) ;;
      *) die "the image reports '$reported', which does not carry the built commit $commit" ;;
    esac
    ;;

  *)
    die "unknown mode '$cmd'; use build, push or smoke"
    ;;
esac
