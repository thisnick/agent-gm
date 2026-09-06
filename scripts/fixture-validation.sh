#!/usr/bin/env bash
# fixture-validation -- spec section 13.4.
#
# Clones mautrix-gmessages at the pinned commit and asserts the twenty claims
# of section 13.4 against THAT tree, not against Agent GM's own code. A test
# that only asserts Agent GM's own serialisation round-trips is not
# compatibility evidence.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

pin=$(sed -n 's/^const PinnedUpstreamCommit = "\([0-9a-f]\{7,40\}\)".*/\1/p' internal/gm/pin.go)
pin_full=$(sed -n 's/^const PinnedUpstreamCommitFull = "\([0-9a-f]\{40\}\)".*/\1/p' internal/gm/pin.go)
[ -n "$pin" ] || { echo "fixture-validation: no pin in internal/gm/pin.go" >&2; exit 1; }
[ -n "$pin_full" ] || { echo "fixture-validation: no full pin in internal/gm/pin.go" >&2; exit 1; }

# A checkout may be supplied (a developer machine keeps one); otherwise clone.
if [ -n "${AGENT_GM_UPSTREAM_DIR:-}" ]; then
  upstream="$AGENT_GM_UPSTREAM_DIR"
  echo "fixture-validation: using existing checkout $upstream"
else
  upstream=$(mktemp -d -t agent-gm-upstream-XXXXXX)
  trap 'rm -rf "$upstream"' EXIT
  echo "fixture-validation: cloning mautrix-gmessages at $pin"
  git init -q "$upstream"
  git -C "$upstream" remote add origin https://github.com/mautrix/gmessages.git
  git -C "$upstream" fetch -q --depth 1 origin "$pin_full"
  git -C "$upstream" checkout -q FETCH_HEAD
fi

have=$(git -C "$upstream" rev-parse HEAD)
case "$have" in
  "$pin"*) ;;
  *) echo "fixture-validation: checkout is at $have, expected $pin" >&2; exit 1 ;;
esac

AGENT_GM_UPSTREAM_DIR="$upstream" go test -tags fixtures ./internal/upstream/... -count=1 -v
