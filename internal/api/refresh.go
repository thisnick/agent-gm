package api

import (
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/store"
)

func (d *HandlerDeps) refreshRead(r *Request, accountID, scope string) error {
	if d.Freshness == nil {
		return nil
	}
	refs, err := d.accountRefs(r.Ctx)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if accountID != "" && ref.ID != accountID {
			continue
		}
		// Signed-out history remains readable by contract. It has no live source.
		if ref.State == string(store.StateSignedOut) || ref.State == string(store.StatePairing) {
			continue
		}
		a, err := d.engine(ref.ID, r.Source)
		if err != nil {
			return err
		}
		if err = d.Freshness.Refresh(r.Ctx, a, scope, false); err != nil {
			return err
		}
	}
	return nil
}

func (d *HandlerDeps) refreshMessages(r *Request, accountID, conversationID string) error {
	if d.Freshness == nil {
		return nil
	}
	if conversationID == "" {
		return d.refreshRead(r, accountID, "conversations")
	}
	c, err := d.Store.Conversation(r.Ctx, conversationID)
	if err != nil {
		return err
	}
	if err = core.CheckObjectAccount("conversation_id", c.ID, c.AccountID, accountID); err != nil {
		return err
	}
	return d.refreshRead(r, c.AccountID, c.ID)
}
