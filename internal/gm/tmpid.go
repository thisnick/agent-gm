package gm

import "github.com/google/uuid"

// GenerateTmpID mints the transaction ID for one send attempt.
//
// It is a bare UUID, not Agent GM's op_-prefixed operation ID (D22). Upstream
// generates it with util.GenerateTmpID() (util/func.go:9-12), whose comment is
// "Matches what the native app does", and the phone echoes the value back on
// the remote message. Sending a non-UUID transaction ID would diverge from
// every other client of this protocol for no benefit.
func GenerateTmpID() string { return uuid.NewString() }
