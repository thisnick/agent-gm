package main

// version and commit are set at link time by the Dockerfile and the release
// build (spec section 14.1):
//
//	-ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT}"
//
// They exist because `go build` inside a container has no VCS stamp to read.
// The build stage copies the source tree, not the `.git` directory, and
// `-trimpath` is set, so `debug.ReadBuildInfo` reports no `vcs.revision` and
// buildCommit() would answer "unknown" for every image ever shipped. That is
// not a cosmetic loss: section 1.4 makes the commit an AGPL section 13
// obligation -- `GET /v1/health` and the MCP `serverInfo` offer the
// corresponding source at the *exact built commit*, and "unknown" offers
// nothing.
//
// A `go build ./cmd/agent-gm` from a checkout sets neither, and the VCS stamp
// is then the better answer; buildVersion and buildCommit prefer whichever is
// actually present.
var (
	version = ""
	commit  = ""
)
