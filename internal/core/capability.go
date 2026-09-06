package core

import (
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// The capability checks of spec section 7.8.
//
// Every one of them is emitted BEFORE any operation row exists, so a refused
// action never leaves a record that looks like an attempt. The order of
// checks is part of the contract: resolve the object, check the capability,
// validate the request, THEN create the operation -- which is step 6 of
// section 6.2 sitting entirely above runOperation.
//
// `not_signed_in` is the account-level member of this vocabulary and lives in
// accounts.go as CheckAccountWritable, because it is a property of the
// account rather than of the object. `not_paired` is deliberately not here at
// all: having no accounts is a service-level condition, not a property of a
// conversation.

// CheckConversationActionable refuses an action on a thread Google will not
// let anybody write to: `conversation_read_only` when Conversation.ReadOnly
// is set, and `conversation_deleted` when delete-for-me has been applied
// locally.
func CheckConversationActionable(c store.Conversation, action string) error {
	if c.ReadOnly {
		return apierr.UnsupportedCapability(apierr.ReasonConversationReadOnly,
			c.ID, action, "read_only", true)
	}
	if c.DeletedAtMS != 0 {
		return apierr.UnsupportedCapability(apierr.ReasonConversationDeleted,
			c.ID, action, "deleted", true)
	}
	return nil
}

// CheckReplySupported refuses a reply on an SMS/MMS conversation: replies are
// RCS-only. An SMS thread that silently dropped the reply target would send
// the text anyway, to the right person, with the quoted context gone -- which
// is worse than a refusal, because the agent believes it replied.
func CheckReplySupported(c store.Conversation, replyToMessageID string) error {
	if replyToMessageID == "" {
		return nil
	}
	if c.ConversationType == string(gm.ConversationTypeRCS) {
		return nil
	}
	return apierr.UnsupportedCapability(apierr.ReasonReplyNotSupported,
		c.ID, "reply", "conversation_type", c.ConversationType)
}

// CheckForceRCS refuses force_rcs where capabilities.force_rcs is false --
// section 4.6 derives that as "RCS and SEND_MODE_AUTO", which is exactly the
// force_rcs_eligible column.
func CheckForceRCS(c store.Conversation, forceRCS bool) error {
	if !forceRCS || c.ForceRCSEligible {
		return nil
	}
	return apierr.UnsupportedCapability(apierr.ReasonRCSNotAvailable,
		c.ID, "send", "force_rcs", false)
}

// CheckMyMessage refuses deleting a message the owner did not send. Direction
// is the test: Google only permits an unsend of one's own message, and an
// incoming one is somebody else's.
func CheckMyMessage(m store.Message, action string) error {
	if m.Direction == string(gm.DirectionOutgoing) {
		return nil
	}
	return apierr.UnsupportedCapability(apierr.ReasonNotMyMessage,
		m.ID, action, "direction", m.Direction)
}

// CheckMyReaction refuses removing somebody else's reaction. is_mine is
// decided at write time by the ingest path, so this reads a fact rather than
// re-deriving one.
func CheckMyReaction(r store.Reaction) error {
	if r.IsMine {
		return nil
	}
	return apierr.UnsupportedCapability(apierr.ReasonNotMyReaction,
		r.ID, "remove_reaction", "is_mine", false)
}

// CheckMediaReady refuses an action on an attachment whose bytes are not
// downloaded yet.
func CheckMediaReady(att store.Attachment, action string) error {
	if att.DownloadState == store.DownloadStateAvailable {
		return nil
	}
	return apierr.UnsupportedCapability(apierr.ReasonMediaPending,
		att.ID, action, "download_state", att.DownloadState)
}
