package apierr

// The idempotency key has exactly two transports (spec section 6.3): the
// `Idempotency-Key` header, or the `client_request_id` body field. There is
// no query-parameter form on any route, and one presented as a query
// parameter is an unknown parameter like any other (spec section 7.1).
//
// Every error here names `client_request_id` in `details.field`, whichever
// transport the value arrived by, because that is the name the caller reads
// in the tool description, in `agm --help` and in the API docs.

// ContradictoryIdempotencyKey is both transports carrying DIFFERENT values.
//
// It is refused rather than resolved by a precedence rule, because it is a
// contradiction and not a preference: a server that picked one would be
// guessing which of two things the caller meant, and the cost of guessing
// wrong is a second real text message to a real person.
func ContradictoryIdempotencyKey() *Error {
	return withDetails(CodeInvalidRequest,
		"the Idempotency-Key header and the client_request_id body field carry different values; "+
			"send one, or send the same value in both",
		map[string]any{"field": "client_request_id"})
}

// MissingIdempotencyKey is a mutation with no key at all. The key is required
// on every mutation (D7): retries are the normal case for an agent, and a
// duplicate SMS is not recoverable.
func MissingIdempotencyKey() *Error {
	return withDetails(CodeInvalidRequest,
		"every mutation needs an idempotency key: send the Idempotency-Key header "+
			"or the client_request_id body field. Repeating a call with the same value returns "+
			"the same operation and sends nothing further; a fresh value is a different call, "+
			"not a repeat",
		map[string]any{"field": "client_request_id"})
}

// BadIdempotencyKey is a key that is present but unusable. `why` completes
// the sentence "the idempotency key is refused because ...".
func BadIdempotencyKey(why string) *Error {
	return withDetails(CodeInvalidRequest,
		"the idempotency key is refused because "+why+"; nothing was done",
		map[string]any{"field": "client_request_id"})
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
func IdempotencyKeyUsedWithAnotherAccount(key, firstAccountID string) *Error {
	return withDetails(CodeInvalidRequest,
		"idempotency key "+key+" has already been used by this authorization for this kind "+
			"against account "+firstAccountID+"; sending it to another account would be a new "+
			"message to a different person, not a retry. Use a fresh key",
		map[string]any{"field": "client_request_id", "account_id": firstAccountID})
}
