package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// The pairing routes of spec section 7.5.
//
// A pairing in flight is held **in memory and nowhere else**. That is the
// whole of section 16 Slice 2 test 38: an abandoned pairing, a timed-out one
// and one killed with the process must each leave nothing behind, and the
// cheapest way to guarantee that is to have nothing to leave. In particular
// there is no `pairings` table and no half-built `acct_` row, so an abandoned
// pairing can never trip section 7.3's ambiguity rule and make a
// single-account deployment start demanding `account_id`.

// pairing states, as `GET /v1/pairing/{pairing_id}` reports them.
const (
	pairingWaiting = "waiting"
	pairingPaired  = "paired"
	pairingFailed  = "failed"
	pairingExpired = "expired"
)

// pairing is one in-flight attempt.
type pairing struct {
	ID        string
	State     string
	Emoji     string
	AccountID string
	Err       *apierr.Error
	ExpiresAt time.Time
	cancel    context.CancelFunc
}

// pairingManager holds them.
type pairingManager struct {
	mu   sync.Mutex
	byID map[string]*pairing
}

func newPairingManager() *pairingManager {
	return &pairingManager{byID: map[string]*pairing{}}
}

// forAccount is the `pairing_id` an account DTO carries while a pairing that
// named it is in flight, and "" otherwise.
func (m *pairingManager) forAccount(accountID string) string {
	if m == nil || accountID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.byID {
		if p.AccountID == accountID && p.State == pairingWaiting {
			return id
		}
	}
	return ""
}

func (m *pairingManager) get(id string) (*pairing, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.byID[id]
	return p, ok
}

func (m *pairingManager) put(p *pairing) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[p.ID] = p
}

func (m *pairingManager) drop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byID, id)
}

type pairingStartBody struct {
	Cookies     map[string]string `json:"cookies"`
	DeviceIndex *int              `json:"device_index"`
	AccountID   string            `json:"account_id"`
}

type pairingDTO struct {
	PairingID string    `json:"pairing_id"`
	State     string    `json:"state"`
	Emoji     *string   `json:"emoji"`
	AccountID *string   `json:"account_id"`
	Error     *errorDTO `json:"error"`
}

func pairingDTOFrom(p *pairing) pairingDTO {
	out := pairingDTO{
		PairingID: p.ID,
		State:     p.State,
		Emoji:     nullable(p.Emoji),
		AccountID: nullable(p.AccountID),
	}
	if p.Err != nil {
		out.Error = &errorDTO{
			Code:      string(p.Err.Code),
			Message:   p.Err.Message,
			Retryable: p.Err.Retryable(),
			Details:   p.Err.Details,
		}
	}
	return out
}

// pairingStart is `POST /v1/pairing/start`. **It adds an account, or resumes
// an existing one** (spec section 4.7). There is one pairing flow, so there is
// no `method` field; a body carrying one is `invalid_request` naming it,
// which the inventory already guarantees.
func (d *HandlerDeps) pairingStart(r *Request) (*Response, error) {
	var body pairingStartBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	if len(body.Cookies) == 0 {
		return nil, apierr.New(apierr.CodePairingNoCookies,
			"pairing needs the Google cookies captured from messages.google.com; nothing was created")
	}
	if body.AccountID != "" {
		if e := apierr.CheckIDPrefix(body.AccountID, "account_id", store.PrefixAccount); e != nil {
			return nil, e
		}
	}
	if d.NewBackend == nil {
		return nil, apierr.New(apierr.CodeInternalError,
			"this build has no way to open a Google Messages client")
	}
	backend, err := d.NewBackend()
	if err != nil {
		return nil, err
	}
	deviceIndex := 0
	if body.DeviceIndex != nil {
		deviceIndex = *body.DeviceIndex
	}

	p := &pairing{
		ID:        "pair_" + store.AuditID(),
		State:     pairingWaiting,
		AccountID: body.AccountID,
		ExpiresAt: d.now().Add(5 * time.Minute),
	}
	emojiCh := make(chan string, 1)

	// The pairing runs on its own goroutine with its own context, so the
	// HTTP request that started it can answer with the emoji as soon as
	// Google produces one rather than holding a connection open for the
	// minutes the owner takes to look at their phone.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Ctx))
	p.cancel = cancel
	d.pairings.put(p)

	go func() {
		defer cancel()
		account, err := d.Supervisor.PairAs(ctx, body.AccountID, backend, body.Cookies, deviceIndex,
			func(e string) {
				select {
				case emojiCh <- e:
				default:
				}
			})
		d.pairings.mu.Lock()
		defer d.pairings.mu.Unlock()
		switch {
		case errors.Is(err, context.Canceled):
			// Abandoned. Nothing was written; nothing needs undoing.
			delete(d.pairings.byID, p.ID)
		case err != nil:
			p.State = pairingFailed
			p.Err = apierr.From(err)
		default:
			p.State = pairingPaired
			p.AccountID = account.ID
		}
	}()

	// Wait briefly for the emoji, which is what the caller needs to show the
	// owner. A pairing that has not produced one yet is still `waiting`.
	select {
	case e := <-emojiCh:
		d.pairings.mu.Lock()
		p.Emoji = e
		d.pairings.mu.Unlock()
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
	}

	d.pairings.mu.Lock()
	dto := pairingDTOFrom(p)
	d.pairings.mu.Unlock()
	return &Response{Data: dto}, nil
}

// pairingGet is `GET /v1/pairing/{pairing_id}`: the poll.
func (d *HandlerDeps) pairingGet(r *Request) (*Response, error) {
	p, ok := d.pairings.get(r.Path["pairing_id"])
	if !ok {
		return nil, apierr.NotFound("pairing")
	}
	d.pairings.mu.Lock()
	if p.State == pairingWaiting && d.now().After(p.ExpiresAt) {
		p.State = pairingExpired
		p.Err = apierr.New(apierr.CodePairingTimeout,
			"the owner did not confirm in time; nothing was created")
		if p.cancel != nil {
			p.cancel()
		}
	}
	dto := pairingDTOFrom(p)
	d.pairings.mu.Unlock()
	return &Response{Data: dto}, nil
}

// pairingAbandon is `DELETE /v1/pairing/{pairing_id}`. It cancels the attempt
// and forgets it; because nothing was ever written, there is nothing else to
// undo (spec section 16 Slice 2 test 38).
func (d *HandlerDeps) pairingAbandon(r *Request) (*Response, error) {
	id := r.Path["pairing_id"]
	p, ok := d.pairings.get(id)
	if !ok {
		return nil, apierr.NotFound("pairing")
	}
	if p.cancel != nil {
		p.cancel()
	}
	d.pairings.drop(id)
	return &Response{Status: http.StatusNoContent, Raw: func(http.ResponseWriter) {}}, nil
}

// backendAddress reads the paired Google address from a backend that keeps
// one, without this package having to know how a session is shaped.
func backendAddress(b gm.Backend) string {
	if p, ok := b.(interface{ AccountAddress() string }); ok {
		return p.AccountAddress()
	}
	return ""
}
