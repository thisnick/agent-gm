package core

import (
	"context"
	"encoding/json"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// ConversationPatch is PATCH /v1/conversations/{id} (spec section 7.7):
// archive, unarchive, pin, unpin and mark-unread. Each field is a pointer, so
// "not mentioned" and "set to false" are different requests.
type ConversationPatch struct {
	Archived *bool
	Pinned   *bool
	Unread   *bool
}

// PatchConversation applies one conversation change.
//
// Repeating a change that is already applied returns `changed: false` with
// `operation: null` and calls the backend ZERO times. An idempotent PATCH
// that re-sent the change anyway would be indistinguishable from one that had
// to, and would spend a mutation budget on nothing.
func (a *Account) PatchConversation(ctx context.Context, in ConversationPatch, req Request) (Result, error) {
	conv, err := a.writableConversation(ctx, req.ConversationID, "update")
	if err != nil {
		return Result{}, err
	}

	change, kind, changed := diffConversation(conv, in)
	if !changed {
		return Result{Changed: false}, nil
	}

	return a.runOperation(ctx, kind, req, func(ctx context.Context, _ store.Operation) (Outcome, error) {
		if err := a.Backend.UpdateConversation(ctx, conv.SourceID, change); err != nil {
			return Outcome{}, err
		}
		// Apply the change locally through the same upsert the live stream
		// uses, so a conversation event arriving a moment later is a no-op
		// rather than a fight.
		next := toGMConversation(conv)
		if change.Folder != nil {
			next.Folder = *change.Folder
		}
		if change.Pinned != nil {
			next.Pinned = *change.Pinned
		}
		if change.Unread != nil {
			next.Unread = *change.Unread
		}
		id, err := a.Ingest().IngestConversation(ctx, next)
		return Outcome{ConversationID: id}, err
	})
}

// diffConversation reduces the request to the fields that would actually
// move, and names the operation kind for the audit trail.
func diffConversation(conv store.Conversation, in ConversationPatch) (gm.ConversationChange, string, bool) {
	var change gm.ConversationChange
	kind := KindArchive
	changed := false

	if in.Archived != nil {
		isArchived := conv.Folder == gm.FolderArchive.String()
		if *in.Archived != isArchived {
			folder := gm.FolderInbox
			kind = KindUnarchive
			if *in.Archived {
				folder = gm.FolderArchive
				kind = KindArchive
			}
			change.Folder = &folder
			changed = true
		}
	}
	if in.Pinned != nil && *in.Pinned != conv.Pinned {
		v := *in.Pinned
		change.Pinned = &v
		changed = true
	}
	if in.Unread != nil && *in.Unread != conv.Unread {
		v := *in.Unread
		change.Unread = &v
		changed = true
	}
	return change, kind, changed
}

// toGMConversation rebuilds the gm shape from a stored row, so a local apply
// travels the one upsert function rather than a second, drifting writer. The
// participant set is deliberately left empty: UpsertConversation replaces
// participants only when it is given some, and a PATCH says nothing about
// them.
func toGMConversation(c store.Conversation) gm.Conversation {
	var sim []byte
	if len(c.SIMPayload) > 0 {
		_ = json.Unmarshal(c.SIMPayload, &sim)
	}
	folder := gm.FolderInbox
	switch c.Folder {
	case gm.FolderArchive.String():
		folder = gm.FolderArchive
	case gm.FolderSpamBlocked.String():
		folder = gm.FolderSpamBlocked
	}
	return gm.Conversation{
		SourceID:          c.SourceID,
		Name:              c.Name,
		IsGroup:           c.IsGroup,
		Type:              gm.ConversationType(c.ConversationType),
		SendModeRaw:       gm.SendMode(c.SendModeRaw),
		Folder:            folder,
		Unread:            c.Unread,
		Pinned:            c.Pinned,
		ReadOnly:          c.ReadOnly,
		Deleted:           c.DeletedAtMS != 0,
		DefaultOutgoingID: c.DefaultOutgoingID,
		LatestMessageID:   c.LatestMessageID,
		LastActivity:      msToTime(c.LastActivityMS),
		GroupAvatarURL:    c.GroupAvatarURL,
		SIMPayload:        sim,
	}
}
