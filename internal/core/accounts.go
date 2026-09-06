package core

import (
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/store"
)

// AccountRef is the little of an account that choosing one needs: exactly the
// three fields §7.3 says `details.accounts` carries, so the refusal can be
// built from the same value the decision was made on.
type AccountRef struct {
	ID            string `json:"id"`
	GoogleAccount string `json:"google_account"`
	State         string `json:"state"`
}

// Intent is whether the caller is about to read or to write. The two differ,
// and the difference is the whole of §7.3: a read that omits `account_id`
// covers every account, which is a useful default for "what came in today";
// a write that omits it would have to guess which person to text, which is
// not a default anybody wants.
type Intent int

const (
	// Read covers every account when no account is named.
	Read Intent = iota
	// Write must name one when there is more than one account.
	Write
)

// Resolution is the answer: either one account, or every account.
type Resolution struct {
	// AccountID is the single account chosen, or "" for every account.
	AccountID string
	// AllAccounts is true when the query covers every account. It is never
	// true for a Write.
	AllAccounts bool
}

// ResolveAccount implements §7.3.
//
//   - Exactly one account exists → the parameter may be omitted and defaults
//     to it. A single-account deployment never has to think about accounts.
//   - More than one → a **write** must name one; omitting it is
//     `invalid_request` with `details.field = "account_id"` and
//     `details.accounts` listing the candidates, so the caller can retry
//     without a second round trip. A **read** may still omit it and then
//     covers every account.
//   - Zero accounts → any write is `not_paired`; a read returns an empty
//     page, which is a resolution over no accounts rather than an error.
//   - An `acct_` ID that does not exist is `not_found`. A malformed one is
//     `invalid_request` naming the parameter and the expected prefix, never
//     `not_found` (§4.1).
//
// `accounts` is every account the server holds, **whatever its state**. A
// `signed_out` or `parked` account is still a candidate here: it is a real
// account the caller may have meant, and refusing the write for the right
// reason (`unsupported_capability` / `not_signed_in`, at step 5 of §6.2)
// tells the caller far more than pretending it does not exist. Filtering by
// state here would also make the ambiguity rule depend on which accounts
// happened to be connected, so the same request would sometimes need
// `account_id` and sometimes not.
func ResolveAccount(requested string, accounts []AccountRef, intent Intent) (Resolution, error) {
	if requested != "" {
		if !store.HasPrefix(requested, store.PrefixAccount) {
			return Resolution{}, apierr.WrongIDPrefix("account_id", store.PrefixAccount)
		}
		for _, a := range accounts {
			if a.ID == requested {
				return Resolution{AccountID: a.ID}, nil
			}
		}
		return Resolution{}, apierr.NotFound("account")
	}

	switch {
	case len(accounts) == 1:
		return Resolution{AccountID: accounts[0].ID}, nil
	case len(accounts) == 0:
		if intent == Write {
			return Resolution{}, apierr.NotPaired()
		}
		return Resolution{AllAccounts: true}, nil
	default:
		if intent == Read {
			return Resolution{AllAccounts: true}, nil
		}
		return Resolution{}, apierr.AmbiguousAccount(toCandidates(accounts))
	}
}

func toCandidates(accounts []AccountRef) []apierr.AccountCandidate {
	out := make([]apierr.AccountCandidate, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, apierr.AccountCandidate{
			ID:            a.ID,
			GoogleAccount: a.GoogleAccount,
			State:         a.State,
		})
	}
	return out
}

// CheckObjectAccount enforces the other half of §7.3: a `conv_` or `msg_` ID
// that belongs to a different account than the `account_id` given is
// `invalid_request` naming both — **never `not_found`**, which would suggest
// the thread is gone when in fact the caller merely named the wrong account.
//
// `param` is the parameter the object ID arrived under, so the message names
// the thing the caller typed. The refusal itself is apierr's
// IDFromAnotherAccount, which is the one constructor for this shape.
//
// objectAccountID is the account the object really belongs to; it is compared
// here and is not echoed into the error, which names the account_id the
// caller supplied.
func CheckObjectAccount(param, objectID, objectAccountID, requestedAccountID string) error {
	if requestedAccountID == "" || objectAccountID == requestedAccountID {
		return nil
	}
	return apierr.IDFromAnotherAccount(param, objectID, requestedAccountID)
}

// WritableStates are the account states a write may proceed in (§4.7).
// Everything else reads but does not write.
var WritableStates = map[string]bool{
	"connected": true,
	// `degraded` is a transient listen error and the client is still there;
	// the write is likely to fail, and failing at Google is a better answer
	// than refusing locally on a guess.
	"degraded": true,
}

// CheckAccountWritable is step 5 of §6.2, and it runs **before** any
// operation row exists, so a refused action never leaves a record that looks
// like an attempt (§7.8).
//
// Every non-writable state answers `unsupported_capability` with
// `not_signed_in` — a per-account condition, distinct from the service-level
// `not_paired`, which means there are no accounts at all. `parked` is in that
// set because a parked account holds no client to write with; the account's
// own `state` and `state_reason` say which of the reasons it is, which is
// exactly why `state_reason` exists.
func CheckAccountWritable(a AccountRef) error {
	if WritableStates[a.State] {
		return nil
	}
	return apierr.UnsupportedCapability(apierr.ReasonNotSignedIn, a.ID, "write to", "state", a.State)
}
