package api

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/media"
	"github.com/thisnick/agent-gm/internal/oauth"
	"github.com/thisnick/agent-gm/internal/settings"
	"github.com/thisnick/agent-gm/internal/store"
)

// HandlerDeps is everything the handlers collaborate with.
//
// It is a separate struct from Deps, which the transport needs, because the
// two have different lifetimes and different audiences: Deps is what
// ServeHTTP reaches for on every request, and this is what a handler reaches
// for once the transport has finished. Keeping them apart also keeps the
// middleware chain from acquiring a dependency on the store by accident.
//
// Every field is an explicit collaborator rather than a package-level
// singleton, so section 13.1's "one fake is one account" test can build a
// whole server over two fakes, its own clock and its own temporary directory.
type HandlerDeps struct {
	Freshness  *core.Freshener
	Store      *store.Store
	Sessions   *store.SessionStore
	Supervisor *accounts.Supervisor
	Authz      *authz.Service
	Settings   *settings.Settings
	// OAuth is the authorization server of spec section 9. The admin routes
	// of section 9.5 delegate to it; it is nil in a narrow unit test, and
	// those routes then answer internal_error rather than pretending.
	OAuth   *oauth.Server
	Audit   *audit.Writer
	Signer  *media.Signer
	Cache   *media.Cache
	Clock   clock.Clock
	DataKey store.DataKey
	Log     core.Logger

	// DataDir is where backups, the media cache and staged upload bytes
	// live. The caller of POST /v1/admin/backup does not choose a path
	// (spec section 15.2), so the server has to know one.
	DataDir string
	// StagingDir holds the bytes of a filled upload reservation until the
	// send that consumes them. It defaults to <DataDir>/uploads.
	StagingDir string

	// PublicURL is AGENT_GM_PUBLIC_URL. **Every URL a handler hands out is
	// built from it and never from the request's Host or X-Forwarded-Host**
	// (spec sections 10.3, 12.3), so an agent in a sandbox on another
	// machine reaches the origin the tunnel exposes rather than the one it
	// happened to dial.
	PublicURL string

	// The build facts GET /v1/health reports.
	Version               string
	Commit                string
	SourceURLBase         string
	ConfigVersionCompiled string
	UpstreamCommit        string

	// Core is the ingest and operation configuration a per-request engine
	// is built with.
	Core core.Config

	// NewBackend mints a backend for a pairing that has no account yet. It
	// is nil in a deployment that cannot pair, and POST /v1/pairing/start
	// then answers internal_error rather than pretending.
	NewBackend func() (gm.Backend, error)

	// AfterErase is a fault seam for spec section 16 Slice 2 test 20: it
	// runs between the erasure transaction's commit and the unlink of the
	// cached files, which is the one window where a crash is supposed to
	// leave an orphan file and no orphan row. It is nil in production, and
	// it is placed exactly where the window is rather than anywhere
	// convenient.
	AfterErase func(accountID string, paths []string) error

	// PeerTyping reports the instant a conversation's peer stops being
	// considered to be typing, or nil. It is in-memory live state, never
	// persisted (spec section 7.6).
	PeerTyping func(conversationID string) *time.Time

	// Heartbeat is the SSE keep-alive interval. Zero means the 30 seconds
	// spec section 7.5 fixes; a test sets it shorter rather than waiting.
	Heartbeat time.Duration

	// pairings holds the in-flight pairings. It is unexported and built by
	// RegisterAll: a pairing lives in memory and nowhere else, so that an
	// abandoned one leaves nothing behind (spec section 16 Slice 2 test 38).
	pairings *pairingManager
}

// now reads the injected clock. Nothing in this package calls time.Now.
func (d *HandlerDeps) now() time.Time {
	if d.Clock == nil {
		return time.Now().UTC()
	}
	return d.Clock.Now().UTC()
}

// url builds an absolute URL from AGENT_GM_PUBLIC_URL and a path.
//
// This is the ONLY place a handler may build one. It takes no request and
// has no way to reach a header, which is what makes spec section 16 Slice 2
// test 19 -- "built from AGENT_GM_PUBLIC_URL even when the request carries a
// hostile Host and X-Forwarded-Host" -- true by construction rather than by
// review: there is nothing here to be hostile to.
func (d *HandlerDeps) url(path string) string {
	base := strings.TrimRight(d.PublicURL, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// escapePathSegment makes a value safe to put in a path. Every ID Agent GM
// mints is already URL-safe, so this only ever matters for a value that
// arrived from outside.
func escapePathSegment(s string) string { return url.PathEscape(s) }

// stagingDir is where a filled reservation's bytes wait.
func (d *HandlerDeps) stagingDir() string {
	if d.StagingDir != "" {
		return d.StagingDir
	}
	return d.DataDir + "/uploads"
}

// heartbeat is the SSE keep-alive interval (spec section 7.5).
func (d *HandlerDeps) heartbeat() time.Duration {
	if d.Heartbeat > 0 {
		return d.Heartbeat
	}
	return 30 * time.Second
}

// engine builds the per-account mutation engine of internal/core.
//
// **The handler contains no business logic**: it resolves an account, hands
// the request to one of core's mutation methods, and renders what comes back.
// The order of spec section 6.2 -- and in particular the rule that an
// `unsupported_capability` refusal happens before any operation row exists --
// lives in core, and calling it rather than re-deriving it is what keeps that
// true on every route at once.
func (d *HandlerDeps) engine(accountID, source string) (*core.Account, error) {
	sup, err := d.Supervisor.Get(accountID)
	if err != nil {
		return nil, err
	}
	key := d.DataKey
	return &core.Account{
		Freshness: d.Freshness,
		ID:        accountID,
		Store:     d.Store,
		Backend:   sup.Backend,
		Clock:     d.Clock,
		Config:    d.Core,
		Log:       d.Log,
		Audit:     d.Audit,
		Source:    source,
		DataKey:   &key,
	}, nil
}

// accountRefs is every account, whatever its state, in the shape section
// 7.3's account resolution and its refusal both read from. Filtering by state
// here would make the ambiguity rule depend on which accounts happened to be
// connected, so the same request would sometimes need `account_id` and
// sometimes not.
func (d *HandlerDeps) accountRefs(ctx context.Context) ([]core.AccountRef, error) {
	rows, err := d.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]core.AccountRef, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, core.AccountRef{
			ID:            r.ID,
			GoogleAccount: r.GoogleAccount,
			State:         string(r.State),
		})
	}
	return refs, nil
}

// setting reads one effective integer setting, falling back to its declared
// default when there is no settings service (a narrow unit test).
func (d *HandlerDeps) settingInt(ctx context.Context, key string, fallback int64) int64 {
	if d.Settings == nil {
		return fallback
	}
	eff, err := d.Settings.Get(ctx, key)
	if err != nil {
		return fallback
	}
	if eff.Value.Kind == settings.KindInt && eff.Value.Int != 0 {
		return eff.Value.Int
	}
	return fallback
}

// writeAudit appends one row, tolerating a nil writer. An audit failure never
// fails the request that caused it: the row is the record of something that
// already happened.
func (d *HandlerDeps) writeAudit(ctx context.Context, e audit.Event) {
	if d.Audit == nil {
		return
	}
	if err := d.Audit.Append(ctx, e); err != nil && d.Log != nil {
		d.Log.Warn("audit append failed", "kind", string(e.Kind), "error", err.Error())
	}
}

// peerTypingUntil is the in-memory live typing state of one conversation.
//
// It is a hook rather than a store read because **`peer_typing_until` is held
// in memory only and never persisted** (spec section 7.6): a column for it
// would survive a restart and go on claiming somebody was typing three days
// ago. A deployment that tracks typing installs PeerTyping; one that does not
// serves null, which is the honest answer and the same one every list
// response gives.
func (d *HandlerDeps) peerTypingUntil(conversationID string) *time.Time {
	if d.PeerTyping == nil {
		return nil
	}
	return d.PeerTyping(conversationID)
}
