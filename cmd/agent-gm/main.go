// Command agent-gm is Agent GM.
//
// It carries two subcommands that matter. `serve` is the server: the /v1
// REST API of spec section 7, over the store, the account supervisor and the
// Google Messages adapter. `spike` drives internal/gm directly and was Slice
// 1's deliverable, kept because it is the one way to exercise the adapter
// with no server in the way.
//
// The CLI is NOT here. `agm` is its own binary (cmd/agm), because it speaks
// REST and nothing else and is routinely run from a machine that is not the
// server -- the headless pairing flow of spec section 11.4 depends on that.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "spike":
		os.Exit(runSpike(os.Args[2:]))
	case "version":
		fmt.Println(versionLine())
		os.Exit(exitOK)
	case "-h", "--help", "help":
		usage()
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "agent-gm: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(exitUsage)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agent-gm -- one owner's agents, their Google Messages accounts.

Usage:
  agent-gm serve [--addr <host:port>]
  agent-gm spike <pair|list|send|watch|diag> [flags]
  agent-gm version

`+"`serve`"+` is the server: the /v1 REST API of spec section 7. `+"`agm`"+` is the
client for it and is a separate binary. `+"`spike`"+` drives the Google Messages
adapter directly and is Slice 1's deliverable; MCP and OAuth arrive in Slice 3.

Environment (spec section 15.1):
  AGENT_GM_PUBLIC_URL       required by `+"`serve`"+`; every URL is built from it
  AGENT_GM_LISTEN_ADDR      bind address (default 0.0.0.0:8080)
  AGENT_GM_ADMIN_SECRET     required by `+"`serve`"+`; at least 43 characters
  AGENT_GM_TRUSTED_PROXY_CIDRS  an invalid value refuses to start
  AGENT_GM_DATA_DIR         where the database and sessions/ live (default /data)
  AGENT_GM_DATA_KEY         256 bits, 64 hex characters or standard base64
  AGENT_GM_BACKEND          libgm (default) or fake
  AGENT_GM_ALLOW_FAKE       must be 1 for AGENT_GM_BACKEND=fake
  AGENT_GM_CHROME           a Chrome or Chromium binary for the pairing flow
  AGENT_GM_PAIRING_TIMEOUT  how long a started pairing may sit unconfirmed
  AGENT_GM_LOG_LEVEL        debug | info | warn | error
`)
}
