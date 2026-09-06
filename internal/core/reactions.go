package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// Reactions are spec section 7.6 and D23.
//
// Two rules decide everything here:
//
//  1. **Canonicalisation is mandatory** (section 3.7). Every inbound emoji
//     goes through EmojiType and back out through its canonical unicode
//     BEFORE a react_ ID is derived or a path segment is matched. Without it,
//     adding "❤" and removing "❤️" are different reactions and the same
//     reaction gets two IDs.
//  2. **One reaction per person per message.** Adding a second, different
//     emoji from the same person REPLACES the first, and the library is told
//     so with SWITCH rather than with an ADD that would leave two.

// ReactionInput is POST /v1/messages/{id}/reactions and its DELETE.
type ReactionInput struct {
	Request
	MessageID string
	// Emoji is whatever the caller sent, in whatever form. It is
	// canonicalised here.
	Emoji string
	// ReactionID is the react_ ID form of the removal. Exactly one of Emoji
	// and ReactionID is given on a remove.
	ReactionID string
}

// AddReaction adds, or SWITCHes, this account's own reaction on a message.
//
// When the owner already has exactly that reaction, there is nothing to do:
// the answer is `changed: false` with `operation: null`, and the backend is
// called zero times. That is not an optimisation, it is the honest answer --
// creating an operation row for a no-op would make "did my reaction land"
// unanswerable from the operation alone.
func (a *Account) AddReaction(ctx context.Context, in ReactionInput) (Result, error) {
	msg, conv, err := a.reactionTarget(ctx, in.MessageID, "add_reaction")
	if err != nil {
		return Result{}, err
	}
	emojiType, canonical := gm.CanonicaliseEmojiInput(in.Emoji)
	if canonical == nil {
		return Result{}, apierr.WrongTypeForField("emoji", "an emoji this account can send")
	}

	mine, hasMine, err := a.myReaction(ctx, msg.ID)
	if err != nil {
		return Result{}, err
	}
	action := gm.ReactionActionAdd
	switch {
	case hasMine && mine.Emoji == *canonical:
		// Already exactly this one.
		return Result{Changed: false}, nil
	case hasMine:
		// A different one from the same person: SWITCH, so one row remains.
		action = gm.ReactionActionSwitch
	}

	req := in.Request
	req.ConversationID = conv.ID
	return a.runOperation(ctx, KindAddReaction, req, func(ctx context.Context, _ store.Operation) (Outcome, error) {
		if err := a.Backend.React(ctx, msg.SourceID, *canonical, action); err != nil {
			return Outcome{}, err
		}
		return Outcome{ConversationID: conv.ID, MessageID: msg.ID},
			a.recordReaction(ctx, conv.ID, msg.ID, conv.DefaultOutgoingID, emojiType, canonical)
	})
}

// RemoveReaction removes this account's own reaction, addressed either by
// emoji or by react_ ID. Somebody else's is `not_my_reaction`, refused before
// any operation row exists.
func (a *Account) RemoveReaction(ctx context.Context, in ReactionInput) (Result, error) {
	msg, conv, err := a.reactionTarget(ctx, in.MessageID, "remove_reaction")
	if err != nil {
		return Result{}, err
	}

	var target store.Reaction
	switch {
	case in.ReactionID != "":
		if e := apierr.CheckIDPrefix(in.ReactionID, "reaction_id", store.PrefixReaction); e != nil {
			return Result{}, e
		}
		target, err = a.Store.Reaction(ctx, in.ReactionID)
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, apierr.NotFound("No such reaction.")
		}
		if err != nil {
			return Result{}, err
		}
	case in.Emoji != "":
		_, canonical := gm.CanonicaliseEmojiInput(in.Emoji)
		if canonical == nil {
			return Result{}, apierr.WrongTypeForField("emoji", "an emoji this account can send")
		}
		found := false
		rs, err := a.Store.ReactionsForMessage(ctx, msg.ID)
		if err != nil {
			return Result{}, err
		}
		for _, r := range rs {
			if r.Emoji == *canonical {
				target, found = r, true
				break
			}
		}
		if !found {
			return Result{}, apierr.NotFound("No such reaction.")
		}
	default:
		return Result{}, apierr.MissingParameter("emoji")
	}

	// Section 7.8, before any operation row exists.
	if err := CheckMyReaction(target); err != nil {
		return Result{}, err
	}

	req := in.Request
	req.ConversationID = conv.ID
	return a.runOperation(ctx, KindRemoveReaction, req, func(ctx context.Context, _ store.Operation) (Outcome, error) {
		if err := a.Backend.React(ctx, msg.SourceID, target.Emoji, gm.ReactionActionRemove); err != nil {
			return Outcome{}, err
		}
		return Outcome{ConversationID: conv.ID, MessageID: msg.ID}, a.dropReaction(ctx, msg.ID, target.ParticipantID)
	})
}

// reactionTarget resolves the message and its thread and runs steps 4 to 6.
func (a *Account) reactionTarget(ctx context.Context, messageID, action string) (store.Message, store.Conversation, error) {
	msg, err := a.message(ctx, messageID)
	if err != nil {
		return msg, store.Conversation{}, err
	}
	conv, err := a.writableConversation(ctx, msg.ConversationID, action)
	return msg, conv, err
}

func (a *Account) myReaction(ctx context.Context, messageID string) (store.Reaction, bool, error) {
	rs, err := a.Store.ReactionsForMessage(ctx, messageID)
	if err != nil {
		return store.Reaction{}, false, err
	}
	for _, r := range rs {
		if r.IsMine {
			return r, true, nil
		}
	}
	return store.Reaction{}, false, nil
}

// recordReaction applies the local half of an add or a switch. The reaction
// set is a set-replace whenever Google sends one, so this only has to hold
// the truth until the next echo does.
func (a *Account) recordReaction(ctx context.Context, conversationID, messageID, sourceParticipantID string, t gm.EmojiType, canonical *string) error {
	if sourceParticipantID == "" {
		return nil
	}
	// The caller passes the conversation's own DefaultOutgoingID, which is
	// Google's participant ID. Everything stored and served is the derived
	// part_ ID (spec section 4.1).
	participantID := store.ParticipantID(conversationID, sourceParticipantID)
	rs, err := a.Store.ReactionsForMessage(ctx, messageID)
	if err != nil {
		return err
	}
	next := []gm.Reaction{{Emoji: canonical, Type: t, ParticipantIDs: []string{participantID}}}
	for _, r := range rs {
		if r.ParticipantID == participantID {
			continue // one reaction per person per message
		}
		next = append(next, toGMReaction(r))
	}
	// "" means "these are already derived part_ IDs": deriving twice would
	// produce an ID for a participant that does not exist.
	return a.Store.ReplaceReactions(ctx, "", messageID, participantID, next)
}

func (a *Account) dropReaction(ctx context.Context, messageID, participantID string) error {
	rs, err := a.Store.ReactionsForMessage(ctx, messageID)
	if err != nil {
		return err
	}
	var next []gm.Reaction
	for _, r := range rs {
		if r.ParticipantID == participantID {
			continue
		}
		next = append(next, toGMReaction(r))
	}
	return a.Store.ReplaceReactions(ctx, "", messageID, participantID, next)
}

func toGMReaction(r store.Reaction) gm.Reaction {
	var emoji *string
	if r.HasEmoji {
		v := r.Emoji
		emoji = &v
	}
	return gm.Reaction{
		Emoji:          emoji,
		Type:           gm.EmojiType(r.EmojiType),
		ParticipantIDs: []string{r.ParticipantID},
	}
}

// ReactionPathSegment canonicalises the emoji in a URL path segment, so
// DELETE .../reactions/❤️ and DELETE .../reactions/❤ address the same row.
func ReactionPathSegment(segment string) string {
	if strings.HasPrefix(segment, store.PrefixReaction) {
		return segment
	}
	if _, canonical := gm.CanonicaliseEmojiInput(segment); canonical != nil {
		return *canonical
	}
	return segment
}
