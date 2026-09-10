package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

const RefreshTTL = time.Minute

// Freshener is shared by request engines and scheduled workers. Account locks
// coalesce stale reads and prevent a slower refresh overwriting newer results.
type Freshener struct {
	mu    sync.Mutex
	gates map[string]chan struct{}
}

func (f *Freshener) locked(ctx context.Context, id string, fn func() error) error {
	f.mu.Lock()
	if f.gates == nil {
		f.gates = make(map[string]chan struct{})
	}
	gate := f.gates[id]
	if gate == nil {
		gate = make(chan struct{}, 1)
		f.gates[id] = gate
	}
	f.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-gate }()
	return fn()
}

func withSession(ctx context.Context, a *Account, fn func(context.Context) error) error {
	if b, ok := a.Backend.(interface {
		WithSession(context.Context, func(context.Context) error) error
	}); ok {
		return b.WithSession(ctx, fn)
	}
	return fn(ctx)
}

// Refresh records both timestamps after the whole fetch and session drain succeed.
// The start is the catch-up cutoff; completion starts the cache TTL. No push or
// partial page advances this checkpoint.
func (f *Freshener) Refresh(ctx context.Context, a *Account, scope string, force bool) error {
	return f.refresh(ctx, a, scope, force, nil)
}

func (f *Freshener) refresh(ctx context.Context, a *Account, scope string, force bool, requestedSince *time.Time) error {
	return f.locked(ctx, a.ID, func() error {
		last, err := a.Store.LastRefresh(ctx, a.ID, scope)
		if err != nil {
			return err
		}
		cutoff, err := a.Store.RefreshSince(ctx, a.ID, scope)
		if err != nil {
			return err
		}
		started := a.now()
		if !force && last > 0 && started.UnixMilli() >= last && started.Sub(time.UnixMilli(last)) < RefreshTTL {
			return nil
		}
		if a.Log != nil {
			a.Log.Info("refresh started", "account_id", a.ID, "scope", scope)
		}
		err = withSession(ctx, a, func(ctx context.Context) error {
			switch scope {
			case "conversations":
				since, e := a.SweepSince(ctx)
				if e != nil {
					return e
				}
				if cutoff > 0 {
					since = time.UnixMilli(cutoff)
				}
				if requestedSince != nil && requestedSince.Before(since) {
					since = *requestedSince
				}
				return a.reconcile(ctx, since)
			case "contacts":
				return a.backfillContacts(ctx)
			default:
				c, e := a.Store.Conversation(ctx, scope)
				if e != nil {
					return e
				}
				if c.AccountID != a.ID {
					return apierr.NotFound("conversation")
				}
				fresh, e := refreshMetadata(ctx, a, c)
				if e != nil {
					return e
				}
				if cutoff == 0 {
					row, e := a.Store.Account(ctx, a.ID)
					if e != nil {
						return e
					}
					cutoff = row.LastSweepAtMS
				}
				return a.sweepConversation(ctx, *fresh, cutoff)
			}
		})
		if err == nil {
			err = a.Store.SetLastRefresh(ctx, a.ID, scope, started.UnixMilli(), a.now().UnixMilli())
		}
		if a.Log != nil {
			if err != nil {
				a.Log.Warn("refresh failed", "account_id", a.ID, "scope", scope)
			} else {
				a.Log.Info("refresh completed", "account_id", a.ID, "scope", scope)
			}
		}
		if err != nil {
			return fmt.Errorf("refresh %s failed: %w", scope, err)
		}
		return nil
	})
}

func refreshMetadata(ctx context.Context, a *Account, c store.Conversation) (*gm.Conversation, error) {
	fresh, err := a.Backend.GetConversation(ctx, c.SourceID)
	if err != nil {
		return nil, err
	}
	if fresh == nil || fresh.SourceID != c.SourceID {
		return nil, apierr.NotFound("conversation")
	}
	if _, err = a.Ingest().IngestConversation(ctx, *fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func (a *Account) freshSend(ctx context.Context, id string, send func(context.Context) (Result, error)) (Result, error) {
	var result Result
	err := a.Freshness.locked(ctx, a.ID, func() error {
		// Preserve local authorization/state checks before attempting network I/O.
		c, err := a.Store.Conversation(ctx, id)
		if err != nil {
			return err
		}
		if c.AccountID != a.ID {
			return apierr.NotFound("conversation")
		}
		row, err := a.Store.Account(ctx, a.ID)
		if err != nil {
			return err
		}
		if err = CheckAccountWritable(AccountRef{ID: row.ID, State: string(row.State), GoogleAccount: row.GoogleAccount}); err != nil {
			return err
		}
		return withSession(ctx, a, func(ctx context.Context) error {
			if _, err := refreshMetadata(ctx, a, c); err != nil {
				return err
			}
			var err error
			result, err = send(ctx)
			return err
		})
	})
	return result, err
}
