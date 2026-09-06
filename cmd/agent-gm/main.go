// Command agent-gm is Agent GM.
//
// In Slice 1 it carries one subcommand, `spike`, which drives internal/gm
// directly to prove Agent GM can pair, read and write against the real thing
// before any of the surfaces exist (spec section 16, Slice 1). REST, MCP,
// OAuth, media and the CLI proper arrive in later slices.
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
  agent-gm spike <pair|list|send|watch|diag> [flags]
  agent-gm version

`+"`spike`"+` drives the Google Messages adapter directly. It is the Slice 1
deliverable: the REST API, MCP, OAuth and the agm CLI arrive in later slices.

Environment (spec section 15.1):
  AGENT_GM_DATA_DIR         where the database and sessions/ live (default /data)
  AGENT_GM_DATA_KEY         256 bits, 64 hex characters or standard base64
  AGENT_GM_BACKEND          libgm (default) or fake
  AGENT_GM_ALLOW_FAKE       must be 1 for AGENT_GM_BACKEND=fake
  AGENT_GM_CHROME           a Chrome or Chromium binary for the pairing flow
  AGENT_GM_PAIRING_TIMEOUT  how long a started pairing may sit unconfirmed
  AGENT_GM_LOG_LEVEL        debug | info | warn | error
`)
}
