package gm

// PinnedUpstreamCommit is the mautrix-gmessages commit Agent GM is built
// against. It is a fact recorded in three places that must agree: go.mod,
// this constant, and spec section 3.6. The CI job `pin-consistency` fails if
// any one of them is edited alone.
//
// Bumping the pin is a deliberate slice with its own live gate (spec 3.6),
// never a drive-by commit.
const PinnedUpstreamCommit = "be48a58"

// PinnedUpstreamCommitFull is the same commit in full. Git cannot fetch a
// commit by abbreviation, so the fixture-validation job needs all forty
// characters; pin-consistency asserts it still begins with
// PinnedUpstreamCommit.
const PinnedUpstreamCommitFull = "be48a58b733825f6dfd6bb630af5f236d3bc9ae8"

// PinnedUpstreamModule is the Go module path of the pinned dependency.
const PinnedUpstreamModule = "go.mau.fi/mautrix-gmessages"
