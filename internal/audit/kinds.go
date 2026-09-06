// Package audit is the append-only audit log of spec section 12.4.
//
// Two properties are structural here rather than conventional.
//
//  1. **A row is never updated and never deleted.** The persistence interface
//     this package writes through offers exactly one method, `AppendAudit`.
//     There is no Update and no Delete to call, so no migration and no
//     handler can rewrite history by accident; a reviewer checks the
//     interface rather than every caller.
//
//  2. **Redaction happens on the way in, not by remembering to.** Every
//     payload passes through the redactor of redact.go before it is
//     marshalled. A caller cannot opt out, and a payload carrying message
//     text, a token, a Google cookie, an enrollment code, an authorization
//     code, a Google account address or attachment bytes is refused rather
//     than stored (section 12.2).
package audit

// Kind is an audit event kind. They are constants so that a typo is a compile
// error rather than a row nobody will ever find with `--kind`.
type Kind string

// Account lifecycle and pairing (section 12.4).
const (
	KindAccountPaired       Kind = "account.paired"
	KindAccountResumed      Kind = "account.resumed"
	KindAccountSignedOut    Kind = "account.signed_out"
	KindAccountRemoved      Kind = "account.removed"
	KindAccountLabelChanged Kind = "account.label_changed"
	KindAccountPairFailed   Kind = "account.pair_failed"
	KindAccountStateChanged Kind = "account.state_changed"
)

// Admin authentication (section 12.4). None of these ever carries the
// presented secret; `auth.admin_secret_failed` carries the source and the
// resulting cooldown state and nothing else.
const (
	KindAuthAdminSessionMinted    Kind = "auth.admin_session_minted"
	KindAuthAdminSessionNarrowed  Kind = "auth.admin_session_narrowed"
	KindAuthAdminSessionRefreshed Kind = "auth.admin_session_refreshed"
	KindAuthAdminSecretFailed     Kind = "auth.admin_secret_failed"
	KindAuthRefreshTokenReused    Kind = "auth.refresh_token_reused"
)

// Write operations and their outcomes (section 12.4: "every write operation
// and its outcome"). The outcome is the Event's Result — ok, refused or
// failed — so one kind per operation covers both halves, and a listing
// filtered by `kind_prefix=operation.` returns the whole write history of an
// account.
const (
	KindOperationSend           Kind = "operation.send"
	KindOperationSendMedia      Kind = "operation.send_media"
	KindOperationDeleteMessage  Kind = "operation.delete_message"
	KindOperationAddReaction    Kind = "operation.add_reaction"
	KindOperationRemoveReaction Kind = "operation.remove_reaction"
	KindOperationMarkRead       Kind = "operation.mark_read"
	KindOperationArchive        Kind = "operation.archive"
	KindOperationUnarchive      Kind = "operation.unarchive"
	KindOperationDeleteConv     Kind = "operation.delete_conversation"
	KindOperationStartConv      Kind = "operation.start_conversation"
	KindOperationTyping         Kind = "operation.typing"
	KindOperationCrashRecovered Kind = "operation.crash_recovered"
)

// Ingestion.
const (
	KindMessageStatusOutOfOrder Kind = "message.status_out_of_order"
)

// Settings, backup and the one unsafe switch.
const (
	KindSettingsChanged     Kind = "settings.changed"
	KindAdminBackup         Kind = "admin.backup"
	KindAdminBackupPruned   Kind = "admin.backup_pruned"
	KindSecurityUnsafeTrace Kind = "security.unsafe_trace_enabled"
)

// Enrollment, authorization and clients (section 12.4; Slice 3 writes these).
const (
	KindEnrollmentCreated  Kind = "enrollment.created"
	KindEnrollmentConsumed Kind = "enrollment.consumed"
	KindEnrollmentExpired  Kind = "enrollment.expired"
	KindEnrollmentRevoked  Kind = "enrollment.revoked"

	KindAuthorizationCreated  Kind = "authorization.created"
	KindAuthorizationApproved Kind = "authorization.approved"
	KindAuthorizationDenied   Kind = "authorization.denied"
	KindAuthorizationExpired  Kind = "authorization.expired"
	KindAuthorizationRevoked  Kind = "authorization.revoked"

	KindClientRevoked Kind = "client.revoked"
)

// AccountRule says whether a kind must, must not, or may carry an account_id.
type AccountRule int

const (
	// AccountRequired is an account-shaped kind. Section 12.4: every audit
	// row carries account_id where the event belongs to an account, which is
	// how "what happened to this account" stays answerable after the account
	// is removed (section 4.7). The writer refuses one of these without an
	// account.
	AccountRequired AccountRule = iota
	// AccountForbidden is a server-wide kind: OAuth, settings, backup. The
	// column is NULL, and the writer refuses an account_id on one.
	AccountForbidden
	// AccountOptional is the one shape that is genuinely both.
	AccountOptional
)

// kinds is the declaration of every kind and its account rule. A kind absent
// from this table is not a kind the writer will accept: an unknown string
// cannot become an audit row.
var kinds = map[Kind]AccountRule{
	KindAccountPaired:       AccountRequired,
	KindAccountResumed:      AccountRequired,
	KindAccountSignedOut:    AccountRequired,
	KindAccountRemoved:      AccountRequired,
	KindAccountLabelChanged: AccountRequired,
	KindAccountStateChanged: AccountRequired,
	// account.pair_failed is the one account-shaped kind that is optional,
	// and the reason is in section 3.2: the acct_ ID is derived from
	// AuthData.Mobile.SourceID, which a pairing that fails at GaiaInitTimeout
	// never produced. Requiring an account here would mean either dropping
	// the record of the failure or inventing an ID for an account that does
	// not exist. When the failure happens on a re-pair or a cookie refresh,
	// where the account is known, the ID is set and must be.
	KindAccountPairFailed: AccountOptional,

	KindAuthAdminSessionMinted:    AccountForbidden,
	KindAuthAdminSessionNarrowed:  AccountForbidden,
	KindAuthAdminSessionRefreshed: AccountForbidden,
	KindAuthAdminSecretFailed:     AccountForbidden,
	KindAuthRefreshTokenReused:    AccountForbidden,

	KindOperationSend:           AccountRequired,
	KindOperationSendMedia:      AccountRequired,
	KindOperationDeleteMessage:  AccountRequired,
	KindOperationAddReaction:    AccountRequired,
	KindOperationRemoveReaction: AccountRequired,
	KindOperationMarkRead:       AccountRequired,
	KindOperationArchive:        AccountRequired,
	KindOperationUnarchive:      AccountRequired,
	KindOperationDeleteConv:     AccountRequired,
	KindOperationStartConv:      AccountRequired,
	KindOperationTyping:         AccountRequired,
	KindOperationCrashRecovered: AccountRequired,

	KindMessageStatusOutOfOrder: AccountRequired,

	KindSettingsChanged:     AccountForbidden,
	KindAdminBackup:         AccountForbidden,
	KindAdminBackupPruned:   AccountForbidden,
	KindSecurityUnsafeTrace: AccountForbidden,

	KindEnrollmentCreated:  AccountForbidden,
	KindEnrollmentConsumed: AccountForbidden,
	KindEnrollmentExpired:  AccountForbidden,
	KindEnrollmentRevoked:  AccountForbidden,

	KindAuthorizationCreated:  AccountForbidden,
	KindAuthorizationApproved: AccountForbidden,
	KindAuthorizationDenied:   AccountForbidden,
	KindAuthorizationExpired:  AccountForbidden,
	KindAuthorizationRevoked:  AccountForbidden,

	KindClientRevoked: AccountForbidden,
}

// Rule reports the account rule for a kind.
func Rule(k Kind) (AccountRule, bool) {
	r, ok := kinds[k]
	return r, ok
}

// Kinds returns every declared kind.
func Kinds() []Kind {
	out := make([]Kind, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	return out
}

// Result is the outcome column of section 4.2's audit_events.
type Result string

const (
	ResultOK      Result = "ok"
	ResultRefused Result = "refused"
	ResultFailed  Result = "failed"
)
