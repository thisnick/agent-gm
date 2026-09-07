package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/config"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/logging"
	"github.com/thisnick/agent-gm/internal/mcp"
	"github.com/thisnick/agent-gm/internal/media"
	"github.com/thisnick/agent-gm/internal/oauth"
	"github.com/thisnick/agent-gm/internal/settings"
	"github.com/thisnick/agent-gm/internal/store"
)

// runServe is `agent-gm serve`: the whole server, wired.
//
// The order below is the one spec section 4.3 and section 6.6 fix, and it is
// the order rather than a convenience:
//
//  1. read the environment, and refuse to start on anything an operator
//     mistyped -- an invalid trusted-proxy list, a short admin secret, a
//     missing public URL. An operator who mistyped should find out now, not
//     discover months later that the trust they configured was never in
//     force (spec section 12.3).
//  2. open and migrate the database. Migrations run on the writer goroutine
//     **before any listener binds** (section 4.3), so a request never sees a
//     half-migrated schema.
//  3. settle every operation left `running` by a previous process, also
//     before the listener binds and before any account connects (section
//     6.6). A crash is a property of the process, so every account's
//     in-flight operations are settled at once -- and nothing is retried.
//  4. revoke admin sessions minted under a previous AGENT_GM_ADMIN_SECRET
//     (section 12.1).
//  5. resume the accounts, then bind.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "", "override AGENT_GM_LISTEN_ADDR")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	built, code := buildServer(ctx, *addr)
	if code != exitOK {
		return code
	}
	defer built.Close()
	return built.listenAndServe(ctx)
}

// built is everything runServe assembles before it binds.
//
// It is a separate function from the binding half for one reason: three
// reviewer plants -- deleting `sup.Backfill = workers`, deleting
// `sup.Sweep = workers`, and making `resumeAccounts` return `0, nil` --
// ALL SURVIVED the whole suite. Each restored exactly one of the blockers
// this slice was rejected for. The compiler can prove `Workers` satisfies the
// two interfaces; it cannot prove that `serve` assigns them, and nothing
// else did either, because the wiring lived inside a function that also
// bound a socket and blocked.
//
// So the wiring is now a value a test can build and assert on.
type built struct {
	cfg      config.Config
	log      zerolog.Logger
	Store    *store.Store
	Sessions *store.SessionStore
	Sup      *accounts.Supervisor
	Deps     *api.HandlerDeps
	Server   *api.Server
	// OAuth is the authorization server of spec section 9, and MCP is the
	// streamable-HTTP surface of section 8. Handler is the three of them
	// mounted together, and it is what the listener serves.
	OAuth   *oauth.Server
	MCP     *mcp.Handler
	Handler http.Handler
	// Resumed is how many accounts came back from sessions/ at startup.
	Resumed int

	workers *accounts.Workers
	libLog  zerolog.Logger

	mu              sync.Mutex
	accountsStarted bool
}

// Close releases what buildServer opened.
func (b *built) Close() {
	if b.Store != nil {
		_ = b.Store.Close()
	}
}

// buildServer does everything runServe does except bind.
func buildServer(ctx context.Context, addrOverride string) (*built, int) {
	addr := &addrOverride

	cfg, err := config.LoadServer()
	if err != nil {
		// This is the "refuses to start" of sections 12.3 and 15.1. The
		// message names the variable and never its value: the admin secret
		// is the strongest credential Agent GM issues.
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitLocalConfig
	}
	if *addr != "" {
		cfg.ListenAddr = *addr
	}

	logOpts := logging.Options{
		Level:       cfg.LogLevel,
		Format:      cfg.LogFormat,
		UnsafeTrace: cfg.UnsafeTrace,
	}
	log := logging.New(logOpts)
	libLog := logging.Library(log, logOpts)

	// One line, once, so a misconfiguration where every caller collapses to
	// one source is visible from the first line of the log (section 12.3).
	log.Info().
		Str("client_source_mode", cfg.ClientSourceMode()).
		Str("public_url", cfg.PublicURL).
		Str("backend", string(cfg.Backend)).
		Msg("starting")
	if cfg.UnsafeTrace {
		log.Warn().Msg("AGENT_GM_UNSAFE_TRACE is set: libgm will log decrypted payloads")
	}

	clk := clock.Real{}
	st, err := store.Open(cfg.DataDir, clk)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitLocalConfig
	}

	// Agent GM tightens what it creates, so a warning here means something
	// outside it loosened the file. It warns rather than refusing, because
	// refusing would turn a fixable disclosure into an outage and an
	// operator who cannot start the server cannot read the explanation.
	for _, w := range store.CheckPermissions(cfg.DataDir) {
		log.Warn().Str("path", w.Path).
			Str("mode", fmt.Sprintf("%04o", w.Mode.Perm())).
			Str("want", fmt.Sprintf("%04o", w.Want)).
			Msg("a file under the data directory is readable by more than its owner; " +
				"message text is not encrypted at rest, so the file mode is the at-rest model")
	}

	sessions, err := store.NewSessionStore(cfg.DataDir, cfg.DataKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitLocalConfig
	}

	set := settings.New(settings.NewRegistry(), store.NewSettingsStore(st), os.Getenv)
	aud := audit.NewWriter(api.NewStoreAppender(st), clk)

	// Crash recovery, before the listener binds and before any account
	// connects. Each row is audited; nothing is retried. If the send did
	// reach Google, the echo will arrive on reconnect and correct the
	// operation, which is exactly why the correction out of `unknown`
	// exists (section 6.6).
	recovered, err := core.RecoverOperations(ctx, st, aud, "startup")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: settling operations left running: %v\n", err)
		return nil, exitContract
	}
	if len(recovered) > 0 {
		log.Warn().Int("operations", len(recovered)).
			Msg("settled operations left running by a previous process; nothing was resent")
	}

	authzSvc, err := authz.New(st, clk, settingsAdapter{set}, nil, authz.Config{
		AdminSecret:       cfg.AdminSecret,
		PublicURL:         cfg.PublicURL,
		TrustedProxyCIDRs: cfg.TrustedProxyRaw,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitLocalConfig
	}
	if revoked, err := authzSvc.RevokeSupersededAdminSessions(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitContract
	} else if len(revoked) > 0 {
		log.Warn().Int("authorizations", len(revoked)).
			Msg("revoked admin sessions minted under a previous AGENT_GM_ADMIN_SECRET")
	}

	signer, err := media.NewSigner(cfg.DataKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitContract
	}

	sup := accounts.New(st, sessions, clk, nil)
	deps := &api.HandlerDeps{
		Store:      st,
		Sessions:   sessions,
		Supervisor: sup,
		Authz:      authzSvc,
		Settings:   set,
		Audit:      aud,
		Signer:     signer,
		Cache: &media.Cache{
			Dir:      cfg.DataDir + "/" + store.MediaCacheDirName,
			Index:    media.NewStoreIndex(st),
			MaxBytes: 2 << 30,
		},
		Clock:     clk,
		DataKey:   cfg.DataKey,
		DataDir:   cfg.DataDir,
		PublicURL: cfg.PublicURL,
		Version:   buildVersion(),
		Commit:    buildCommit(),
		// The AGPL obligation of spec section 1.4: every deployment says
		// where its own source is, at the exact commit it is running. An
		// empty source_url would make GET /v1/health a licence gap rather
		// than merely an incomplete DTO.
		SourceURLBase:         sourceURLBase,
		ConfigVersionCompiled: gm.CompiledConfigVersion().String(),
		UpstreamCommit:        gm.PinnedUpstreamCommit,
		Core:                  core.DefaultConfig(),
		// NewBackend mints the backend a NEW pairing runs on.
		//
		// Under AGENT_GM_BACKEND=fake it hands out a fake, because spec
		// section 13.1 promises that with AGENT_GM_ALLOW_FAKE=1 as well
		// "the CLI, the REST suite and the MCP conformance run all drive a
		// real server with no phone". Refusing here made
		// POST /v1/pairing/start a hard 500 and meant no account could ever
		// exist against a fake-backed server -- which is to say the promise
		// was not kept, and the whole acceptance suite had to build its
		// accounts by writing rows directly.
		NewBackend: func() (gm.Backend, error) {
			if cfg.Backend == config.BackendFake {
				return fake.New(nextFakeAddress()), nil
			}
			return gm.New(libLog), nil
		},
	}

	// The real backfill and the real reconciliation sweep. Leaving these nil
	// is what made both -- including the sweep D26 calls mandatory BECAUSE
	// the event stream loses messages -- never run in the shipped binary,
	// while both passed their tests against injected doubles.
	workers := accounts.NewWorkers(st, func(accountID string) (*core.Account, error) {
		a, err := sup.Get(accountID)
		if err != nil {
			return nil, err
		}
		k := cfg.DataKey
		return &core.Account{
			ID: accountID, Store: st, Backend: a.Backend, Clock: clk,
			Config: core.DefaultConfig(), Audit: aud, Source: "server", DataKey: &k,
		}, nil
	}, nil)
	sup.Sweep = workers
	sup.Backfill = workers

	srv := api.NewServer(api.Deps{
		Authz:     authzSvc,
		PublicURL: cfg.PublicURL,
		LogError: func(msg string, kv ...any) {
			e := log.Error()
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					e = e.Any(k, kv[i+1])
				}
			}
			e.Msg(msg)
		},
		Log: func(msg string, kv ...any) {
			e := log.Debug()
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					e = e.Any(k, kv[i+1])
				}
			}
			e.Msg(msg)
		},
	})
	if err := api.RegisterAll(srv, deps); err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: wiring the routes: %v\n", err)
		return nil, exitContract
	}

	// The authorization server of section 9. It shares the credential
	// layer's store and clock rather than opening its own, so an approval
	// and the token it eventually mints are one database with one writer.
	oauthSrv, err := oauth.New(oauth.Config{
		PublicURL:  cfg.PublicURL,
		SigningKey: cfg.DataKey[:],
		Authz:      authzSvc,
		Clock:      clk,
		// The accounts the authorization screen names under its
		// global-scope disclosure line (section 9.4). It is read at render
		// time, not at startup: a screen that named a stale set would be
		// exactly the dishonesty section 9.7 is trying to avoid.
		Accounts: func() []oauth.Account {
			rows, aerr := st.Accounts(context.Background())
			if aerr != nil {
				return nil
			}
			out := make([]oauth.Account, 0, len(rows))
			for _, row := range rows {
				out = append(out, oauth.Account{
					ID: row.ID, Address: row.GoogleAccount, Label: row.Label,
				})
			}
			return out
		},
		Log: func(msg string, kv ...any) {
			e := log.Debug()
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					e = e.Any(k, kv[i+1])
				}
			}
			e.Msg(msg)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return nil, exitLocalConfig
	}
	deps.OAuth = oauthSrv

	mcpHandler := mcp.New(mcp.Config{
		API:       srv,
		Authz:     authzSvc,
		PublicURL: cfg.PublicURL,
		Version:   buildVersion(),
		Commit:    buildCommit(),
		// The SAME string /v1/health serves, from the one accessor, because
		// the AGPL section 13 obligation of section 1.4 is not kept by two
		// surfaces reporting two different answers (section 16 Slice 3
		// test 28).
		SourceURL: deps.SourceURL(),
		Log: func(msg string, kv ...any) {
			e := log.Debug()
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					e = e.Any(k, kv[i+1])
				}
			}
			e.Msg(msg)
		},
	})

	return &built{
		cfg: cfg, log: log, Store: st, Sessions: sessions,
		Sup: sup, Deps: deps, Server: srv, workers: workers, libLog: libLog,
		OAuth: oauthSrv, MCP: mcpHandler,
		Handler: mcp.Mount(mountOAuth(srv, oauthSrv), mcpHandler),
	}, exitOK
}

// startAccounts resumes every account whose session file is on disk and runs
// any pending reprocess task.
//
// **It runs AFTER the listener binds, deliberately.** Both steps talk to
// phones: on the owner's real deployment the resume took two minutes and the
// reprocess another two, and for those four minutes `/healthz` did not
// answer -- so anything watching "is the service up" concluded it was not,
// which is exactly backwards. Section 7.5 says `/healthz` is `ok` "whenever
// the process is serving", and a process that has bound its socket is
// serving; an account that has not connected yet is a fact about that
// account, reported in its own row and in accounts_summary.
//
// The two things that must NOT wait are already done by the time this runs:
// migrations (section 4.3, before any listener binds) and crash recovery
// (section 6.6, before any account connects). Those settle the DATABASE.
// This connects to the network, and nothing about correctness depends on it
// having finished.
func (b *built) startAccounts(ctx context.Context) {
	resumed, err := resumeAccounts(ctx, b.cfg, b.Store, b.Sessions, b.Sup, b.libLog)
	if err != nil {
		// Not fatal, and it cannot be: the listener is already up. An
		// undecryptable session is still reported loudly, because section
		// 15.4's first runbook row is exactly that.
		b.log.Error().Err(err).Msg("resuming accounts failed; the server is serving " +
			"and the accounts that did not resume are listed with their state")
	}
	b.mu.Lock()
	b.Resumed = resumed
	b.accountsStarted = true
	b.mu.Unlock()
	b.log.Info().Int("accounts", resumed).Msg("resumed")

	// The accounts that resumed, which is the set that can be SWEPT. Every
	// account row is relinked regardless -- see core.RunPendingReprocess.
	var live []string
	for _, a := range b.Sup.List() {
		live = append(live, a.ID)
	}
	if task, ran, err := core.RunPendingReprocess(ctx, b.Store, b.workers, live); err != nil {
		b.log.Warn().Err(err).Str("task", task).
			Msg("a pending reprocess task did not complete; it will be retried at the next start")
	} else if ran {
		b.log.Info().Str("task", task).Int("accounts", len(live)).
			Msg("ran the pending reprocess task and cleared the key")
	}
}

// AccountsStarted reports whether startAccounts has finished, so a test can
// wait on the outcome rather than on a duration.
func (b *built) AccountsStarted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accountsStarted
}

// listenAndServe binds and serves until the context ends.
func (b *built) listenAndServe(ctx context.Context) int {
	cfg, log, sup := b.cfg, b.log, b.Sup

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: binding %s: %v\n", cfg.ListenAddr, err)
		return exitLocalConfig
	}
	httpSrv := &http.Server{
		Handler: b.Handler,
		// A read that never finishes must not hold a connection for ever.
		// The write bound is generous because a send waits on a phone: the
		// worst case of section 6.1 is 271 seconds.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	log.Info().Str("addr", ln.Addr().String()).Msg("listening")

	// Accounts come up behind the listener, so /healthz answers immediately.
	go b.startAccounts(ctx)
	// The 60-second registration sweep of section 9.3, behind the listener
	// for the same reason: it talks to nothing a request depends on.
	go b.oauthMaintenance(ctx)

	done := make(chan error, 1)
	go func() { done <- httpSrv.Serve(ln) }()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
			return exitContract
		}
	case <-ctx.Done():
		log.Info().Msg("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Warn().Err(err).Msg("the listener did not drain cleanly")
		}
		sup.StopAll(context.WithoutCancel(ctx))
	}
	return exitOK
}

// settingsAdapter narrows *settings.Settings to the two-method interface
// internal/authz declares, so the credential layer does not depend on the
// whole settings registry.
type settingsAdapter struct{ s *settings.Settings }

func (a settingsAdapter) Duration(ctx context.Context, key string) (time.Duration, error) {
	eff, err := a.s.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if eff.Value.Kind != settings.KindDuration {
		// A TTL that silently became zero would expire every credential the
		// instant it was minted, so a key of the wrong type is an error
		// rather than a zero.
		return 0, fmt.Errorf("setting %s is a %s, not a duration", key, eff.Value.Kind)
	}
	return eff.Value.Dur, nil
}

// fakeAccountSeq numbers the addresses a fake-backed server pairs as.
var fakeAccountSeq atomic.Int64

// nextFakeAddress is the Google account address a fake pairing yields.
//
// Spec section 13.1 requires the fake to have a **settable**
// `AuthData.Mobile.SourceID` "so a test can pair two distinct accounts, or
// re-pair the same one", and both halves matter here:
//
//   - AGENT_GM_FAKE_ACCOUNT pins the address, so re-pairing produces the SAME
//     `acct_` ID and section 16 Slice 2 test 32's "the same acct_ ID is
//     reused, no second account row appears" is reachable against a real
//     server;
//   - unset, each pairing gets a fresh address, so two accounts can be paired
//     and every multi-account rule -- the ambiguity refusal, per-account
//     idempotency, `accounts.max_concurrent` -- can be driven end to end.
//
// It is reached only under AGENT_GM_BACKEND=fake, which itself requires
// AGENT_GM_ALLOW_FAKE=1 (section 15.1), so a production deployment cannot be
// talked into it.
func nextFakeAddress() string {
	if pinned := os.Getenv("AGENT_GM_FAKE_ACCOUNT"); pinned != "" {
		return pinned
	}
	return fmt.Sprintf("fake-%d@example.test", fakeAccountSeq.Add(1))
}

// resumeAccounts reloads every account whose session file is on disk,
// connects it and starts its ingest goroutine. It never pairs: pairing is a
// physical act at the owner's browser and phone (spec section 11.4).
//
// **One account that cannot be resumed is that account's problem, not the
// server's, and not the NEXT account's.** This used to `return` on the first
// undecryptable session, which made sense while the caller treated that
// return as fatal. It no longer is -- startAccounts runs behind the listener
// and logs the error -- so the return stopped stopping anything and merely
// abandoned the loop: every account after the failing one in row order never
// resumed, held no client, refused writes as `not_signed_in`, and said
// nothing about why, because nothing had touched its row. Which of three
// accounts worked depended on which one had the bad session file.
//
// So each failure is recorded on ITS account and the walk continues, exactly
// as a failed `sup.Start` already did three lines below. The account is
// marked `signed_out` with `credentials` (section 4.7), which is what
// section 15.4's runbook row promises an undecryptable key leaves behind, the
// error is logged loudly and returned to the caller joined with any others,
// and the operator's fix is unchanged: restore the original data key.
func resumeAccounts(
	ctx context.Context,
	cfg config.Config,
	st *store.Store,
	sessions *store.SessionStore,
	sup *accounts.Supervisor,
	libLog zerolog.Logger,
) (int, error) {
	rows, err := st.Accounts(ctx)
	if err != nil {
		return 0, err
	}
	resumed := 0
	var failures []error
	// unresumable records the failure on the account it belongs to and lets
	// the walk carry on to the next one.
	unresumable := func(row store.Account, err error) {
		failures = append(failures, err)
		fmt.Fprintf(os.Stderr, "agent-gm: %s could not be resumed: %v\n", row.ID, err)
		if markErr := sup.MarkUnresumable(ctx, row.ID, accounts.ReasonCredentials); markErr != nil {
			fmt.Fprintf(os.Stderr, "agent-gm: %s could not be marked signed out: %v\n",
				row.ID, markErr)
		}
	}
	for _, row := range rows {
		if !row.SessionPresent {
			continue
		}
		blob, err := sessions.Load(row.ID)
		if err != nil {
			if errors.Is(err, store.ErrSessionUndecryptable) {
				err = fmt.Errorf("%s: session envelope cannot be decrypted; the data "+
					"key differs from the one that sealed it. Restore the original "+
					"AGENT_GM_DATA_KEY -- there is no in-place rotation (spec 15.4)", row.ID)
			} else {
				err = fmt.Errorf("%s: %w", row.ID, err)
			}
			unresumable(row, err)
			continue
		}
		var backend gm.Backend
		if cfg.Backend == config.BackendFake {
			f := fake.New("")
			if err := f.LoadSession(blob); err != nil {
				unresumable(row, fmt.Errorf("%s: %w", row.ID, err))
				continue
			}
			backend = f
		} else {
			b, err := gm.NewFromSession(blob, libLog)
			if err != nil {
				unresumable(row, fmt.Errorf("%s: %w", row.ID, err))
				continue
			}
			backend = b
		}
		a := sup.Adopt(ctx, row.ID, row.GoogleAccount, backend)
		if err := sup.Start(ctx, a); err != nil {
			// One account that will not connect is that account's problem,
			// not the server's: the rest still serve, and its state and
			// state_reason say why (spec section 4.7).
			fmt.Fprintf(os.Stderr, "agent-gm: %s could not connect: %v\n", row.ID, err)
			continue
		}
		resumed++
	}
	return resumed, errors.Join(failures...)
}

// sourceURLBase is where this program's source lives. GET /v1/health serves
// it with the running commit appended (spec sections 1.4, 7.5), so anyone the
// service is offered to can fetch the exact source it is running -- which is
// what the AGPL requires and what this project chose to make trivially easy
// rather than merely possible.
const sourceURLBase = "https://github.com/thisnick/agent-gm"

// buildVersion and buildCommit are what GET /v1/health reports (spec section
// 7.5) and what the MCP serverInfo carries (section 1.4). They prefer the
// link-time stamp of section 14.1, which is the only one a container build
// has, and fall back to the build's own VCS stamp, which is the only one a
// plain `go build` from a checkout has. Neither is a constant somebody has to
// remember to bump.
func buildVersion() string {
	if version != "" {
		return version
	}
	// An unstamped build -- a checkout, a test binary -- reports the
	// in-development version. The commit, which is the field section 1.4
	// actually leans on, still comes from the VCS stamp below.
	return "1.0.0-slice2"
}

func buildCommit() string {
	if commit != "" {
		return commit
	}
	if v := vcsSetting("vcs.revision"); v != "" {
		return v
	}
	// The one string api.SourceURL recognises as "there is no commit to
	// name", so that /v1/health and serverInfo fall back to the repository
	// root rather than offering a dead <repo>/tree/unknown link.
	return api.UnknownCommit
}

func vcsSetting(key string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}


// mountOAuth serves spec section 9.1's public routes from the authorization
// server and everything else from the REST surface.
//
// The split is by PATH PREFIX rather than by exact route, because an unknown
// path under `/oauth` is the OAuth layer's 404 to answer and not the REST
// router's (section 9.1). Handing `/oauth/nonsense` to the REST router would
// still produce a 404, but with the wrong shape and without the browser
// security headers of section 9.9.
func mountOAuth(next http.Handler, o *oauth.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.Handles(r.URL.Path) {
			o.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// oauthMaintenance removes expired, unactivated registrations every 60
// seconds, auditing each removal (spec section 9.3).
func (b *built) oauthMaintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed, err := b.OAuth.SweepExpiredRegistrations(ctx)
			if err != nil {
				b.log.Warn().Err(err).Msg("sweeping expired client registrations")
				continue
			}
			if len(removed) > 0 {
				b.log.Info().Int("registrations", len(removed)).
					Msg("removed expired client registrations")
			}
		}
	}
}
