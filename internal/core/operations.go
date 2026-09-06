package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// The operation kinds of spec section 6.5.
const (
	KindSendText          = "send_text"
	KindSendMedia         = "send_media"
	KindStartConversation = "start_conversation"
	KindMarkRead          = "mark_read"
	KindAddReaction       = "add_reaction"
	KindRemoveReaction    = "remove_reaction"
	KindDeleteMessage     = "delete_message"
	KindDeleteConv        = "delete_conversation"
	KindArchive           = "archive"
	KindUnarchive         = "unarchive"
)

// MaxIdempotencyKeyBytes is spec section 6.3's ceiling on a key.
const MaxIdempotencyKeyBytes = 200

// Request is the part of a mutation that every kind shares: who is asking,
// under which key, over which body.
type Request struct {
	// AuthorizationID scopes the idempotency tuple. Two clients may use the
	// same key value.
	AuthorizationID string
	// Key is the Idempotency-Key header or client_request_id from the body.
	// It is REQUIRED on every mutation.
	Key string
	// Body is the request body exactly as it arrived. The fingerprint is a
	// SHA-256 over its canonical serialisation, so reordered JSON keys are a
	// replay and any changed value is not.
	Body []byte
	// ConversationID is the conv_ ID the operation is about, where there is
	// one.
	ConversationID string
	// Source is the section 12.3 client source, for the audit row.
	Source string
}

// Outcome is what a library call produced. The caller of runOperation fills
// it in; the pipeline decides what it means for the operation row.
type Outcome struct {
	MessageID       string
	ConversationID  string
	GoogleStatusRaw int32
}

// Call is one account's library call, made at step 9 -- after the operation
// row is committed at step 8. The operation is passed in because a send needs
// its tmp_id.
type Call func(ctx context.Context, op store.Operation) (Outcome, error)

// Result is a mutation's answer.
type Result struct {
	// Operation is the row as it stands after the outcome was recorded. It is
	// the zero value only when the mutation was a no-op that creates no
	// operation at all (an unchanged PATCH, or a reaction that is already
	// exactly the one asked for).
	Operation store.Operation
	// HasOperation is false for those no-ops, which serve `operation: null`.
	HasOperation bool
	// Replayed is true when the idempotency key matched and nothing was sent.
	Replayed bool
	// Changed is section 7.7's `changed` field.
	Changed bool
}

// runOperation is spec section 6.2, steps 7 to 11, and it is the ONLY place
// they happen. Steps 1 to 6 -- authenticate, parse, resolve the account,
// resolve the conversation, check the account is usable, check it is
// actionable -- are done by the caller BEFORE this is entered, which is what
// makes "no operation row is created" for an `unsupported_capability` true by
// construction rather than by discipline: there is no path from a capability
// refusal to this function.
//
//  7. Look up the idempotency key.
//  8. INSERT the operation row with status='running' and COMMIT.
//  9. Call that account's libgm client.
//  10. UPDATE the operation with the outcome, and COMMIT.
//  11. Respond.
//
// Step 8 committing before step 9 is what makes the crash story honest: if
// the process dies between 8 and 9, the operation is found at startup in
// `running` and settled to `unknown` by the recovery pass, never silently
// retried. Agent GM never re-sends a message on behalf of a crashed request.
func (a *Account) runOperation(ctx context.Context, kind string, req Request, call Call) (Result, error) {
	var out Result

	// 7a. The key is required, and its shape is checked before anything is
	// written.
	if err := ValidateIdempotencyKey(req.Key); err != nil {
		return out, err
	}

	fingerprint, err := store.RequestFingerprint(req.Body)
	if err != nil {
		return out, apierr.MalformedBody(err.Error())
	}

	// 7b. THE MIRROR HAZARD (section 6.3). A key already used by this
	// authorization for this kind against a DIFFERENT account is refused
	// rather than obeyed: obeying it sends a second real message to a real
	// person. The refusal names the account the key was first used with, so
	// the caller can see what it actually did.
	if other, used, err := a.Store.KeyUsedByAnotherAccount(ctx, req.AuthorizationID, kind, req.Key, a.ID); err != nil {
		return out, err
	} else if used {
		return out, idempotencyKeyCrossedAccounts(req.Key, other, a.ID)
	}

	// 7c. Same key + same fingerprint returns the existing operation, and its
	// message_id, and sends nothing. Same key + a different body is
	// idempotency_conflict and sends nothing either.
	existing, err := a.Store.OperationByKey(ctx, req.AuthorizationID, a.ID, kind, req.Key)
	switch {
	case err == nil && existing.RequestFingerprint == fingerprint:
		return Result{Operation: existing, HasOperation: true, Replayed: true, Changed: false}, nil
	case err == nil:
		return out, apierr.IdempotencyConflict(req.Key)
	case !errors.Is(err, store.ErrOperationNotFound):
		return out, err
	}

	// 8. INSERT running and COMMIT. The tmp_id is minted and written HERE,
	// before the library call, so the echo -- which can arrive before the
	// HTTP response is even written -- always has something to attach to.
	op := store.Operation{
		ID:                 store.OperationID(),
		AccountID:          a.ID,
		Kind:               kind,
		AuthorizationID:    req.AuthorizationID,
		IdempotencyKey:     req.Key,
		RequestFingerprint: fingerprint,
		ConversationID:     req.ConversationID,
		TmpID:              tmpIDFor(kind),
	}
	if err := a.Store.InsertOperation(ctx, op); err != nil {
		return out, err
	}
	op, err = a.Store.Operation(ctx, op.ID)
	if err != nil {
		return out, err
	}

	// A test-only seam standing in for the process dying between the commit
	// at 8 and the call at 9. It is nil in production, and it is the only way
	// to reproduce the crash window without killing a real process mid-test.
	if a.crashBefore != nil {
		if err := a.crashBefore(op); err != nil {
			return Result{Operation: op, HasOperation: true}, err
		}
	}

	// 9. The library call.
	outcome, callErr := call(ctx, op)

	// 10. Record the outcome and COMMIT.
	settled, settleErr := a.settle(ctx, op, outcome, callErr)
	if settleErr != nil {
		return Result{Operation: op, HasOperation: true}, settleErr
	}
	// 11. Respond -- with the classified error where there was one, so the
	// caller gets phone_not_responding with the operation `pending` rather
	// than a bare 500.
	return Result{Operation: settled, HasOperation: true, Changed: callErr == nil}, callErr
}

// settle applies section 6.4's status vocabulary to one outcome.
//
// The critical rule, stated once here and tested: ErrPhoneNotResponding
// produces `pending`, not `failed`. The server accepted the request and the
// phone may still act on it when it wakes; reporting it as a failure invites
// the caller to resend, which sends the message twice.
func (a *Account) settle(ctx context.Context, op store.Operation, outcome Outcome, callErr error) (store.Operation, error) {
	st := store.Settlement{
		Status:          store.OpSucceeded,
		MessageID:       outcome.MessageID,
		ConversationID:  outcome.ConversationID,
		GoogleStatusRaw: outcome.GoogleStatusRaw,
	}
	if callErr != nil {
		e := gm.Classify(callErr)
		if e == nil {
			return op, callErr
		}
		st.Status = store.OpFailed
		if e.KeepsOperationPending {
			// phone_not_responding, and nothing else, keeps it pending. The
			// error is still carried, with terminal derived as false, which
			// is how a caller tells "not yet" from "no".
			st.Status = store.OpPending
		}
		st.ErrorCode = string(e.Code)
		st.ErrorMessage = e.Message
		st.ErrorRetryable = e.Retryable
		if e.GoogleStatusRaw != 0 {
			st.GoogleStatusRaw = e.GoogleStatusRaw
		}
	}
	return a.Store.SettleOperation(ctx, op.ID, st)
}

// tmpIDFor mints the bare UUID of section 6.3 for the kinds that produce a
// message and therefore an echo. It is NOT the op_-prefixed operation ID, and
// it is reused across the in-request retries so a retried send cannot produce
// two correlations.
func tmpIDFor(kind string) string {
	switch kind {
	case KindSendText, KindSendMedia:
		return gm.GenerateTmpID()
	default:
		return ""
	}
}

// ValidateIdempotencyKey is section 6.3's shape rule. An empty key, a key
// over 200 bytes, or a key containing control characters is invalid_request
// naming client_request_id in details.field, and writes nothing.
func ValidateIdempotencyKey(key string) error {
	switch {
	case key == "":
		return idempotencyKeyInvalid("an idempotency key is required on every mutation; " +
			"send it as the Idempotency-Key header or as client_request_id in the body")
	case len(key) > MaxIdempotencyKeyBytes:
		return idempotencyKeyInvalid(fmt.Sprintf(
			"client_request_id must be at most %d bytes", MaxIdempotencyKeyBytes))
	case !utf8.ValidString(key):
		return idempotencyKeyInvalid("client_request_id must be valid UTF-8")
	case strings.ContainsFunc(key, isControl):
		return idempotencyKeyInvalid("client_request_id must not contain control characters")
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

func idempotencyKeyInvalid(msg string) *apierr.Error {
	e := apierr.New(apierr.CodeInvalidRequest, msg)
	e.Details = map[string]any{"field": "client_request_id"}
	return e
}

// idempotencyKeyCrossedAccounts is the mirror hazard's refusal. It is
// invalid_request rather than idempotency_conflict on purpose: the key is not
// in conflict with itself, the caller has aimed a "retry" at a different
// person's phone.
func idempotencyKeyCrossedAccounts(key, firstAccount, thisAccount string) *apierr.Error {
	e := apierr.New(apierr.CodeInvalidRequest, fmt.Sprintf(
		"client_request_id %q was already used for this kind against account %s; "+
			"reusing it against %s is not a replay -- it would send a second real message "+
			"to a real person. Use a fresh key for a genuinely new send.",
		key, firstAccount, thisAccount))
	e.Details = map[string]any{
		"field":            "client_request_id",
		"account_id":       firstAccount,
		"other_account_id": thisAccount,
	}
	return e
}

// --- echo correlation (spec section 6.3, 6.4) -------------------------------

// correlateEcho is step 8 of section 5.3. When the remote echo arrives
// carrying a TmpID this account minted, the ingest loop writes the message's
// msg_ ID onto the operation and, if the operation was pending, settles it.
//
// A `succeeded` or `running` operation keeps its status and only gains its
// message_id: the request itself is the authority on those. A `pending` one
// is settled by the echo -- to `succeeded`, or to `failed` when the echo
// reports a failed status. An `unknown` one is CORRECTED, which is the whole
// reason the transition out of `unknown` exists: if a crashed send did reach
// Google, this is how the operation stops lying.
func (a *Account) correlateEcho(ctx context.Context, messageID string, m gm.Message) error {
	op, err := a.Store.OperationByTmpID(ctx, a.ID, m.TmpID)
	if errors.Is(err, store.ErrOperationNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	st := store.Settlement{Status: op.Status, MessageID: messageID}
	switch op.Status {
	case store.OpPending, store.OpUnknown:
		if m.DeliveryState == gm.DeliveryStateFailed {
			st.Status = store.OpFailed
			st.ErrorCode = string(gm.CodeGoogleError)
			st.ErrorMessage = "the phone reported the message as failed"
		} else {
			st.Status = store.OpSucceeded
		}
		st.GoogleStatusRaw = m.StatusRaw
	}
	if _, err := a.Store.SettleOperation(ctx, op.ID, st); err != nil {
		a.logWarn("settling operation from echo failed",
			"account_id", a.ID, "operation_id", op.ID, "error", err.Error())
	}
	return nil
}
