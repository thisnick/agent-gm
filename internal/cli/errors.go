package cli

import (
	"errors"
	"fmt"

	"github.com/thisnick/agent-gm/internal/apierr"
)

// The CLI's own failures, the ones spec section 11.2 assigns an exit code to
// that no section 7.2 error code produces. They are types rather than
// sentinel values because each carries the thing the operator needs next: the
// flag that was wrong, the path that was not writable, the operation ID that
// is still waitable.
//
// Every exit code that CAN come from a server error comes from
// apierr.ExitCodeFor and from nowhere else (section 11.2 is one table, and a
// second copy here is how `agm` and the server come to disagree about what
// exit 7 means).

// UsageError is exit 2: a usage or validation error decided locally. Flag
// names the flag at fault when there is one, because "invalid duration" with
// no flag name is a worse message than no message.
type UsageError struct {
	Flag string
	Msg  string
	Err  error
}

func (e *UsageError) Error() string {
	switch {
	case e.Flag != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Flag, e.Msg, e.Err)
	case e.Flag != "":
		return fmt.Sprintf("%s: %s", e.Flag, e.Msg)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	default:
		return e.Msg
	}
}

func (e *UsageError) Unwrap() error { return e.Err }

func usageErr(format string, args ...any) *UsageError {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

func flagErr(flag, format string, args ...any) *UsageError {
	return &UsageError{Flag: flag, Msg: fmt.Sprintf(format, args...)}
}

// LocalError is exit 9: a local configuration or credential-store failure.
// It is decided BEFORE a request is sent -- that ordering is the whole point
// of section 11.5's write-back safety rule -- so a LocalError always means
// nothing was spent.
type LocalError struct {
	Msg string
	Err error
}

func (e *LocalError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	return e.Msg
}

func (e *LocalError) Unwrap() error { return e.Err }

func localErr(err error, format string, args ...any) *LocalError {
	return &LocalError{Msg: fmt.Sprintf(format, args...), Err: err}
}

// OperationFailedError is exit 8: an operation reached `failed` or `unknown`
// while `agm` was waiting on it. It is deliberately NOT what
// phone_not_responding produces -- that is exit 7 and the operation is still
// pending (section 11.2).
type OperationFailedError struct {
	OperationID string
	Status      string
	Detail      string
}

func (e *OperationFailedError) Error() string {
	msg := fmt.Sprintf("operation %s reached %s", e.OperationID, e.Status)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// WaitTimeoutError is exit 7: the wait ran out of time. The operation is not
// failed, so the message names it and says where to pick it up again.
type WaitTimeoutError struct {
	OperationID string
	WaitFor     string
}

func (e *WaitTimeoutError) Error() string {
	if e.OperationID == "" {
		return fmt.Sprintf("timed out waiting for %s", e.WaitFor)
	}
	return fmt.Sprintf(
		"timed out waiting for %s; operation %s is still available to `agm operations wait %s`",
		e.WaitFor, e.OperationID, e.OperationID)
}

// TransportError is a request that never produced an envelope: a refused
// connection, a DNS failure, a read that timed out. Section 11.2 calls exit 7
// "retryable network, Google, phone, or rate-limit failure", and a network
// that was not there is the first of those.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string { return fmt.Sprintf("%s: %v", e.Op, e.Err) }

func (e *TransportError) Unwrap() error { return e.Err }

// ContractError is exit 10: the server answered something that is not the
// contract -- a body that is not an envelope, a status with no error object.
type ContractError struct {
	Msg string
}

func (e *ContractError) Error() string { return e.Msg }

// ErrAborted is a destructive command the operator declined. It exits 2:
// answering "no" to "Continue? [y/N]" is not a failure of Agent GM, it is the
// prompt doing its job, and 0 would tell a script the deletion happened.
var ErrAborted = errors.New("aborted; nothing was done")

// ExitCodeFor maps any error this package produces onto spec section 11.2's
// exit code. Server error codes go through apierr.ExitCodeFor, which is the
// one table; the cases below are the CLI-only codes that no section 7.2 code
// produces.
func ExitCodeFor(err error) int {
	if err == nil {
		return apierr.ExitOK
	}

	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		return apierr.ExitCodeFor(apiErr.Code)
	}

	var usage *UsageError
	if errors.As(err, &usage) {
		return apierr.ExitUsage
	}
	if errors.Is(err, ErrAborted) {
		return apierr.ExitUsage
	}
	var local *LocalError
	if errors.As(err, &local) {
		return apierr.ExitLocalConfig
	}
	var opFailed *OperationFailedError
	if errors.As(err, &opFailed) {
		return apierr.ExitOperationFailed
	}
	var waitTimeout *WaitTimeoutError
	if errors.As(err, &waitTimeout) {
		return apierr.ExitRetryable
	}
	var transport *TransportError
	if errors.As(err, &transport) {
		return apierr.ExitRetryable
	}
	return apierr.ExitServerFailure
}
