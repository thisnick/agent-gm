package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/thisnick/agent-gm/internal/config"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/logging"
	"github.com/thisnick/agent-gm/internal/store"
)

// spikeBackgroundOnce deliberately bypasses the supervisor: starting it would
// claim an active session and invalidate the experiment. Only the encrypted
// session is opened; message events are counted, not written to the database.
func spikeBackgroundOnce(args []string) int {
	fs := flag.NewFlagSet("background-once", flag.ContinueOnError)
	account := fs.String("account", "", "existing account ID; stop the server first")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *account == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "background-once requires --account <acct-id> and no positional arguments")
		return exitUsage
	}
	// Account IDs become filenames in SessionStore. Refuse path components.
	for _, ch := range *account {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_", ch) {
			fmt.Fprintln(os.Stderr, "invalid account ID")
			return exitUsage
		}
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitLocalConfig
	}
	if cfg.Backend != config.BackendLibGM || cfg.UnsafeTrace {
		fmt.Fprintln(os.Stderr, "background-once requires libgm with unsafe tracing disabled")
		return exitLocalConfig
	}
	sessions, err := store.NewSessionStore(cfg.DataDir, cfg.DataKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitLocalConfig
	}
	data, err := sessions.Load(*account)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitLocalConfig
	}
	opts := logging.Options{Level: "warn", Format: cfg.LogFormat, Quiet: true}
	backend, err := gm.NewFromSession(data, logging.Library(logging.New(opts), opts))
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not decode saved session")
		return exitLocalConfig
	}
	fmt.Println("Starting one background poll; the server must remain stopped.")
	pollErr := backend.PollBackgroundOnce()
	// Even an unsuccessful poll may have refreshed the auth token.
	updated, err := backend.MarshalSession()
	if err == nil {
		err = sessions.Save(*account, updated)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not persist refreshed session")
		return exitLocalConfig
	}
	counts := map[string]int{}
	draining := true
	for draining {
		select {
		case event := <-backend.Events():
			counts[fmt.Sprintf("%T", event)]++
		default:
			draining = false
		}
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s: %d\n", name, counts[name])
	}
	fmt.Printf("Dropped events: %d\n", backend.DroppedEvents())
	if pollErr != nil {
		// libgm also reports an empty background poll as an unclean exit.
		fmt.Fprintln(os.Stderr, "Background poll did not report a clean drain; this may be an empty poll or a connection failure.")
		return exitLocalConfig
	}
	fmt.Println("Background poll drained successfully; no message contents were printed or stored.")
	return exitOK
}
