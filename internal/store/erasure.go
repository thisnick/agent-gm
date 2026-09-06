package store

import (
	"context"
	"database/sql"
)

// Account removal is the only thing in Agent GM that deletes anything
// (spec section 4.7). Signing out, a RevokePairData from the phone, cookie
// expiry, account_changed, an abandoned re-pair and max_concurrent parking
// each delete zero rows.

// ErasureCounts is what a removal deleted, per table, for the response body
// and the account.removed audit row.
type ErasureCounts struct {
	Conversations     int64
	Participants      int64
	Messages          int64
	Attachments       int64
	Reactions         int64
	BackfillState     int64
	Operations        int64
	Contacts          int64
	MediaCacheEntries int64
}

// ErasureResult is the outcome of EraseAccount.
//
// MediaPaths are the cached relative paths, collected *inside* the delete
// transaction and returned to the caller, which unlinks them only after the
// commit. That order is deliberate: the eviction sweep finds every file it
// deletes through media_cache_entries, so deleting the rows without the files
// would leave bytes nothing references and a cache budget that silently stops
// matching the disk. A crash between the commit and the unlink leaves an
// orphan file, which an operator can delete, and never an orphan row
// (spec section 10.3).
type ErasureResult struct {
	MediaPaths []string
	Counts     ErasureCounts
}

// EraseAccount deletes one account's data in a single transaction.
//
// conversations cascades to participants, messages, attachments, reactions
// and backfill_state; attachments cascades to media_cache_entries and
// download_tickets. operations, contacts and the account row follow, in that
// order: contacts must go after participants, because participants.contact_id
// references contacts(id) with no cascade of its own, so deleting contacts
// first would violate the foreign key rather than the invariant.
//
// Audit rows are NOT deleted. They record what happened when it happened, and
// they keep their account_id, so the trail of a removed account survives it
// (spec sections 4.3, 4.7, 12.4).
func (s *Store) EraseAccount(ctx context.Context, accountID string) (ErasureResult, error) {
	var out ErasureResult
	err := s.Write(ctx, func(tx *sql.Tx) error {
		out = ErasureResult{}

		// Collected before anything is deleted, and inside the transaction:
		// after the commit the rows that name these files are gone.
		paths, err := collectMediaPaths(ctx, tx, accountID)
		if err != nil {
			return err
		}
		out.MediaPaths = paths

		count := func(query string) (int64, error) {
			var n int64
			err := tx.QueryRowContext(ctx, query, accountID).Scan(&n)
			return n, err
		}
		type counter struct {
			dst   *int64
			query string
		}
		for _, c := range []counter{
			{&out.Counts.Conversations, `SELECT COUNT(*) FROM conversations WHERE account_id = ?`},
			{&out.Counts.Participants, `SELECT COUNT(*) FROM participants WHERE account_id = ?`},
			{&out.Counts.Messages, `SELECT COUNT(*) FROM messages WHERE account_id = ?`},
			{&out.Counts.Attachments, `SELECT COUNT(*) FROM attachments WHERE account_id = ?`},
			{&out.Counts.Reactions, `SELECT COUNT(*) FROM reactions r
			     WHERE EXISTS (SELECT 1 FROM messages m WHERE m.id = r.message_id AND m.account_id = ?)`},
			{&out.Counts.BackfillState, `SELECT COUNT(*) FROM backfill_state WHERE account_id = ?`},
			{&out.Counts.Operations, `SELECT COUNT(*) FROM operations WHERE account_id = ?`},
			{&out.Counts.Contacts, `SELECT COUNT(*) FROM contacts WHERE account_id = ?`},
			{&out.Counts.MediaCacheEntries, `SELECT COUNT(*) FROM media_cache_entries e
			     WHERE EXISTS (SELECT 1 FROM attachments a WHERE a.id = e.attachment_id AND a.account_id = ?)`},
		} {
			n, err := count(c.query)
			if err != nil {
				return err
			}
			*c.dst = n
		}

		for _, stmt := range []string{
			`DELETE FROM conversations WHERE account_id = ?`,
			`DELETE FROM operations WHERE account_id = ?`,
			`DELETE FROM contacts WHERE account_id = ?`,
			`DELETE FROM accounts WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, accountID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ErasureResult{}, err
	}
	return out, nil
}

// EffectSentence is the exact wording spec section 4.7 requires the CLI and
// the API to print before a removal. It is defined once so the CLI can print
// the route's own string byte for byte (spec section 16 test 28).
const EffectSentence = "permanently deletes Agent GM's copy of this account's conversations, " +
	"messages, attachments and operations; your Google Messages account and the " +
	"messages in it are untouched"
