package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Contact is somebody in one account's phone contact list.
//
// The same person in two accounts is two contact rows with two contact_ IDs,
// which is why every contact DTO carries account_id: a cross-account list
// would otherwise be unattributable (spec section 7.6).
type Contact struct {
	ID          string
	AccountID   string
	SourceID    string
	DisplayName string
	PhoneE164   string
	AvatarHash  string
	IsTop       bool
	UpdatedAtMS int64
}

// ErrContactNotFound is returned when no contact has that ID.
var ErrContactNotFound = errors.New("contact not found")

const contactColumns = `id, account_id, source_id, display_name, phone_e164,
	avatar_hash, is_top, updated_at_ms`

func scanContact(sc interface{ Scan(...any) error }) (Contact, error) {
	var c Contact
	var name, phone, avatar sql.NullString
	err := sc.Scan(&c.ID, &c.AccountID, &c.SourceID, &name, &phone, &avatar,
		&c.IsTop, &c.UpdatedAtMS)
	if err != nil {
		return c, err
	}
	c.DisplayName = name.String
	c.PhoneE164 = phone.String
	c.AvatarHash = avatar.String
	return c, nil
}

// UpsertContact writes one contact under its account and returns its
// contact_ ID. avatarHash is a SHA-256 of the avatar bytes or empty: Agent GM
// stores the hash so a caller can detect a change, never the picture
// (spec section 7.6).
func (s *Store) UpsertContact(ctx context.Context, accountID string, c gm.Contact, avatarHash string) (string, error) {
	id := ContactID(accountID, c.SourceID)
	now := s.clock.Now().UnixMilli()
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO contacts (id, account_id, source_id, display_name, phone_e164,
			    avatar_hash, is_top, updated_at_ms)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET
			    display_name  = excluded.display_name,
			    phone_e164    = excluded.phone_e164,
			    avatar_hash   = COALESCE(excluded.avatar_hash, contacts.avatar_hash),
			    is_top        = excluded.is_top,
			    updated_at_ms = excluded.updated_at_ms`,
			id, accountID, c.SourceID, nullString(c.DisplayName), nullString(c.PhoneE164),
			nullString(avatarHash), c.IsTop, now)
		return err
	})
	return id, err
}

// Contact reads one contact by its contact_ ID.
func (s *Store) Contact(ctx context.Context, id string) (Contact, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+contactColumns+`
		   -- all-accounts: a contact_ ID already carries its account (section 4.1).
		   FROM contacts WHERE id = ?`, id)
	c, err := scanContact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrContactNotFound
	}
	return c, err
}

// UpsertParticipants replaces one conversation's participant set, stamping
// account_id on every row. It is the same write UpsertConversation performs
// inline, exposed for the contact-linking pass that runs after contacts are
// known.
func (s *Store) UpsertParticipants(ctx context.Context, accountID, conversationID string, ps []gm.Participant) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM participants
			   -- all-accounts: scoped by conversation_id, which carries the account (section 4.1).
			   WHERE conversation_id = ?`, conversationID); err != nil {
			return err
		}
		for _, p := range ps {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO participants (id, account_id, conversation_id, source_id, contact_id,
				    display_name, first_name, phone_e164, formatted_number, identifier_type,
				    is_me, is_visible)
				VALUES (?,?,?,?,NULL,?,?,?,?,?,?,?)`,
				ParticipantID(conversationID, p.SourceID), accountID, conversationID, p.SourceID,
				nullString(p.DisplayName), nullString(p.FirstName),
				nullString(p.PhoneE164), nullString(p.FormattedNumber), nullString(p.IdentifierType),
				p.IsMe, p.IsVisible)
			if err != nil {
				return err
			}
		}
		return linkParticipantContacts(ctx, tx, accountID, conversationID)
	})
}

// linkParticipantContacts fills participants.contact_id with the contact_ ID
// of the matching contacts row, or leaves it NULL when this account holds no
// contact for that person.
//
// It cannot simply store the Google contact ID the library hands back:
// participants.contact_id references contacts(id), which holds derived
// contact_ IDs (spec section 4.1), so writing a raw Google ID there would
// either violate the foreign key or point at nothing. Both IDs derive from
// the same Google participant ID, so the join is on source_id.
func linkParticipantContacts(ctx context.Context, tx *sql.Tx, accountID, conversationID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE participants
		   SET contact_id = (SELECT c.id FROM contacts c
		                      WHERE c.account_id = ? AND c.source_id = participants.source_id)
		 -- all-accounts: scoped by conversation_id, which carries the account (section 4.1).
		 WHERE conversation_id = ?`, accountID, conversationID)
	return err
}

// RelinkAccountContacts re-runs the participant-to-contact link for a whole
// account, for the ingest pass that learns contacts after the conversations
// that mention them.
func (s *Store) RelinkAccountContacts(ctx context.Context, accountID string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE participants
			   SET contact_id = (SELECT c.id FROM contacts c
			                      WHERE c.account_id = participants.account_id
			                        AND c.source_id = participants.source_id)
			 WHERE account_id = ?`, accountID)
		return err
	})
}
