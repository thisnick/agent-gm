#!/usr/bin/env bash
# pin-consistency -- spec section 3.6.
#
# The libgm pin is a fact recorded in three places that must agree: go.mod,
# the constant gm.PinnedUpstreamCommit in internal/gm/pin.go, and section 3.6
# of plans/AGENT_GM_SPEC.md. This job fails if any one of them is edited alone.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# 1. internal/gm/pin.go is the reference: the constant is the pin.
pin_go=$(sed -n 's/^const PinnedUpstreamCommit = "\([0-9a-f]\{7,40\}\)".*/\1/p' internal/gm/pin.go)
if [ -z "$pin_go" ]; then
  echo "pin-consistency: internal/gm/pin.go does not declare PinnedUpstreamCommit" >&2
  exit 1
fi

# 2. go.mod pins by pseudo-version, whose final component is the commit hash.
pin_mod=$(sed -n 's|^[[:space:]]*go\.mau\.fi/mautrix-gmessages v[^[:space:]]*-\([0-9a-f]\{12\}\)[[:space:]]*$|\1|p' go.mod)
if [ -z "$pin_mod" ]; then
  echo "pin-consistency: go.mod does not pin go.mau.fi/mautrix-gmessages by pseudo-version" >&2
  exit 1
fi

# 3. Spec section 3.6 states the commit.
pin_spec=$(awk '/^### 3\.6 /{f=1} f && /^commit:/{print $2; exit}' plans/AGENT_GM_SPEC.md)
if [ -z "$pin_spec" ]; then
  echo "pin-consistency: spec section 3.6 does not state a commit" >&2
  exit 1
fi

fail=0
case "$pin_mod" in
  "$pin_go"*) ;;
  *) echo "pin-consistency: go.mod pseudo-version names $pin_mod, internal/gm/pin.go names $pin_go" >&2; fail=1 ;;
esac
if [ "$pin_spec" != "$pin_go" ]; then
  echo "pin-consistency: spec section 3.6 names $pin_spec, internal/gm/pin.go names $pin_go" >&2
  fail=1
fi

# 4. The full commit the fixture-validation job clones begins with the pin.
pin_full=$(sed -n 's/^const PinnedUpstreamCommitFull = "\([0-9a-f]\{40\}\)".*/\1/p' internal/gm/pin.go)
if [ -z "$pin_full" ]; then
  echo "pin-consistency: internal/gm/pin.go does not declare a 40-character PinnedUpstreamCommitFull" >&2
  fail=1
else
  case "$pin_full" in
    "$pin_go"*) ;;
    *) echo "pin-consistency: PinnedUpstreamCommitFull ($pin_full) does not begin with PinnedUpstreamCommit ($pin_go)" >&2; fail=1 ;;
  esac
  case "$pin_full" in
    "$pin_mod"*) ;;
    *) echo "pin-consistency: PinnedUpstreamCommitFull ($pin_full) does not begin with the go.mod pseudo-version hash ($pin_mod)" >&2; fail=1 ;;
  esac
fi

# 5. The module path in the constant matches the one go.mod requires.
mod_path=$(sed -n 's/^const PinnedUpstreamModule = "\(.*\)"$/\1/p' internal/gm/pin.go)
if ! grep -q "^[[:space:]]*${mod_path} v" go.mod; then
  echo "pin-consistency: go.mod does not require $mod_path" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  cat >&2 <<'MSG'

Bumping the pin is a deliberate slice with its own live gate (spec section
3.6), never a drive-by commit. All three places move together, or none do.
MSG
  exit 1
fi

echo "pin-consistency: go.mod, internal/gm/pin.go and spec section 3.6 all name $pin_go"
