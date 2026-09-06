package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/cli"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/config"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/logging"
	"github.com/thisnick/agent-gm/internal/store"
)

func versionLine() string {
	rev := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				rev = s.Value
			}
		}
	}
	return fmt.Sprintf("agent-gm %s (libgm pinned at %s)", rev, gm.PinnedUpstreamCommit)
}

func spikeUsage() {
	fmt.Fprint(os.Stderr, `Usage:
  agent-gm spike pair  [--device-index N] [--timeout 5m] [--account <acct-id>]
                       [--paste] [--paste-file <path>] [--refresh-cookies]
                       [--forget-browser]
  agent-gm spike list  [--account <acct-id>]
  agent-gm spike send  <conv-id> <text>
  agent-gm spike watch [--account <acct-id>] [--for 60s]
  agent-gm spike diag
`)
}

func runSpike(args []string) int {
	if len(args) == 0 {
		spikeUsage()
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "pair":
		return spikePair(rest)
	case "list":
		return spikeList(rest)
	case "send":
		return spikeSend(rest)
	case "watch":
		return spikeWatch(rest)
	case "diag":
		return spikeDiag(rest)
	case "-h", "--help":
		spikeUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "agent-gm spike: unknown subcommand %q\n\n", sub)
		spikeUsage()
		return exitUsage
	}
}

// --- shared wiring -----------------------------------------------------------

type runtimeEnv struct {
	cfg      config.Config
	store    *store.Store
	sessions *store.SessionStore
	sup      *accounts.Supervisor
	log      zerolog.Logger
	libLog   zerolog.Logger
	clock    clock.Clock
}

func (r *runtimeEnv) Close() {
	if r.store != nil {
		_ = r.store.Close()
	}
}

func openRuntime() (*runtimeEnv, int) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitLocalConfig
	}

	// Two loggers, and the split is the point (internal/logging): Agent GM's
	// own on stderr, and a separate one for libgm, tagged and floored at
	// warn, so upstream's narration of the long poll never lands in the
	// output of a command the owner is reading. Neither ever writes to
	// stdout.
	logOpts := logging.Options{
		Level:       cfg.LogLevel,
		Format:      cfg.LogFormat,
		UnsafeTrace: cfg.UnsafeTrace,
		Quiet:       true,
	}
	logger := logging.New(logOpts)
	if cfg.UnsafeTrace {
		logger.Warn().Msg("AGENT_GM_UNSAFE_TRACE is set: libgm will log decrypted payloads")
	}

	clk := clock.Real{}
	st, err := store.Open(cfg.DataDir, clk)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitLocalConfig
	}
	sessions, err := store.NewSessionStore(cfg.DataDir, cfg.DataKey)
	if err != nil {
		_ = st.Close()
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitLocalConfig
	}
	// Agent GM tightens what it creates (store.SecureDataDir,
	// secureDatabaseFiles), so a warning here means something OUTSIDE Agent
	// GM loosened it: a restore, a bind mount with its own ownership, an
	// operator's chmod -R. It warns rather than refusing to start, because
	// refusing would turn a fixable disclosure into an outage and an
	// operator who cannot start the server cannot read the message telling
	// them why.
	for _, w := range store.CheckPermissions(cfg.DataDir) {
		logger.Warn().
			Str("path", w.Path).
			Str("mode", fmt.Sprintf("%04o", w.Mode.Perm())).
			Str("want", fmt.Sprintf("%04o", w.Want)).
			Msg("a file under the data directory is readable by more than its owner; " +
				"message text is not encrypted at rest, so the file mode is the at-rest model")
	}

	r := &runtimeEnv{cfg: cfg, store: st, sessions: sessions, log: logger,
		libLog: logging.Library(logger, logOpts), clock: clk}
	r.sup = accounts.New(st, sessions, clk, nil)
	return r, exitOK
}

// newBackend builds the backend this process was configured for. The fake
// needs both AGENT_GM_BACKEND=fake and AGENT_GM_ALLOW_FAKE=1, which
// config.Load has already enforced.
func (r *runtimeEnv) newBackend() gm.Backend {
	if r.cfg.Backend == config.BackendFake {
		return newSeededFake()
	}
	return gm.New(r.libLog)
}

// newSeededFake gives the fake backend a small fixture world, so that
// AGENT_GM_BACKEND=fake drives a usable spike with no phone. Every number in
// it is a fictional 555 number, which must never be dialled (spec 13.3).
func newSeededFake() *fake.Backend {
	f := fake.New("spike@example.com")
	f.SeedConversation(gm.Conversation{
		SourceID: "fixture-conv-1", Name: "Fixture direct", Folder: gm.FolderInbox,
		Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto,
		DefaultOutgoingID: "fixture-me", LastActivity: time.Now().UTC(),
		Participants: []gm.Participant{
			{SourceID: "fixture-me", IsMe: true, IsVisible: true},
			{SourceID: "fixture-them", PhoneE164: "+12025550123", DisplayName: "Fixture", IsVisible: true},
		},
	})
	f.SeedContacts(gm.Contact{SourceID: "fixture-them", DisplayName: "Fixture", PhoneE164: "+12025550123"})
	return f
}

// resume reloads every account whose session file is on disk, connects it,
// and starts its ingest goroutine. It never pairs.
func (r *runtimeEnv) resume(ctx context.Context) ([]*accounts.Account, error) {
	rows, err := r.store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	var out []*accounts.Account
	for _, row := range rows {
		if !row.SessionPresent {
			continue
		}
		blob, err := r.sessions.Load(row.ID)
		if err != nil {
			if errors.Is(err, store.ErrSessionUndecryptable) {
				return nil, fmt.Errorf("%s: session envelope cannot be decrypted; "+
					"the data key differs from the one that sealed it. Restore the "+
					"original AGENT_GM_DATA_KEY -- there is no in-place rotation", row.ID)
			}
			continue
		}
		var backend gm.Backend
		if r.cfg.Backend == config.BackendFake {
			f := fake.New("")
			if err := f.LoadSession(blob); err != nil {
				return nil, err
			}
			backend = f
		} else {
			b, err := gm.NewFromSession(blob, r.log)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", row.ID, err)
			}
			backend = b
		}
		a := r.sup.Adopt(ctx, row.ID, row.GoogleAccount, backend)
		if err := r.sup.Start(ctx, a); err != nil {
			fmt.Fprintf(os.Stderr, "agent-gm: %s could not connect: %v\n", row.ID, err)
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (r *runtimeEnv) pickAccount(ctx context.Context, want string) (*accounts.Account, error) {
	live := r.sup.List()
	if want == "" {
		want = r.cfg.DefaultAccount
	}
	if want != "" {
		for _, a := range live {
			if a.ID == want {
				return a, nil
			}
		}
		return nil, fmt.Errorf("no connected account %s", want)
	}
	switch len(live) {
	case 0:
		return nil, errors.New("no accounts are connected; run `agent-gm spike pair` first")
	case 1:
		return live[0], nil
	default:
		var ids []string
		for _, a := range live {
			ids = append(ids, a.ID)
		}
		return nil, fmt.Errorf("several accounts are connected; name one with --account: %s",
			strings.Join(ids, ", "))
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func fail(err error) int {
	var ge *gm.Error
	if errors.As(err, &ge) {
		fmt.Fprintf(os.Stderr, "agent-gm: %s: %s\n", ge.Code, ge.Message)
		for k, v := range ge.Details {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", k, v)
		}
	} else {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
	}
	return exitCodeFor(err)
}

// --- spike pair --------------------------------------------------------------

func spikePair(args []string) int {
	fs := flag.NewFlagSet("spike pair", flag.ContinueOnError)
	deviceIndex := fs.Int("device-index", 0, "select among several primary-looking devices")
	timeoutStr := fs.String("timeout", "", "how long to wait for the sign-in and the emoji")
	accountID := fs.String("account", "", "the acct_ ID to refresh or re-pair")
	paste := fs.Bool("paste", false, "read the cookies from stdin instead of launching Chrome")
	pasteFile := fs.String("paste-file", "", "read the cookies from a file")
	refresh := fs.Bool("refresh-cookies", false, "re-authenticate an existing pairing")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	r, code := openRuntime()
	if code != exitOK {
		return code
	}
	defer r.Close()

	ctx, cancel := signalContext()
	defer cancel()

	timeout := r.cfg.PairingTimeout
	if *timeoutStr != "" {
		d, err := config.ParseDuration(*timeoutStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-gm: --timeout: %v\n", err)
			return exitUsage
		}
		timeout = d
	}

	cookies, code := gatherCookies(ctx, r, *paste, *pasteFile, timeout)
	if code != exitOK {
		return code
	}

	if *refresh {
		if *accountID == "" {
			fmt.Fprintln(os.Stderr, "agent-gm: --refresh-cookies names an account: pass --account <acct-id>")
			return exitUsage
		}
		if _, err := r.resume(ctx); err != nil {
			return fail(err)
		}
		a, err := r.sup.Get(*accountID)
		if err != nil {
			return fail(err)
		}
		if err := a.Backend.RefreshGoogleCookies(ctx, cookies); err != nil {
			return fail(err)
		}
		if err := a.PersistSession(ctx); err != nil {
			return fail(err)
		}
		fmt.Printf("Re-authenticated.  account: %s\n", a.ID)
		return exitOK
	}

	backend := r.newBackend()
	fmt.Println()
	acct, err := r.sup.Pair(ctx, backend, cookies, *deviceIndex, func(emoji string) {
		fmt.Println("  Your phone will show three emoji and ask which one matches.")
		fmt.Println("  Tap this one:")
		fmt.Println()
		fmt.Printf("        %s\n", emoji)
		fmt.Println()
		fmt.Println("  Waiting for the phone...")
	})
	if err != nil {
		return fail(err)
	}
	defer r.sup.StopAll(context.WithoutCancel(ctx))

	// Ingest the conversation list before returning.
	//
	// Pairing starts an event goroutine and the phone's ClientReady carries
	// the conversations, but that arrives ASYNCHRONOUSLY: without this the
	// process could exit before the first event was applied, and `spike
	// list` in the next process would find an empty database. It did, on a
	// slower machine -- the same race the spec's own transcript rules out by
	// printing "Backfilling 41 conversations... done" before the prompt
	// comes back.
	//
	// Doing it synchronously here, through the same upsert path the event
	// loop uses, makes "paired" mean the history is readable rather than
	// merely that the phone said yes.
	if n, err := r.ingestConversations(ctx, acct); err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: reading the conversation list: %v\n", err)
	} else {
		fmt.Printf("Read %d conversations.\n", n)
	}

	row, err := r.store.Account(ctx, acct.ID)
	if err != nil {
		return fail(err)
	}
	fmt.Println()
	fmt.Printf("Paired.  account: %s  (%s)  phone: %s\n", acct.ID, row.GoogleAccount, row.PhoneID)
	if row.GaiaDestRegUUID != "" {
		// The library's own identifier for the phone it chose, printed under
		// its own name rather than dressed up as "Device": it is the value
		// spec 3.2 says to record so "which phone did we pair?" is
		// answerable, not something an owner recognises.
		fmt.Printf("dest_reg_uuid: %s\n", row.GaiaDestRegUUID)
	}
	fmt.Println(afterPairingNotes)
	return exitOK
}

const afterPairingNotes = `
Two things to check on the phone, once:
  * Google Messages must be your default SMS app, or sends will fail.
    ` + "`agent-gm spike diag`" + ` reports this as is_default_sms_app.
  * For group messages, Settings -> Advanced -> Group messaging should be
    "Send an MMS reply to all recipients". With the SMS setting, a group
    send may fan out as separate SMS threads instead.

Using Google Messages for web in a browser at the same time is fine; it
causes extra resyncs, not a lost pairing.`

// gatherCookies runs the capture, or reads a paste. Cookies never touch a
// file of Agent GM's own: they go from here into the pairing call.
func gatherCookies(ctx context.Context, r *runtimeEnv, paste bool, pasteFile string, timeout time.Duration) (map[string]string, int) {
	if paste || pasteFile != "" {
		var raw []byte
		var err error
		if pasteFile != "" {
			raw, err = os.ReadFile(pasteFile)
		} else {
			raw, err = io.ReadAll(os.Stdin)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-gm: reading the paste: %v\n", err)
			return nil, exitUsage
		}
		cookies, err := cli.ParsePaste(string(raw))
		if err != nil {
			// The input is never echoed.
			fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
			return nil, exitUsage
		}
		return cookies, exitOK
	}

	chrome, err := cli.FindChrome(r.cfg.Chrome)
	if err != nil {
		if errors.Is(err, cli.ErrNoChrome) {
			// Not a missing-binary error: the two ways forward, best first,
			// and exit 9 because nothing about the command was malformed.
			fmt.Fprint(os.Stderr, cli.NoChromeMessage(os.Getenv("AGENT_GM_PUBLIC_URL")))
			return nil, exitLocalConfig
		}
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitLocalConfig
	}

	fmt.Println("Adding a Google account to Agent GM.")
	fmt.Println("Opening a dedicated Chrome window for Google sign-in.")
	fmt.Println("This profile belongs to agm alone; your normal Chrome is untouched.")
	fmt.Println()
	fmt.Println("  Sign in to your Google account in that window, and wait for")
	fmt.Println("  Google Messages for web to load.")
	fmt.Println()
	fmt.Println("  A brand-new empty Chrome profile is a new device to Google and may")
	fmt.Println("  trigger 2FA or a device-verification challenge. That is a one-time")
	fmt.Println("  cost, not a failure.")
	fmt.Println("  The debugging port is a live credential channel: it is bound to")
	fmt.Println("  loopback on a random port and Chrome is killed the moment the")
	fmt.Println("  capture completes.")
	fmt.Println("  The profile is short-lived: it is created for this capture and")
	fmt.Println("  deleted as soon as Chrome closes, so nothing signed in to Google")
	fmt.Println("  is left on this machine. A later --refresh-cookies opens a fresh")
	fmt.Println("  window and asks you to sign in again; Google may or may not ask")
	fmt.Println("  for your password.")
	fmt.Println()

	capture := &cli.Capture{
		Chrome:  chrome,
		Timeout: timeout,
		Logf:    func(format string, args ...any) { fmt.Printf("  "+format+"\n", args...) },
	}
	cookies, err := capture.Run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitRetryable
	}
	return cookies, exitOK
}

// --- spike list --------------------------------------------------------------

func spikeList(args []string) int {
	fs := flag.NewFlagSet("spike list", flag.ContinueOnError)
	accountID := fs.String("account", "", "limit to one account")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	r, code := openRuntime()
	if code != exitOK {
		return code
	}
	defer r.Close()
	ctx, cancel := signalContext()
	defer cancel()

	if _, err := r.resume(ctx); err != nil {
		return fail(err)
	}
	defer r.sup.StopAll(context.WithoutCancel(ctx))

	// Give the initial ClientReady a moment to land, then read from the
	// store -- reads never depend on the phone being awake.
	a, err := r.pickAccount(ctx, *accountID)
	if err != nil {
		return fail(err)
	}
	if _, err := r.ingestConversations(ctx, a); err != nil {
		return fail(err)
	}

	rows, err := r.store.Conversations(ctx, store.ConversationFilter{AccountID: a.ID})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("%-42s  %-8s  %-19s  %s\n", "CONVERSATION", "TYPE", "LAST ACTIVITY", "NAME")
	for _, c := range rows {
		fmt.Printf("%-42s  %-8s  %-19s  %s\n", c.ID, c.ConversationType,
			time.UnixMilli(c.LastActivityMS).UTC().Format("2006-01-02 15:04:05"), c.Name)
	}
	fmt.Printf("\n%d conversations for %s\n", len(rows), a.ID)
	return exitOK
}

// ingestConversations reads this account's conversation list and applies it
// through the SAME upsert path the event loop uses, so a caller never has to
// wonder which of two writers produced a row.
func (r *runtimeEnv) ingestConversations(ctx context.Context, a *accounts.Account) (int, error) {
	convs, err := a.Backend.ListConversations(ctx, gm.FolderInbox, 100)
	if err != nil {
		return 0, err
	}
	for _, c := range convs {
		if _, err := a.Ingester().IngestConversation(ctx, c); err != nil {
			return 0, err
		}
	}
	return len(convs), nil
}

// --- spike send --------------------------------------------------------------

func spikeSend(args []string) int {
	fs := flag.NewFlagSet("spike send", flag.ContinueOnError)
	accountID := fs.String("account", "", "limit to one account")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "Usage: agent-gm spike send <conv-id> <text>")
		return exitUsage
	}
	convID, text := fs.Arg(0), strings.Join(fs.Args()[1:], " ")
	if !store.HasPrefix(convID, store.PrefixConversation) {
		fmt.Fprintf(os.Stderr, "agent-gm: %q is not a conversation ID; it must start with %q\n",
			convID, store.PrefixConversation)
		return exitUsage
	}

	r, code := openRuntime()
	if code != exitOK {
		return code
	}
	defer r.Close()
	ctx, cancel := signalContext()
	defer cancel()

	if _, err := r.resume(ctx); err != nil {
		return fail(err)
	}
	defer r.sup.StopAll(context.WithoutCancel(ctx))

	conv, err := r.store.Conversation(ctx, convID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: no conversation %s\n", convID)
		return exitNotFound
	}
	a, err := r.pickAccount(ctx, orDefault(*accountID, conv.AccountID))
	if err != nil {
		return fail(err)
	}

	tmpID := gm.GenerateTmpID()
	res, err := a.Backend.SendText(ctx, gm.SendTextRequest{
		ConversationID: conv.SourceID,
		ParticipantID:  conv.DefaultOutgoingID,
		Text:           text,
		TmpID:          tmpID,
		SIMPayload:     conv.SIMPayload,
	})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("status: %s  tmp_id: %s\n", res.Status, tmpID)
	if res.GoogleAccountSwitch != "" {
		fmt.Println("the phone switched Google accounts underneath us")
	}
	if res.Status != gm.SendStatusSuccess {
		return fail(gm.SendFailure(res.Status))
	}
	return exitOK
}

func orDefault(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// --- spike watch -------------------------------------------------------------

func spikeWatch(args []string) int {
	fs := flag.NewFlagSet("spike watch", flag.ContinueOnError)
	accountID := fs.String("account", "", "limit to one account")
	forStr := fs.String("for", "", "stop after this long")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	r, code := openRuntime()
	if code != exitOK {
		return code
	}
	defer r.Close()
	ctx, cancel := signalContext()
	defer cancel()

	if *forStr != "" {
		d, err := config.ParseDuration(*forStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-gm: --for: %v\n", err)
			return exitUsage
		}
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, d)
		defer stop()
	}

	// One goroutine per account applies the events AND prints them, so
	// nothing competes with the supervisor's own loop for the same channel.
	r.sup.ManualIngest = true
	live, err := r.resume(ctx)
	if err != nil {
		return fail(err)
	}
	defer r.sup.StopAll(context.WithoutCancel(ctx))
	if len(live) == 0 {
		fmt.Fprintln(os.Stderr, "agent-gm: no accounts are connected")
		return exitUnsupportedCapability
	}

	watched := live
	if *accountID != "" {
		a, err := r.pickAccount(ctx, *accountID)
		if err != nil {
			return fail(err)
		}
		watched = []*accounts.Account{a}
	}

	fmt.Println("Watching. Ctrl-C to stop.")
	events := make(chan watched1, 64)
	for _, a := range watched {
		go func(a *accounts.Account) {
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-a.Backend.Events():
					if !ok {
						return
					}
					a.Apply(ctx, ev)
					select {
					case events <- watched1{a: a, ev: ev}:
					case <-ctx.Done():
						return
					}
				}
			}
		}(a)
	}
	for {
		select {
		case <-ctx.Done():
			return exitOK
		case e := <-events:
			printEvent(e.a.ID, e.ev)
		}
	}
}

type watched1 struct {
	a  *accounts.Account
	ev gm.Event
}

// printEvent renders one event. Message bodies ARE printed here, because the
// spike's whole job in the live gate is to show the owner the message that
// arrived; nothing it prints is logged or stored beyond the database.
func printEvent(accountID string, ev gm.Event) {
	stamp := time.Now().UTC().Format("15:04:05")
	switch e := ev.(type) {
	case *gm.EventMessage:
		m := e.Message
		fmt.Printf("%s %s  message  %s  %s  %s  tmp_id=%s  %q\n",
			stamp, accountID, m.Direction(), m.DeliveryState, m.SourceID, m.TmpID, m.Text)
	case *gm.EventConversation:
		fmt.Printf("%s %s  conversation  %s  %q\n", stamp, accountID,
			e.Conversation.SourceID, e.Conversation.Name)
	case *gm.EventUserAlert:
		fmt.Printf("%s %s  alert  %d\n", stamp, accountID, e.Alert)
	case *gm.EventClientReady:
		fmt.Printf("%s %s  ready  session=%s  conversations=%d\n",
			stamp, accountID, e.SessionID, len(e.Conversations))
	case *gm.EventListenFatalError:
		fmt.Printf("%s %s  listen-fatal  credentials_dead=%v  %v\n",
			stamp, accountID, e.CredentialsDead, e.Err)
	case *gm.EventGaiaLoggedOut:
		fmt.Printf("%s %s  signed-out  the Google cookies are dead; run "+
			"`agent-gm spike pair --refresh-cookies --account %s`\n", stamp, accountID, accountID)
	default:
		fmt.Printf("%s %s  %T\n", stamp, accountID, ev)
	}
}

// --- spike diag --------------------------------------------------------------

func spikeDiag(args []string) int {
	fs := flag.NewFlagSet("spike diag", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	r, code := openRuntime()
	if code != exitOK {
		return code
	}
	defer r.Close()
	ctx, cancel := signalContext()
	defer cancel()

	// The compiled version is a property of the binary and is reported even
	// with no account at all.
	compiled := gm.CompiledConfigVersion()
	fmt.Printf("upstream_commit:          %s\n", gm.PinnedUpstreamCommit)
	fmt.Printf("config_version_compiled:  %s\n", compiled)

	if _, err := r.resume(ctx); err != nil {
		return fail(err)
	}
	defer r.sup.StopAll(context.WithoutCancel(ctx))

	live := r.sup.List()
	if len(live) == 0 {
		fmt.Println("accounts:                 none connected")
		return exitOK
	}
	for _, a := range live {
		// The live version is a property of each account's FetchConfig.
		info, err := a.Backend.FetchConfig(ctx)
		if err != nil {
			fmt.Printf("\n%s\n  google: null (%v)\n", a.ID, err)
			continue
		}
		isDefault, err := a.Backend.IsDefaultSMSApp(ctx)
		fmt.Printf("\n%s\n", a.ID)
		stale := !compiled.SameDate(info.Live)
		fmt.Printf("  config_version_live:    %s\n", info.Live)
		// Informational, not a fault (D32). Google ships a new ConfigVersion
		// on its own schedule, so this is the normal resting state between
		// pin bumps: it does not change the server's status and calls for no
		// action until a conversation-creating call actually fails.
		fmt.Printf("  config_version_stale:   %v", stale)
		if stale {
			fmt.Printf("   (informational: Google is ahead of the pin. Nothing to do\n" +
				"                          unless starting a conversation fails, and then the fix\n" +
				"                          is a pin bump -- spec 15.5)")
		}
		fmt.Println()
		if err != nil {
			fmt.Printf("  is_default_sms_app:     unknown (%v)\n", err)
		} else {
			fmt.Printf("  is_default_sms_app:     %v\n", isDefault)
		}
		fmt.Printf("  session_id:             %s\n", a.Backend.SessionID())
		fmt.Printf("  is_logged_in:           %v\n", a.Backend.IsLoggedIn())
		if c, ok := a.Backend.(gm.DroppedEventCounter); ok {
			fmt.Printf("  dropped_events:         %d\n", c.DroppedEvents())
			fmt.Printf("  unknown_events:         %d\n", c.UnknownEvents())
		}
	}
	return exitOK
}
