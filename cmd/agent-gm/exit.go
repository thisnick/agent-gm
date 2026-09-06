package main

import (
	"errors"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Exit codes, spec section 11.2. `1` is deliberately unassigned.
const (
	exitOK                    = 0
	exitUsage                 = 2
	exitAuthRequired          = 3
	exitForbidden             = 4
	exitNotFound              = 5
	exitUnsupportedCapability = 6
	exitRetryable             = 7
	exitOperationFailed       = 8
	exitLocalConfig           = 9
	exitContract              = 10
)

// exitCodeFor maps a section 7.2 code onto its exit code. The mapping is
// exhaustive: every code in section 7.2 maps onto exactly one exit code.
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	var ge *gm.Error
	if !errors.As(err, &ge) {
		return exitContract
	}
	switch ge.Code {
	case gm.CodeUnsupportedCapability:
		// Including not_signed_in, which is an account-level condition.
		return exitUnsupportedCapability
	case gm.CodeDisconnected, gm.CodePhoneNotResponding,
		gm.CodeGoogleHTTPError, gm.CodePairingInitTimeout:
		return exitRetryable
	case gm.CodePairingNoCookies, gm.CodePairingNoDevices, gm.CodePairingWrongEmoji,
		gm.CodePairingCancelled, gm.CodePairingTimeout, gm.CodePairingNoAccount,
		gm.CodePairingWrongAccount, gm.CodeNotDefaultSMSApp, gm.CodeConfigVersionStale,
		gm.CodeGoogleError, gm.CodeGoogleUndocumentedState, gm.CodeGooglePermissionDenied,
		gm.CodeInternalError:
		return exitContract
	default:
		return exitContract
	}
}
