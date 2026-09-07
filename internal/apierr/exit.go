package apierr

// The CLI exit codes of spec section 11.2. They are exported from here rather
// than kept in internal/cli so that the CLI, a script and a test all read one
// table; a second copy is how `agm` and the API come to disagree about what
// exit 7 means.
//
// 1 is deliberately unassigned: the Go runtime and the shell both use it for
// "something went wrong", so reserving it keeps "the process died" clearly
// distinct from "Agent GM decided something".
const (
	// ExitOK: success.
	ExitOK = 0
	// ExitUsage: CLI usage or validation error. idempotency_conflict lands
	// here because reusing a key with a different body is a caller mistake.
	ExitUsage = 2
	// ExitAuthRequired: authentication required or expired credentials.
	ExitAuthRequired = 3
	// ExitForbidden: authorization or insufficient scope.
	ExitForbidden = 4
	// ExitNotFound: requested resource absent.
	ExitNotFound = 5
	// ExitUnsupported: unsupported capability for this account, conversation
	// or message. not_signed_in is an account-level condition and lands here,
	// which is why the spec's gloss names the account.
	ExitUnsupported = 6
	// ExitRetryable: retryable network, Google, phone, or rate-limit failure.
	// phone_not_responding is here and NOT ExitOperationFailed: the operation
	// is pending, and a script that treats it as failed resends the message.
	ExitRetryable = 7
	// ExitOperationFailed: an operation reached a terminal failure. It is
	// produced by waiting on an operation, not by an error code, so no row of
	// section 7.2 maps to it.
	ExitOperationFailed = 8
	// ExitLocalConfig: local configuration or credential-store failure. It is
	// decided before a request is ever sent, so no row of section 7.2 maps
	// to it either.
	ExitLocalConfig = 9
	// ExitServerFailure: server contract or internal failure.
	ExitServerFailure = 10
)

// exitCodes is spec section 11.2's mapping table. Every code of section 7.2
// appears exactly once; exit_test.go proves that in both directions, so a
// code added to section 7.2 without an exit code fails the build's tests
// rather than silently exiting 10.
var exitCodes = map[Code]int{
	CodeInvalidRequest:       ExitUsage,
	CodeIdempotencyConflict:  ExitUsage,
	CodePayloadTooLarge:      ExitUsage,
	CodeMediaUnsupportedType: ExitUsage,

	CodeInvalidToken: ExitAuthRequired,

	CodeInsufficientScope: ExitForbidden,

	CodeNotFound: ExitNotFound,

	CodeUnsupportedCapability: ExitUnsupported,

	CodeRateLimited:        ExitRetryable,
	CodeDisconnected:       ExitRetryable,
	CodePhoneNotResponding: ExitRetryable,
	CodeGoogleHTTPError:    ExitRetryable,
	CodePairingInitTimeout: ExitRetryable,

	CodeNotPaired:                ExitServerFailure,
	CodePairingNoCookies:         ExitServerFailure,
	CodePairingNoDevices:         ExitServerFailure,
	CodePairingWrongEmoji:        ExitServerFailure,
	CodePairingCancelled:         ExitServerFailure,
	CodePairingTimeout:           ExitServerFailure,
	CodePairingWrongAccount:      ExitServerFailure,
	CodePairingNoAccount:         ExitServerFailure,
	CodeNotDefaultSMSApp:         ExitServerFailure,
	CodeGoogleError:              ExitServerFailure,
	CodeGoogleUndocumentedStatus: ExitServerFailure,
	CodeGooglePermissionDenied:   ExitServerFailure,
	CodeInternalError:            ExitServerFailure,
}

// ExitCodeFor is the exit code spec section 11.2 assigns to c. A code outside
// the vocabulary exits 10: an answer Agent GM does not recognise is a server
// contract failure, which is exactly what 10 means.
func ExitCodeFor(c Code) int {
	if code, ok := exitCodes[c]; ok {
		return code
	}
	return ExitServerFailure
}

// ExitCodes returns a copy of the whole mapping, for the CLI's documentation
// and for tests that enumerate it.
func ExitCodes() map[Code]int {
	out := make(map[Code]int, len(exitCodes))
	for k, v := range exitCodes {
		out[k] = v
	}
	return out
}
