package api

import (
	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/store"
	"net/http"
)

// GET /healthz and GET /v1/health (spec sections 7.5, 15.3).

// healthz is liveness. **It never touches SQLite**, so it answers while a
// migration is running and while the write goroutine is busy -- which is the
// one moment an orchestrator most needs an answer and the one moment a
// database-backed check would hang. There is nothing here but a constant.
func (d *HandlerDeps) healthz(*Request) (*Response, error) {
	// The BARE body, not an envelope. Section 7.5 writes it out as
	// `200 {"status":"ok"}`, and section 7.1's two envelope shapes are a rule
	// about `/v1` -- which this is not. It matters in practice: a liveness
	// probe is usually a literal string match written by whoever configured
	// the orchestrator, and wrapping the answer would break every one of
	// them for the sake of a consistency nobody asked for.
	return &Response{Raw: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	}}, nil
}

// accountsSummaryDTO is the totals-by-state block of spec section 7.5.
type accountsSummaryDTO struct {
	Total            int `json:"total"`
	Connected        int `json:"connected"`
	Degraded         int `json:"degraded"`
	Error            int `json:"error"`
	SignedOut        int `json:"signed_out"`
	BackfillComplete int `json:"backfill_complete"`
}

// healthDTO is spec section 7.5's shape, exactly.
type healthDTO struct {
	Status                string                   `json:"status"`
	Version               string                   `json:"version"`
	Commit                string                   `json:"commit"`
	SourceURL             string                   `json:"source_url"`
	ConfigVersionCompiled string                   `json:"config_version_compiled"`
	UpstreamCommit        string                   `json:"upstream_commit"`
	AccountsSummary       accountsSummaryDTO       `json:"accounts_summary"`
	Accounts              []accounts.AccountHealth `json:"accounts"`
	// PendingReprocess names the section 4.3 task a migration deferred, while
	// it is still outstanding, and is null the rest of the time -- which is
	// almost always. It is here because the task now runs BEHIND the
	// listener (core.RunPendingReprocess): a caller during that one window
	// can read a query filtered on a derived value -- `sender=me` is the one
	// that matters -- and get fewer rows than it will a moment later. Without
	// this field that window is invisible, and "the page was short once" has
	// no explanation an operator could reach.
	PendingReprocess *string `json:"pending_reprocess"`
	ClientSource     string  `json:"client_source"`
}

// health is `GET /v1/health`.
//
// **`status` describes the server, not the accounts.** It is `ok` whenever
// the process is serving: an account in `signed_out` or `error` is a fact
// about that account, reported in its row and in `accounts_summary`, and it
// does not make Agent GM unhealthy. A deployment with zero accounts is `ok`.
// Anything watching for "is the service up" reads `status`; anything watching
// for "can I send" reads the account. A health check that went red because
// one of five accounts needed re-pairing would be a health check an operator
// learns to ignore.
//
// `client_source` is the source **this** request resolved to, which is how a
// trusted-proxy misconfiguration where every caller collapses to one source
// becomes visible rather than silent (spec section 12.3).
func (d *HandlerDeps) health(r *Request) (*Response, error) {
	list, err := d.Supervisor.Health(r.Ctx)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []accounts.AccountHealth{}
	}
	summary := accountsSummaryDTO{Total: len(list)}
	for _, a := range list {
		switch a.State {
		case store.StateConnected:
			summary.Connected++
		case store.StateDegraded:
			summary.Degraded++
		case store.StateError:
			summary.Error++
		case store.StateSignedOut:
			summary.SignedOut++
		}
		if a.Backfill.State == "complete" {
			summary.BackfillComplete++
		}
	}
	var pending *string
	if task, ok, err := d.Store.Meta(r.Ctx, core.PendingReprocessKey); err == nil && ok && task != "" {
		pending = &task
	}
	return &Response{Data: healthDTO{
		Status:                "ok",
		Version:               d.Version,
		Commit:                d.Commit,
		SourceURL:             d.sourceURL(),
		ConfigVersionCompiled: d.ConfigVersionCompiled,
		UpstreamCommit:        d.UpstreamCommit,
		AccountsSummary:       summary,
		Accounts:              list,
		PendingReprocess:      pending,
		ClientSource:          r.Source,
	}}, nil
}

// sourceURL points at the tree this build came from, so an owner reading
// /v1/health can go and look at the code that answered them.
func (d *HandlerDeps) sourceURL() string {
	if d.SourceURLBase == "" || d.Commit == "" {
		return ""
	}
	return d.SourceURLBase + "/tree/" + d.Commit
}
