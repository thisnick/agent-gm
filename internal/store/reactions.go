package store

import (
	"context"
	"database/sql"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Reaction is one person's single reaction to one message.
//
// There is one row per (message, participant): Google's picker is
// single-select and an add over an existing one is SWITCH, not a second entry
// (D23). Emoji is empty when the EmojiType has no unicode of its own, which
// serves as {"emoji": null, "type": "emotify"} rather than being dropped
// (spec section 3.7).
type Reaction struct {
	ID            string
	MessageID     string
	ParticipantID string
	Emoji         string
	HasEmoji      bool
	EmojiType     string
	IsMine        bool
	UpdatedAtMS   int64
}

// ReplaceReactions is a SET-REPLACE of one message's whole reaction set
// (spec section 5.3 step 7). Google sends the complete set on every change,
// so a merge would resurrect a reaction that was removed while Agent GM was
// not listening; the delete and the insert therefore share one transaction.
//
// myParticipantID names this account's own participant in the thread, so
// is_mine is decided here rather than guessed by a later reader; pass "" when
// it is not known.
func (s *Store) ReplaceReactions(ctx context.Context, messageID, myParticipantID string, rs []gm.Reaction) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM reactions WHERE message_id = ?`, messageID); err != nil {
			return err
		}
		for _, r := range rs {
			emoji := any(nil)
			canonical := string(r.Type)
			if r.Emoji != nil {
				emoji = *r.Emoji
				canonical = *r.Emoji
			}
			for _, pid := range r.ParticipantIDs {
				_, err := tx.ExecContext(ctx, `
					INSERT INTO reactions (id, message_id, participant_id, emoji, emoji_type,
					    is_mine, updated_at_ms)
					VALUES (?,?,?,?,?,?,?)
					ON CONFLICT(message_id, participant_id) DO UPDATE SET
					    id            = excluded.id,
					    emoji         = excluded.emoji,
					    emoji_type    = excluded.emoji_type,
					    is_mine       = excluded.is_mine,
					    updated_at_ms = excluded.updated_at_ms`,
					ReactionID(messageID, pid, canonical), messageID, pid, emoji,
					string(r.Type), myParticipantID != "" && pid == myParticipantID, now)
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ReactionsForMessage lists one message's reactions.
func (s *Store) ReactionsForMessage(ctx context.Context, messageID string) ([]Reaction, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, message_id, participant_id, emoji, emoji_type, is_mine, updated_at_ms
		   FROM reactions WHERE message_id = ? ORDER BY participant_id ASC`, messageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Reaction
	for rows.Next() {
		var r Reaction
		var emoji sql.NullString
		if err := rows.Scan(&r.ID, &r.MessageID, &r.ParticipantID, &emoji,
			&r.EmojiType, &r.IsMine, &r.UpdatedAtMS); err != nil {
			return nil, err
		}
		r.Emoji = emoji.String
		r.HasEmoji = emoji.Valid
		out = append(out, r)
	}
	return out, rows.Err()
}

// Reaction reads one reaction by its react_ ID.
func (s *Store) Reaction(ctx context.Context, id string) (Reaction, error) {
	var r Reaction
	var emoji sql.NullString
	err := s.read.QueryRowContext(ctx,
		`SELECT id, message_id, participant_id, emoji, emoji_type, is_mine, updated_at_ms
		   FROM reactions WHERE id = ?`, id).
		Scan(&r.ID, &r.MessageID, &r.ParticipantID, &emoji, &r.EmojiType, &r.IsMine, &r.UpdatedAtMS)
	if err != nil {
		return r, err
	}
	r.Emoji = emoji.String
	r.HasEmoji = emoji.Valid
	return r, nil
}
