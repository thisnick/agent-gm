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
	"syscall"
	"time"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/config"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/logging"
	"github.com/thisnick/agent-gm/internal/media"
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

	cfg, err := config.LoadServer()
	if err != nil {
		// This is the "refuses to start" of sections 12.3 and 15.1. The
		// message names the variable and never its value: the admin secret
		// is the strongest credential Agent GM issues.
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return exitLocalConfig
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
		return exitLocalConfig
	}
	defer func() { _ = st.Close() }()

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
		return exitLocalConfig
	}

	set := settings.New(settings.NewRegistry(), store.NewSettingsStore(st), os.Getenv)
	aud := audit.NewWriter(api.NewStoreAppender(st), clk)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Crash recovery, before the listener binds and before any account
	// connects. Each row is audited; nothing is retried. If the send did
	// reach Google, the echo will arrive on reconnect and correct the
	// operation, which is exactly why the correction out of `unknown`
	// exists (section 6.6).
	recovered, err := core.RecoverOperations(ctx, st, aud, "startup")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: settling operations left running: %v\n", err)
		return exitContract
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
		return exitLocalConfig
	}
	if revoked, err := authzSvc.RevokeSupersededAdminSessions(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return exitContract
	} else if len(revoked) > 0 {
		log.Warn().Int("authorizations", len(revoked)).
			Msg("revoked admin sessions minted under a previous AGENT_GM_ADMIN_SECRET")
	}

	signer, err := media.NewSigner(cfg.DataKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: %v\n", err)
		return exitContract
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
		NewBackend: func() (gm.Backend, error) {
			if cfg.Backend == config.BackendFake {
				return nil, errors.New("this build is configured with the fake backend; " +
					"pairing is not available")
			}
			return gm.New(libLog), nil
		},
	}

	srv := api.NewServer(api.Deps{
		Authz:     authzSvc,
		PublicURL: cfg.PublicURL,
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
		return exitContract
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm: binding %s: %v\n", cfg.ListenAddr, err)
		return exitLocalConfig
	}
	httpSrv := &http.Server{
		Handler: srv,
		// A read that never finishes must not hold a connection for ever.
		// The write bound is generous because a send waits on a phone: the
		// worst case of section 6.1 is 271 seconds.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	log.Info().Str("addr", ln.Addr().String()).Msg("listening")
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

// sourceURLBase is where this program's source lives. GET /v1/health serves
// it with the running commit appended (spec sections 1.4, 7.5), so anyone the
// service is offered to can fetch the exact source it is running -- which is
// what the AGPL requires and what this project chose to make trivially easy
// rather than merely possible.
const sourceURLBase = "https://github.com/thisnick/agent-gm"

// buildVersion and buildCommit are what GET /v1/health reports (spec section
// 7.5). They come from the build's own VCS stamp rather than from a constant
// somebody has to remember to bump.
func buildVersion() string { return "1.0.0-slice2" }

func buildCommit() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}
