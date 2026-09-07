package store

// Migrations are forward-only, numbered, and never edited after they ship.
// PRAGMA user_version is the version; a database at a higher version than the
// binary knows refuses to open, naming both numbers. There is no
// down-migration (spec section 4.3).
//
// Migration 0001 covers the tables Slice 1 needs: server_meta, accounts,
// conversations, participants and messages. The remaining tables of spec
// section 4.2 -- contacts, attachments, reactions, operations,
// backfill_state, uploads, tickets, media cache, auth, settings, audit -- and
// the messages_fts index arrive with the slices that use them.
//
// participants.contact_id is declared without its REFERENCES clause here
// because contacts does not exist yet: SQLite accepts a foreign key to a
// missing table at CREATE time and then fails every insert. The clause
// arrives with the contacts table in its own migration.

type migration struct {
	version int
	name    string
	stmts   []string
}

var migrations = []migration{
	migration0001,
	migration0002,
	migration0003,
	migration0004,
	migration0005,
	migration0006,
	migration0007,
}

var migration0001 = migration{
	version: 1,
	name:    "accounts, conversations, participants, messages, server_meta",
	stmts: []string{
		`CREATE TABLE server_meta (
			    key            TEXT PRIMARY KEY,
			    value          TEXT NOT NULL
			)`,

		`CREATE TABLE accounts (
			    id                       TEXT PRIMARY KEY,
			    google_account           TEXT NOT NULL UNIQUE,
			    label                    TEXT,
			    state                    TEXT NOT NULL,
			    state_reason             TEXT,
			    phone_id                 TEXT,
			    gaia_dest_reg_uuid       TEXT,
			    gaia_device_last_seen_ms INTEGER,
			    session_present          INTEGER NOT NULL DEFAULT 0,
			    paired_at_ms             INTEGER,
			    last_event_at_ms         INTEGER,
			    last_sweep_at_ms         INTEGER,
			    backfill_complete_at_ms  INTEGER,
			    created_at_ms            INTEGER NOT NULL,
			    updated_at_ms            INTEGER NOT NULL
			)`,
		`CREATE INDEX accounts_state ON accounts(state)`,

		`CREATE TABLE conversations (
			    id                     TEXT PRIMARY KEY,
			    account_id             TEXT NOT NULL REFERENCES accounts(id),
			    source_id              TEXT NOT NULL,
			    name                   TEXT,
			    is_group               INTEGER NOT NULL DEFAULT 0,
			    conversation_type      TEXT NOT NULL,
			    send_mode_raw          TEXT NOT NULL,
			    folder                 TEXT NOT NULL DEFAULT 'active',
			    unread                 INTEGER NOT NULL DEFAULT 0,
			    pinned                 INTEGER NOT NULL DEFAULT 0,
			    read_only              INTEGER NOT NULL DEFAULT 0,
			    force_rcs_eligible     INTEGER NOT NULL DEFAULT 0,
			    default_outgoing_id    TEXT,
			    latest_message_id      TEXT,
			    last_activity_ms       INTEGER NOT NULL,
			    group_avatar_url       TEXT,
			    sim_payload_json       TEXT,
			    deleted_at_ms          INTEGER,
			    created_at_ms          INTEGER NOT NULL,
			    updated_at_ms          INTEGER NOT NULL,
			    UNIQUE (account_id, source_id)
			)`,
		`CREATE INDEX conversations_activity ON conversations(account_id, last_activity_ms DESC, id DESC)`,
		`CREATE INDEX conversations_all      ON conversations(last_activity_ms DESC, id DESC)`,
		`CREATE INDEX conversations_folder   ON conversations(account_id, folder, last_activity_ms DESC, id DESC)`,
		`CREATE INDEX conversations_name     ON conversations(name COLLATE NOCASE)`,
		`CREATE INDEX conversations_filters  ON conversations(account_id, conversation_type, is_group, unread, deleted_at_ms)`,
		`CREATE INDEX conversations_filters_all ON conversations(conversation_type, is_group, unread, deleted_at_ms, last_activity_ms DESC, id DESC)`,

		`CREATE TABLE participants (
			    id                TEXT PRIMARY KEY,
			    account_id        TEXT NOT NULL REFERENCES accounts(id),
			    conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
			    source_id         TEXT NOT NULL,
			    contact_id        TEXT,
			    display_name      TEXT,
			    first_name        TEXT,
			    phone_e164        TEXT,
			    formatted_number  TEXT,
			    identifier_type   TEXT,
			    is_me             INTEGER NOT NULL DEFAULT 0,
			    is_visible        INTEGER NOT NULL DEFAULT 1,
			    UNIQUE (conversation_id, source_id)
			)`,
		`CREATE INDEX participants_phone     ON participants(account_id, phone_e164)`,
		`CREATE INDEX participants_phone_all ON participants(phone_e164)`,
		`CREATE INDEX participants_name      ON participants(display_name COLLATE NOCASE)`,
		`CREATE INDEX participants_me        ON participants(account_id, is_me) WHERE is_me = 1`,
		`CREATE INDEX participants_conv      ON participants(conversation_id)`,

		`CREATE TABLE messages (
			    id                  TEXT PRIMARY KEY,
			    account_id          TEXT NOT NULL REFERENCES accounts(id),
			    conversation_id     TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
			    source_id           TEXT NOT NULL,
			    kind                TEXT NOT NULL,
			    direction           TEXT NOT NULL,
			    sender_participant  TEXT,
			    text                TEXT,
			    subject             TEXT,
			    delivery_state      TEXT NOT NULL,
			    delivery_state_raw  INTEGER NOT NULL,
			    delivery_error      TEXT,
			    reply_to_message_id TEXT,
			    operation_id        TEXT,
			    tmp_id              TEXT,
			    is_deleted          INTEGER NOT NULL DEFAULT 0,
			    sent_at_ms          INTEGER NOT NULL,
			    ingested_at_ms      INTEGER NOT NULL,
			    updated_at_ms       INTEGER NOT NULL,
			    content_hash        TEXT NOT NULL,
			    UNIQUE (conversation_id, source_id)
			)`,
		`CREATE INDEX messages_conv_time      ON messages(conversation_id, sent_at_ms DESC, id DESC)`,
		`CREATE INDEX messages_acct_time      ON messages(account_id, sent_at_ms DESC, id DESC)`,
		`CREATE INDEX messages_time           ON messages(sent_at_ms DESC, id DESC)`,
		`CREATE INDEX messages_kind_state     ON messages(account_id, kind, delivery_state, sent_at_ms DESC, id DESC)`,
		`CREATE INDEX messages_kind_state_all ON messages(kind, delivery_state, sent_at_ms DESC, id DESC)`,
		`CREATE INDEX messages_sender         ON messages(sender_participant, sent_at_ms DESC, id DESC)`,
		`CREATE INDEX messages_tmp_id         ON messages(account_id, tmp_id) WHERE tmp_id IS NOT NULL`,
		`CREATE INDEX messages_acct_source    ON messages(account_id, source_id)`,

		// The transition table of spec section 4.4, enforced by a SQL
		// trigger as well as in Go. This is the backstop for any writer that
		// does not go through UpsertMessage -- a repair script, a later
		// backfill, a bare sqlite3 session.
		//
		// It encodes the table as the exact set of permitted moves rather
		// than as a rank comparison, because a rank comparison gives every
		// terminal state the same rank and so lets a `failed` message come
		// back as `sent`. TestTriggerAgreesWithTransitionAllowed walks all
		// state pairs and fails if this list and gm.TransitionAllowed ever
		// disagree.
		//
		//\tsending -> sent -> delivered -> read   (forward skips allowed)
		//\tsending|sent      -> failed
		//\tsending           -> canceled
		//\tany               -> deleted
		//\tunknown           -> any
		`CREATE TRIGGER messages_no_backward_delivery
			 BEFORE UPDATE OF delivery_state ON messages
			 FOR EACH ROW WHEN
			     OLD.direction = 'outgoing'
			     AND NEW.delivery_state <> OLD.delivery_state
			     AND NEW.delivery_state <> 'deleted'
			     AND OLD.delivery_state <> 'unknown'
			     AND (OLD.delivery_state || '>' || NEW.delivery_state) NOT IN (
			         'sending>sent',
			         'sending>delivered',
			         'sending>read',
			         'sending>failed',
			         'sending>canceled',
			         'sent>delivered',
			         'sent>read',
			         'sent>failed',
			         'delivered>read'
			     )
			 BEGIN
			     SELECT RAISE(ABORT, 'delivery_state may not move backwards');
			 END`,
	},
}

// Migration 0002 completes spec section 4.2 for Slice 2: contacts,
// attachments, reactions, operations, backfill_state, uploads,
// download_tickets, media_cache_entries, settings, audit_events, and the
// messages_fts index with the triggers that keep it in step with messages.
//
// It also rebuilds `participants` so that contact_id finally carries its
// REFERENCES clause. SQLite cannot add a foreign key to an existing table, so
// this is the twelve-step rebuild: create the new shape, copy, drop, rename,
// recreate the indexes. Migration 0001 shipped and is never edited, which is
// why the clause could not simply be added there (spec section 4.3).
//
// Every statement here runs inside one transaction on the writer goroutine,
// before any listener binds.
var migration0002 = migration{
	version: 2,
	name:    "contacts, attachments, reactions, operations, uploads, tickets, media cache, settings, audit, fts",
	stmts: []string{
		`CREATE TABLE contacts (
		    id            TEXT PRIMARY KEY,
		    account_id    TEXT NOT NULL REFERENCES accounts(id),
		    source_id     TEXT NOT NULL,
		    display_name  TEXT,
		    phone_e164    TEXT,
		    avatar_hash   TEXT,
		    is_top        INTEGER NOT NULL DEFAULT 0,
		    updated_at_ms INTEGER NOT NULL,
		    UNIQUE (account_id, source_id)
		)`,
		`CREATE INDEX contacts_phone    ON contacts(account_id, phone_e164)`,
		`CREATE INDEX contacts_name     ON contacts(display_name COLLATE NOCASE)`,
		`CREATE INDEX contacts_all      ON contacts(updated_at_ms DESC, id DESC)`,
		`CREATE INDEX contacts_top      ON contacts(account_id, is_top) WHERE is_top = 1`,
		`CREATE INDEX contacts_top_all  ON contacts(is_top) WHERE is_top = 1`,

		// participants.contact_id gets its REFERENCES clause now that
		// contacts exists. The rebuild preserves every row and every index.
		`CREATE TABLE participants_new (
		    id                TEXT PRIMARY KEY,
		    account_id        TEXT NOT NULL REFERENCES accounts(id),
		    conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		    source_id         TEXT NOT NULL,
		    contact_id        TEXT REFERENCES contacts(id),
		    display_name      TEXT,
		    first_name        TEXT,
		    phone_e164        TEXT,
		    formatted_number  TEXT,
		    identifier_type   TEXT,
		    is_me             INTEGER NOT NULL DEFAULT 0,
		    is_visible        INTEGER NOT NULL DEFAULT 1,
		    UNIQUE (conversation_id, source_id)
		)`,
		// contact_id is RESOLVED, not copied.
		//
		// The v1 column had no REFERENCES clause, and Slice 1's ingest wrote
		// Google's own contact ID into it. This migration gives the column a
		// foreign key to `contacts`, which it has just created empty -- so
		// copying those values straight across violates the constraint and
		// the whole migration fails. It did, on the owner's real Slice 1
		// database: 47 of 222 participants carried a raw Google contact ID
		// and `agent-gm serve` exited 9 with "FOREIGN KEY constraint failed".
		//
		// The value cannot be repaired here. A `contact_` ID is UUIDv5 over
		// (account, "contact", the Google participant ID) and SQLite cannot
		// compute that, and inventing a contacts row from a participant's
		// display name would be asserting a contact Google never sent. So
		// anything that does not already resolve becomes NULL -- which is
		// what the column means when this account holds no contact for that
		// person -- and the rows are handed to `pending_reprocess` below,
		// which re-links them from the backend after startup.
		`-- all-accounts: a schema migration rewrites every row, for every account
		 INSERT INTO participants_new
		    (id, account_id, conversation_id, source_id, contact_id, display_name,
		     first_name, phone_e164, formatted_number, identifier_type, is_me, is_visible)
		 SELECT p.id, p.account_id, p.conversation_id, p.source_id,
		        (SELECT c.id FROM contacts c
		          WHERE c.account_id = p.account_id AND c.source_id = p.source_id),
		        p.display_name, p.first_name, p.phone_e164, p.formatted_number,
		        p.identifier_type, p.is_me, p.is_visible
		   FROM participants p`,
		// Read the OLD table, before it is dropped: a participant that
		// arrived carrying a contact_id and did not get one back has lost a
		// link this migration could not rebuild, and the startup task
		// re-resolves it from the backend (section 4.3).
		//
		// The condition is "had one and lost it", not "has none now". A
		// participant this account simply holds no contact for legitimately
		// has NULL, and treating that as work would make every fresh
		// deployment do a full re-walk on its first start for nothing.
		`-- all-accounts: a schema migration inspects every row, for every account
		 INSERT INTO server_meta(key, value)
		 SELECT 'pending_reprocess', 'reconcile_participants'
		  WHERE EXISTS (
		        SELECT 1 FROM participants p
		          JOIN participants_new n ON n.id = p.id
		         WHERE p.contact_id IS NOT NULL AND n.contact_id IS NULL
		    )
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,

		`DROP TABLE participants`,
		`ALTER TABLE participants_new RENAME TO participants`,
		`CREATE INDEX participants_phone     ON participants(account_id, phone_e164)`,
		`CREATE INDEX participants_phone_all ON participants(phone_e164)`,
		`CREATE INDEX participants_name      ON participants(display_name COLLATE NOCASE)`,
		`CREATE INDEX participants_me        ON participants(account_id, is_me) WHERE is_me = 1`,
		`CREATE INDEX participants_conv      ON participants(conversation_id)`,

		`CREATE TABLE attachments (
		    id                 TEXT PRIMARY KEY,
		    account_id         TEXT NOT NULL REFERENCES accounts(id),
		    message_id         TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		    part_index         INTEGER NOT NULL,
		    media_id           TEXT,
		    thumbnail_media_id TEXT,
		    decryption_key     BLOB,
		    filename           TEXT,
		    mime_type          TEXT,
		    media_format       TEXT,
		    size_bytes         INTEGER,
		    width              INTEGER,
		    height             INTEGER,
		    download_state     TEXT NOT NULL,
		    sha256             TEXT,
		    UNIQUE (message_id, part_index)
		)`,
		`CREATE INDEX attachments_message ON attachments(message_id)`,

		// One reaction per person per message: Google's picker is
		// single-select and an add over an existing one is SWITCH, not a
		// second entry (D23).
		`CREATE TABLE reactions (
		    id              TEXT PRIMARY KEY,
		    message_id      TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		    participant_id  TEXT NOT NULL,
		    emoji           TEXT,
		    emoji_type      TEXT NOT NULL,
		    is_mine         INTEGER NOT NULL DEFAULT 0,
		    updated_at_ms   INTEGER NOT NULL,
		    UNIQUE (message_id, participant_id)
		)`,
		`CREATE INDEX reactions_message ON reactions(message_id)`,

		// operations is idempotency and status. It is NOT an outbox (D4):
		// there is no queue, so there is no queued and no accepted state.
		// The idempotency key is scoped to the account as well as the caller
		// and kind, because the same key sending to two accounts is two
		// messages to two people (spec section 6.3).
		`CREATE TABLE operations (
		    id                    TEXT PRIMARY KEY,
		    account_id            TEXT NOT NULL REFERENCES accounts(id),
		    kind                  TEXT NOT NULL,
		    authorization_id      TEXT NOT NULL,
		    idempotency_key       TEXT NOT NULL,
		    request_fingerprint   TEXT NOT NULL,
		    conversation_id       TEXT,
		    message_id            TEXT,
		    tmp_id                TEXT,
		    status                TEXT NOT NULL,
		    terminal              INTEGER NOT NULL DEFAULT 0,
		    terminal_at_ms        INTEGER,
		    corrected_at_ms       INTEGER,
		    error_code            TEXT,
		    error_message         TEXT,
		    error_retryable       INTEGER,
		    google_status_raw     INTEGER,
		    request_payload_json  TEXT NOT NULL,
		    created_at_ms         INTEGER NOT NULL,
		    updated_at_ms         INTEGER NOT NULL,
		    UNIQUE (authorization_id, account_id, kind, idempotency_key)
		)`,
		`CREATE INDEX operations_pending ON operations(status) WHERE terminal = 0`,
		`CREATE INDEX operations_caller  ON operations(authorization_id, created_at_ms DESC)`,
		`CREATE INDEX operations_tmp_id  ON operations(account_id, tmp_id) WHERE tmp_id IS NOT NULL`,

		`CREATE TABLE backfill_state (
		    conversation_id  TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
		    account_id       TEXT NOT NULL REFERENCES accounts(id),
		    cursor_item_id   TEXT,
		    cursor_ts_us     INTEGER,
		    oldest_seen_ms   INTEGER,
		    messages_done    INTEGER NOT NULL DEFAULT 0,
		    complete         INTEGER NOT NULL DEFAULT 0,
		    updated_at_ms    INTEGER NOT NULL
		)`,
		`CREATE INDEX backfill_state_account ON backfill_state(account_id)`,

		// Uploads carry NO account_id, deliberately: an upl_ reservation is
		// bytes staged by an authorization, and the account is fixed at send
		// time by the conversation named in send_message (spec section 10.2).
		`CREATE TABLE uploads (
		    id                  TEXT PRIMARY KEY,
		    authorization_id    TEXT NOT NULL,
		    idempotency_key     TEXT,
		    filename            TEXT NOT NULL,
		    mime_type           TEXT NOT NULL,
		    size_bytes          INTEGER NOT NULL,
		    sha256_declared     TEXT,
		    state               TEXT NOT NULL,
		    token_hash          TEXT NOT NULL,
		    redemptions         INTEGER NOT NULL DEFAULT 0,
		    staged_path         TEXT,
		    expires_at_ms       INTEGER NOT NULL,
		    created_at_ms       INTEGER NOT NULL
		)`,
		`CREATE INDEX uploads_auth    ON uploads(authorization_id, created_at_ms DESC)`,
		`CREATE INDEX uploads_expiry  ON uploads(expires_at_ms)`,
		`CREATE UNIQUE INDEX uploads_token ON uploads(token_hash)`,
		`CREATE UNIQUE INDEX uploads_idempotency
		    ON uploads(authorization_id, idempotency_key) WHERE idempotency_key IS NOT NULL`,

		// Download tickets are stateful because a five-use cap cannot be
		// enforced by a signed blob (D24). The counter is incremented in the
		// same transaction that authorises the read.
		`CREATE TABLE download_tickets (
		    token_hash        TEXT PRIMARY KEY,
		    attachment_id     TEXT NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
		    authorization_id  TEXT NOT NULL,
		    redemptions       INTEGER NOT NULL DEFAULT 0,
		    max_redemptions   INTEGER NOT NULL DEFAULT 5,
		    expires_at_ms     INTEGER NOT NULL,
		    created_at_ms     INTEGER NOT NULL
		)`,
		`CREATE INDEX download_tickets_attachment ON download_tickets(attachment_id)`,
		`CREATE INDEX download_tickets_expiry     ON download_tickets(expires_at_ms)`,

		// media_cache_entries is the single authority for cached bytes on
		// disk: attachments carries no cache path and no cached size, so
		// there is exactly one row per cached file and "no orphan row" is a
		// well-defined assertion (spec section 4.2).
		`CREATE TABLE media_cache_entries (
		    attachment_id   TEXT PRIMARY KEY REFERENCES attachments(id) ON DELETE CASCADE,
		    relative_path   TEXT NOT NULL,
		    size_bytes      INTEGER NOT NULL,
		    pinned          INTEGER NOT NULL DEFAULT 0,
		    last_used_ms    INTEGER NOT NULL
		)`,
		`CREATE INDEX media_cache_lru ON media_cache_entries(last_used_ms) WHERE pinned = 0`,

		`CREATE TABLE settings (
		    key           TEXT PRIMARY KEY,
		    value_json    TEXT NOT NULL,
		    updated_at_ms INTEGER NOT NULL
		)`,

		// Audit rows are never rewritten, by a migration or by anything
		// else: a row records what happened at the time it happened
		// (spec sections 4.3, 12.4). account_id survives the account.
		`CREATE TABLE audit_events (
		    id                TEXT PRIMARY KEY,
		    kind              TEXT NOT NULL,
		    account_id        TEXT,
		    authorization_id  TEXT,
		    target_type       TEXT,
		    target_id         TEXT,
		    result            TEXT NOT NULL,
		    source            TEXT,
		    payload_json      TEXT NOT NULL,
		    created_at_ms     INTEGER NOT NULL
		)`,
		`CREATE INDEX audit_kind_time ON audit_events(kind, created_at_ms DESC)`,
		`CREATE INDEX audit_time      ON audit_events(created_at_ms DESC)`,
		`CREATE INDEX audit_auth      ON audit_events(authorization_id, created_at_ms DESC)`,
		`CREATE INDEX audit_account   ON audit_events(account_id, created_at_ms DESC)`,

		// audit_events.account_id deliberately carries NO foreign key. The
		// trail of a removed account survives it (spec section 4.7), which a
		// reference to accounts(id) would forbid.

		// Both search modes run against this index. `words` tokenises the
		// query and ANDs the terms; `exact` runs the same query and then
		// filters the page in SQL by substring, so neither mode is an
		// unindexed table scan (spec section 7.6).
		`CREATE VIRTUAL TABLE messages_fts USING fts5(
		    text, subject, content='messages', content_rowid='rowid', tokenize='unicode61'
		)`,
		// External-content FTS5 does not follow its content table on its
		// own: without these three the index silently drifts and search
		// starts answering with rows that no longer say what it thinks.
		`CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN
		    INSERT INTO messages_fts(rowid, text, subject) VALUES (new.rowid, new.text, new.subject);
		 END`,
		`CREATE TRIGGER messages_fts_delete AFTER DELETE ON messages BEGIN
		    INSERT INTO messages_fts(messages_fts, rowid, text, subject)
		        VALUES('delete', old.rowid, old.text, old.subject);
		 END`,
		`CREATE TRIGGER messages_fts_update AFTER UPDATE ON messages BEGIN
		    INSERT INTO messages_fts(messages_fts, rowid, text, subject)
		        VALUES('delete', old.rowid, old.text, old.subject);
		    INSERT INTO messages_fts(rowid, text, subject) VALUES (new.rowid, new.text, new.subject);
		 END`,
		// Rows written before this migration are indexed once, here, rather
		// than by a hand-written data migration. It reads every account's
		// messages on purpose -- a migration is not a query on behalf of a
		// caller, so the account-predicate rule of section 13.2 does not
		// apply to it and the marker says so explicitly rather than leaving
		// the scan looking like an oversight.
		`-- all-accounts: a migration indexes every row, for every account
		 INSERT INTO messages_fts(rowid, text, subject) SELECT rowid, text, subject FROM messages`,
	},
}

// Migration 0003 adds the credential tables Slice 2 needs: `authorizations`,
// `tokens` and the durable failure limiter `oauth_attempts` (spec section
// 4.2, listed there with elided bodies because section 9 defines their
// contents).
//
// Slice 2 mints exactly one kind of authorization -- the admin bootstrap of
// section 9.7 -- but the shape is the OAuth one, because Slice 3 adds a
// second token SOURCE and not a second security model. Two columns carry the
// rules that make a narrowed admin session mean something:
//
//   - `minted_scopes` is what the session was minted with, and a refresh may
//     only narrow relative to THAT, never back up to it (spec section 9.6).
//     Without it a narrowed session is one refresh away from full privilege.
//   - `secret_generation` records which AGENT_GM_ADMIN_SECRET minted an admin
//     authorization, so changing the secret revokes every previous admin
//     bootstrap on the next start (spec section 12.1).
//
// Only hashes are stored. No token value is ever written to the database, a
// log, or an audit payload (spec section 9.6).
var migration0003 = migration{
	version: 3,
	name:    "authorizations, tokens, oauth_attempts",
	stmts: []string{
		`CREATE TABLE authorizations (
		    id                TEXT PRIMARY KEY,
		    kind              TEXT NOT NULL,
		    client_id         TEXT,
		    scopes            TEXT NOT NULL,
		    minted_scopes     TEXT NOT NULL,
		    secret_generation TEXT,
		    source            TEXT,
		    revoked_at_ms     INTEGER,
		    expires_at_ms     INTEGER,
		    created_at_ms     INTEGER NOT NULL,
		    updated_at_ms     INTEGER NOT NULL
		)`,
		`CREATE INDEX authorizations_kind ON authorizations(kind, created_at_ms DESC)`,
		`CREATE INDEX authorizations_live ON authorizations(kind) WHERE revoked_at_ms IS NULL`,

		`CREATE TABLE tokens (
		    token_hash       TEXT PRIMARY KEY,
		    authorization_id TEXT NOT NULL REFERENCES authorizations(id) ON DELETE CASCADE,
		    kind             TEXT NOT NULL,
		    family_id        TEXT NOT NULL,
		    spent_at_ms      INTEGER,
		    revoked_at_ms    INTEGER,
		    expires_at_ms    INTEGER NOT NULL,
		    created_at_ms    INTEGER NOT NULL
		)`,
		`CREATE INDEX tokens_authorization ON tokens(authorization_id, kind)`,
		`CREATE INDEX tokens_family        ON tokens(family_id)`,
		`CREATE INDEX tokens_expiry        ON tokens(expires_at_ms)`,

		// The limit an attacker would restart-cycle to reset, so it is
		// durable rather than in memory (spec sections 9.8, 12.3). The
		// cooldown doubles per further failure in the window and caps at 24
		// hours; a successful presentation does not clear the counter.
		`CREATE TABLE oauth_attempts (
		    kind              TEXT NOT NULL,
		    source            TEXT NOT NULL,
		    window_start_ms   INTEGER NOT NULL,
		    failures          INTEGER NOT NULL DEFAULT 0,
		    cooldown_steps    INTEGER NOT NULL DEFAULT 0,
		    cooldown_until_ms INTEGER,
		    updated_at_ms     INTEGER NOT NULL,
		    PRIMARY KEY (kind, source)
		)`,
		`CREATE INDEX oauth_attempts_cooldown ON oauth_attempts(cooldown_until_ms)`,
	},
}

// Migration 0004 rewrites the two columns that held Google's own participant
// IDs into the derived `part_` IDs section 4.1 requires.
//
// A database written before this migration keeps the raw values in
// `messages.sender_participant` and `reactions.participant_id`. Two things
// break there and neither announces itself:
//
//   - `sender=me`, `sender=<E.164>` and `sender=<part_ id>` return an EMPTY
//     PAGE on every listing and on search, because the query matches those
//     columns against `participants.id`. An empty page is a valid answer, so
//     nothing looks wrong;
//   - a stored `react_` ID disagrees with the one re-derived from the same
//     reaction, so removal by ID misses.
//
// The rewrite is a join rather than a recomputation in Go, because the
// derivation is UUIDv5 over (conversation ID, source ID) and SQLite cannot
// compute that -- but `participants` already holds exactly that mapping, one
// row per (conversation_id, source_id), so the answer is already in the
// database.
//
// **What this migration cannot fix, and says so.** A participant Agent GM
// never ingested has no row to join against, so a message whose sender is one
// of those keeps its raw value. That is why `server_meta.pending_reprocess`
// is set: sections 4.2 and 4.3 define it for exactly this case -- "a
// migration needing derived data recomputed sets pending_reprocess = <task
// name>; the process runs that task once after startup and clears the key" --
// and the reconciliation sweep re-ingests those messages through the current
// path, which derives correctly.
var migration0004 = migration{
	version: 4,
	name:    "derive part_ IDs for message senders and reaction participants",
	stmts: []string{
		// messages.sender_participant: join through the message's own
		// conversation, so a source ID that appears in two conversations
		// resolves to the right participant in each.
		`-- all-accounts: a schema migration rewrites every row, for every account
		 UPDATE messages
		    SET sender_participant = (
		        SELECT p.id FROM participants p
		         WHERE p.conversation_id = messages.conversation_id
		           AND p.source_id       = messages.sender_participant
		    )
		  WHERE sender_participant IS NOT NULL
		    AND sender_participant NOT LIKE 'part\_%' ESCAPE '\'
		    AND EXISTS (
		        SELECT 1 FROM participants p
		         WHERE p.conversation_id = messages.conversation_id
		           AND p.source_id       = messages.sender_participant
		    )`,

		// reactions.participant_id: the same join, reached through the
		// reaction's message.
		`-- all-accounts: a schema migration rewrites every row, for every account
		 UPDATE reactions
		    SET participant_id = (
		        SELECT p.id FROM participants p
		          JOIN messages m ON m.conversation_id = p.conversation_id
		         WHERE m.id = reactions.message_id
		           AND p.source_id = reactions.participant_id
		    )
		  WHERE participant_id NOT LIKE 'part\_%' ESCAPE '\'
		    AND EXISTS (
		        SELECT 1 FROM participants p
		          JOIN messages m ON m.conversation_id = p.conversation_id
		         WHERE m.id = reactions.message_id
		           AND p.source_id = reactions.participant_id
		    )`,

		// Anything the join could not resolve -- a participant Agent GM never
		// ingested -- is left alone and handed to the task section 4.3
		// defines for exactly this: `pending_reprocess`, which the process
		// runs once after startup and then clears.
		//
		// The key is set ONLY when something is actually left, which matters
		// because a brand-new database runs every migration too: setting it
		// unconditionally would make a first start do a full re-walk of an
		// account with nothing to fix, on every fresh deployment.
		`-- all-accounts: a schema migration inspects every row, for every account
		 INSERT INTO server_meta(key, value)
		 SELECT 'pending_reprocess', 'reconcile_participants'
		  WHERE EXISTS (
		        SELECT 1 FROM messages
		         WHERE sender_participant IS NOT NULL
		           AND sender_participant NOT LIKE 'part\_%' ESCAPE '\'
		    ) OR EXISTS (
		        SELECT 1 FROM reactions
		         WHERE participant_id NOT LIKE 'part\_%' ESCAPE '\'
		    )
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
	},
}

// Migration 0005 gives an operation the plaintext size of the media it sent.
//
// Google's echo of our own outgoing media carries a MediaContent with no
// Size: the live gate found every outgoing attachment row written with
// `size_bytes` null, while the same account's INCOMING attachments carried
// theirs. The size is not lost -- the upload reservation counted the bytes and
// the send handed them to Google -- it simply had nowhere to live between the
// send and the echo that correlates it, which may arrive after a restart. So
// it lives on the operation row, and step 8 of section 5.3 writes it onto the
// attachment when it links the echo.
//
// Rows written before this migration keep a null here, and their attachments
// keep a null size. There is nothing to recompute: the bytes are gone.
var migration0005 = migration{
	version: 5,
	name:    "operations.media_size_bytes",
	stmts: []string{
		`ALTER TABLE operations ADD COLUMN media_size_bytes INTEGER`,
	},
}

// SchemaVersion is the highest migration this binary knows.
func SchemaVersion() int { return migrations[len(migrations)-1].version }

// Migration 0006 adds the four OAuth tables of spec section 4.2 that Slice 3
// needs: `oauth_clients`, `enrollment_codes`, `authorization_requests` and
// `authorization_codes`.
//
// Three things about the shape are load-bearing and are worth stating here
// rather than in a comment on a column:
//
//   - **Only hashes are stored.** An enrollment code, an authorization code
//     and the two browser secrets (the context cookie handle and the form
//     token) all reach a column as a SHA-256 digest and never as their value
//     (spec sections 9.5, 9.6, section 16 Slice 3 test 12). There is no
//     column anywhere below that could hold a plaintext credential, which is
//     stronger than remembering not to write one.
//   - **`authorization_codes.authorization_id` exists so that a replay can
//     undo the first exchange.** A replayed code is `invalid_grant` AND
//     revokes the tokens the first exchange produced (section 9.6), so the
//     code row has to remember which authorization it minted long after it
//     was consumed. Without this column the second half of that rule could
//     not be carried out at all.
//   - **`oauth_clients.source` and `created_at_ms` carry the registration
//     budget.** Section 9.3 limits registration to 20 per source per hour;
//     counting rows is durable and exact, where an in-memory bucket would
//     forgive a restart -- and a registration is a durable object, unlike an
//     ordinary request.
//
// A registration expires 24 hours after creation unless an authorization
// activates it (`activated_at_ms`), and a maintenance pass removes expired
// unreferenced registrations every 60 seconds, auditing each removal.
var migration0006 = migration{
	version: 6,
	name:    "oauth_clients, enrollment_codes, authorization_requests, authorization_codes",
	stmts: []string{
		`CREATE TABLE oauth_clients (
		    id                         TEXT PRIMARY KEY,
		    client_name                TEXT,
		    redirect_uris              TEXT NOT NULL,
		    grant_types                TEXT NOT NULL,
		    response_types             TEXT NOT NULL,
		    token_endpoint_auth_method TEXT NOT NULL,
		    metadata_json              TEXT NOT NULL,
		    source                     TEXT,
		    activated_at_ms            INTEGER,
		    expires_at_ms              INTEGER,
		    created_at_ms              INTEGER NOT NULL
		)`,
		`CREATE INDEX oauth_clients_source ON oauth_clients(source, created_at_ms)`,
		`CREATE INDEX oauth_clients_expiry ON oauth_clients(expires_at_ms) WHERE activated_at_ms IS NULL`,

		// code_hash is UNIQUE as well as indexed: two enrollment codes that
		// hashed the same would make redemption ambiguous, and the database
		// is the right place to make that impossible.
		`CREATE TABLE enrollment_codes (
		    id              TEXT PRIMARY KEY,
		    code_hash       TEXT NOT NULL UNIQUE,
		    label           TEXT NOT NULL,
		    scopes          TEXT NOT NULL,
		    expires_at_ms   INTEGER NOT NULL,
		    consumed_at_ms  INTEGER,
		    consumed_by     TEXT,
		    revoked_at_ms   INTEGER,
		    revoked_reason  TEXT,
		    created_at_ms   INTEGER NOT NULL,
		    updated_at_ms   INTEGER NOT NULL
		)`,
		`CREATE INDEX enrollment_codes_live ON enrollment_codes(expires_at_ms)
		    WHERE revoked_at_ms IS NULL AND consumed_at_ms IS NULL`,

		`CREATE TABLE authorization_requests (
		    id                    TEXT PRIMARY KEY,
		    client_id             TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
		    redirect_uri          TEXT NOT NULL,
		    state                 TEXT NOT NULL,
		    code_challenge        TEXT NOT NULL,
		    code_challenge_method TEXT NOT NULL,
		    resource              TEXT NOT NULL,
		    requested_scopes      TEXT NOT NULL,
		    selected_scopes       TEXT NOT NULL,
		    granted_scopes        TEXT,
		    enrollment_code_id    TEXT,
		    status                TEXT NOT NULL,
		    deny_reason           TEXT,
		    handle_hash           TEXT NOT NULL,
		    form_token_hash       TEXT NOT NULL,
		    source                TEXT,
		    expires_at_ms         INTEGER NOT NULL,
		    decided_at_ms         INTEGER,
		    completed_at_ms       INTEGER,
		    created_at_ms         INTEGER NOT NULL,
		    updated_at_ms         INTEGER NOT NULL
		)`,
		`CREATE INDEX authorization_requests_status ON authorization_requests(status, created_at_ms DESC)`,

		`CREATE TABLE authorization_codes (
		    code_hash        TEXT PRIMARY KEY,
		    request_id       TEXT NOT NULL REFERENCES authorization_requests(id) ON DELETE CASCADE,
		    client_id        TEXT NOT NULL,
		    redirect_uri     TEXT NOT NULL,
		    code_challenge   TEXT NOT NULL,
		    scopes           TEXT NOT NULL,
		    resource         TEXT NOT NULL,
		    authorization_id TEXT,
		    consumed_at_ms   INTEGER,
		    expires_at_ms    INTEGER NOT NULL,
		    created_at_ms    INTEGER NOT NULL
		)`,
		`CREATE INDEX authorization_codes_request ON authorization_codes(request_id)`,
	},
}

// Migration 0007 makes the idempotency key OPTIONAL (owner decision D38).
//
// Until now `operations.idempotency_key` was `NOT NULL` and part of an inline
// `UNIQUE (authorization_id, account_id, kind, idempotency_key)`, because
// every mutation was required to carry a key. It is not any more: an agent
// cannot invent a stable key across a retry, so the key was friction without
// protection, and a call that supplies none now gets a new operation with a
// server-minted ID.
//
// The column therefore has to be nullable, and the uniqueness has to become a
// PARTIAL index. `''` was not an option: an empty string is a value, so a
// second keyless send by the same authorization to the same account would
// have collided with the first and been refused as a duplicate -- silently
// turning "no key" into "one call, ever". `NULL` is never equal to `NULL` in
// SQLite's unique indexes, and `WHERE idempotency_key IS NOT NULL` states the
// same thing twice on purpose, so the intent survives a reader who does not
// remember that rule.
//
// SQLite cannot drop a NOT NULL or an inline UNIQUE, so the table is rebuilt.
// Nothing references `operations`, so the rename-copy-drop is safe.
var migration0007 = migration{
	version: 7,
	name:    "operations.idempotency_key becomes optional (D38)",
	stmts: []string{
		`ALTER TABLE operations RENAME TO operations_pre0007`,

		`CREATE TABLE operations (
		    id                    TEXT PRIMARY KEY,
		    account_id            TEXT NOT NULL REFERENCES accounts(id),
		    kind                  TEXT NOT NULL,
		    authorization_id      TEXT NOT NULL,
		    idempotency_key       TEXT,
		    request_fingerprint   TEXT NOT NULL,
		    conversation_id       TEXT,
		    message_id            TEXT,
		    tmp_id                TEXT,
		    status                TEXT NOT NULL,
		    terminal              INTEGER NOT NULL DEFAULT 0,
		    terminal_at_ms        INTEGER,
		    corrected_at_ms       INTEGER,
		    error_code            TEXT,
		    error_message         TEXT,
		    error_retryable       INTEGER,
		    google_status_raw     INTEGER,
		    request_payload_json  TEXT NOT NULL,
		    media_size_bytes      INTEGER,
		    created_at_ms         INTEGER NOT NULL,
		    updated_at_ms         INTEGER NOT NULL
		)`,

		`INSERT INTO operations (id, account_id, kind, authorization_id, idempotency_key,
		    request_fingerprint, conversation_id, message_id, tmp_id, status, terminal,
		    terminal_at_ms, corrected_at_ms, error_code, error_message, error_retryable,
		    google_status_raw, request_payload_json, media_size_bytes, created_at_ms, updated_at_ms)
		 SELECT id, account_id, kind, authorization_id, idempotency_key,
		    request_fingerprint, conversation_id, message_id, tmp_id, status, terminal,
		    terminal_at_ms, corrected_at_ms, error_code, error_message, error_retryable,
		    google_status_raw, request_payload_json, media_size_bytes, created_at_ms, updated_at_ms
		   FROM operations_pre0007`,

		`DROP TABLE operations_pre0007`,

		`CREATE INDEX operations_pending ON operations(status) WHERE terminal = 0`,
		`CREATE INDEX operations_caller  ON operations(authorization_id, created_at_ms DESC)`,
		`CREATE INDEX operations_tmp_id  ON operations(account_id, tmp_id) WHERE tmp_id IS NOT NULL`,
		`CREATE UNIQUE INDEX operations_idempotency
		    ON operations(authorization_id, account_id, kind, idempotency_key)
		    WHERE idempotency_key IS NOT NULL`,
	},
}
