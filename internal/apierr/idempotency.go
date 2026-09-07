package apierr

// The idempotency key has exactly one transport (spec section 6.3, owner
// decision D38): the `Idempotency-Key` **header**. It is OPTIONAL. There is
// no body field and no query-parameter form on any route; one presented as
// either is an unknown parameter like any other (spec section 7.1).
//
// The `client_request_id` body field was removed because the callers this
// server exists for cannot use it. An agent regenerates its arguments on a
// retry, so it cannot supply a stable key across one, and a key that changes
// every call protects nothing while making every call harder to write. A
// script that retries on a timeout can still ask for the protection by
// sending the header.
//
// Every error here names `Idempotency-Key` in `details.field`, because that
// is the name the caller reads in the API docs and in `agm --help`.

// BadIdempotencyKey is a key that is present but unusable. `why` completes
// the sentence "the idempotency key is refused because ...".
//
// There is deliberately no "missing key" error: an absent header means a new
// operation with a server-minted ID, which is the ordinary case.
func BadIdempotencyKey(why string) *Error {
	return withDetails(CodeInvalidRequest,
		"the Idempotency-Key header is refused because "+why+"; nothing was done",
		map[string]any{"field": "Idempotency-Key"})
}

// IdempotencyKeyUsedWithAnotherAccount is the mirror hazard of spec section
// 6.3, and it is refused rather than obeyed.
//
// Because the account is part of the idempotency tuple, reusing a key
// against a DIFFERENT account_id is not a replay -- it is a new operation,
// and it sends a second real message to a real person. That is the safe
// direction for the store, which never silently adopts a row across
// accounts, and the dangerous direction for a caller that "retries" a failed
// send by switching accounts. So both agent-facing surfaces refuse it,
// naming the account the key was first used with.
//
// It survives D38 unchanged: the hazard belongs to a key that IS present,
// and a caller that bothered to send one is exactly the caller who would
// retry with it.
func IdempotencyKeyUsedWithAnotherAccount(key, firstAccountID string) *Error {
	return withDetails(CodeInvalidRequest,
		"idempotency key "+key+" has already been used by this authorization for this kind "+
			"against account "+firstAccountID+"; sending it to another account would be a new "+
			"message to a different person, not a retry. Use a fresh key",
		map[string]any{"field": "Idempotency-Key", "account_id": firstAccountID})
}
