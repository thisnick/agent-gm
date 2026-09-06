package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/store"
)

// `GET /v1/accounts/{account_id}/events` and `GET /v1/accounts/events`
// (spec section 7.5).
//
// This is **the only streaming route**, and it carries **no message data** --
// one event per account state change, and a heartbeat. That is why it needs
// no replay ring and no cursor: a client that reconnects re-reads
// `GET /v1/accounts` and is immediately correct again, because the stream
// carries transitions rather than history. A stream that carried messages
// would need every one of those things and would be a second, weaker
// pagination surface beside section 7.4's.
//
// The heartbeat is 30 seconds, and it exists for the middleboxes: a
// connection with nothing on it for minutes is one a proxy or a phone's NAT
// will close, and a client would read that as "the server went away" rather
// than "nothing has happened".

// accountsEvents streams one account's state changes.
func (d *HandlerDeps) accountsEvents(r *Request) (*Response, error) {
	id, e := pathID(r, "account_id", store.PrefixAccount)
	if e != nil {
		return nil, e
	}
	if _, err := d.Store.Account(r.Ctx, id); err != nil {
		return nil, apierr.NotFound("account")
	}
	return d.stream(r, id), nil
}

// accountsEventsAll streams every account's state changes, each tagged with
// its `account_id`.
func (d *HandlerDeps) accountsEventsAll(r *Request) (*Response, error) {
	return d.stream(r, ""), nil
}

// stream is the SSE loop.
//
// It is written against http.Flusher rather than assuming one: a
// ResponseWriter that cannot flush would buffer the whole stream and deliver
// it when the client gave up, which is worse than refusing.
func (d *HandlerDeps) stream(r *Request, accountID string) *Response {
	return &Response{Raw: func(w http.ResponseWriter) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Connection", "keep-alive")
		// The security headers the middleware set are already on this
		// response; no-store in particular matters more here than anywhere,
		// because a cached event stream is a stream that never updates.
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		sub := d.Supervisor.Subscribe(accountID)
		defer sub.Close()

		beat := time.NewTicker(d.heartbeat())
		defer beat.Stop()

		for {
			select {
			case <-r.Ctx.Done():
				return
			case change, open := <-sub.Events():
				if !open {
					return
				}
				payload, err := json.Marshal(change)
				if err != nil {
					continue
				}
				// The event name is the transition's kind, so a client can
				// filter without parsing; the data is the whole StateChange,
				// which carries account_id even on the single-account form so
				// the two streams parse identically.
				if _, err := fmt.Fprintf(w, "event: account.state_changed\ndata: %s\n\n", payload); err != nil {
					return
				}
				flusher.Flush()
			case <-beat.C:
				// A comment frame, which every SSE client ignores and every
				// middlebox counts as traffic.
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}}
}
