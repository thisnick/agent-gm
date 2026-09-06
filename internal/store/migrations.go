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
	{
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
	},
}

// SchemaVersion is the highest migration this binary knows.
func SchemaVersion() int { return migrations[len(migrations)-1].version }
