// Package settings is the runtime settings table of spec section 15.1: the
// keys that are mutable through `PATCH /v1/admin/settings` without an
// environment change and without a redeploy.
//
// Three things about this package are load-bearing.
//
//  1. **The registry is declarative.** One `Def` per key carries the default,
//     the bounds, the scope, the type, mutability and the restart
//     requirement. A key that is not declared does not exist; there is no
//     path that reads an undeclared key and no path that writes one.
//
//  2. **Scope is not decoration.** `ScopePerAccount` says the value is
//     applied to each account independently, so a two-account deployment
//     spends twice the budget. Recording it wrongly is how a deployment does
//     twice the work it was configured for, or half (section 15.1).
//
//  3. **Persistence is an interface.** This package never imports
//     internal/store; the store implements `Store` against the `settings`
//     table of section 4.2 and this package resolves precedence over it.
package settings

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind is a setting's type. It decides how a value is parsed from JSON, how
// it is compared against its bounds, and how it is rendered back out.
type Kind string

const (
	// KindInt is a whole number: counts, page sizes, byte budgets.
	KindInt Kind = "int"
	// KindDuration is a duration, carried in JSON as a string in Go's
	// duration syntax extended with `d` for days ("15m", "365d").
	KindDuration Kind = "duration"
	// KindBool is a boolean.
	KindBool Kind = "bool"
	// KindEnum is a string constrained to a declared set.
	KindEnum Kind = "enum"
)

// Value is one typed setting value. It is a struct rather than an `any` so
// that a bounds comparison cannot silently compare an int to a duration.
type Value struct {
	Kind Kind
	Int  int64
	Dur  time.Duration
	Bool bool
	Str  string
}

// Int64 builds an integer value.
func Int64(v int64) Value { return Value{Kind: KindInt, Int: v} }

// Duration builds a duration value.
func Duration(d time.Duration) Value { return Value{Kind: KindDuration, Dur: d} }

// Bool builds a boolean value.
func Bool(b bool) Value { return Value{Kind: KindBool, Bool: b} }

// Enum builds an enumerated string value.
func Enum(s string) Value { return Value{Kind: KindEnum, Str: s} }

// Day is the unit section 15.1 writes horizons and TTLs in.
const Day = 24 * time.Hour

// String renders a value the way section 15.1 writes it.
func (v Value) String() string {
	switch v.Kind {
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindDuration:
		return formatDuration(v.Dur)
	case KindBool:
		return strconv.FormatBool(v.Bool)
	case KindEnum:
		return v.Str
	}
	return ""
}

// Equal reports whether two values are the same type and the same value.
func (v Value) Equal(o Value) bool {
	return v.Kind == o.Kind && v.Int == o.Int && v.Dur == o.Dur && v.Bool == o.Bool && v.Str == o.Str
}

// compare orders two values of the same kind: -1, 0 or 1. Booleans and enums
// are unordered and always compare equal, which is why neither carries
// bounds.
func (v Value) compare(o Value) int {
	switch v.Kind {
	case KindInt:
		switch {
		case v.Int < o.Int:
			return -1
		case v.Int > o.Int:
			return 1
		}
	case KindDuration:
		switch {
		case v.Dur < o.Dur:
			return -1
		case v.Dur > o.Dur:
			return 1
		}
	}
	return 0
}

// MarshalJSON writes the wire form: a number for an int, a bool for a bool,
// a string for a duration or an enum.
func (v Value) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case KindInt:
		return json.Marshal(v.Int)
	case KindDuration:
		return json.Marshal(formatDuration(v.Dur))
	case KindBool:
		return json.Marshal(v.Bool)
	case KindEnum:
		return json.Marshal(v.Str)
	}
	return nil, fmt.Errorf("settings: cannot marshal value of unknown kind %q", v.Kind)
}

// JSON is MarshalJSON without the error, for the many places that hold a
// value already known to be well-typed.
func (v Value) JSON() string {
	b, err := v.MarshalJSON()
	if err != nil {
		return ""
	}
	return string(b)
}

// parseValue reads one JSON value as the given kind. It is deliberately
// strict: a duration is a string and never a bare number of seconds, because
// "300" is ambiguous between seconds and milliseconds and an operator who
// means five minutes should have to say so.
func parseValue(kind Kind, raw []byte) (Value, error) {
	switch kind {
	case KindInt:
		// json.Number rather than float64: 104857600 must not round-trip
		// through a float, and 1.5 must not become 1.
		// A quoted "4" is not a number. encoding/json will read a JSON
		// string into a json.Number (json.Number is a string type), so the
		// shape has to be checked before decoding or `"4"` becomes 4.
		if t := strings.TrimSpace(string(raw)); t == "" || (t[0] != '-' && (t[0] < '0' || t[0] > '9')) {
			return Value{}, fmt.Errorf("expected a whole number, got %s", raw)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		var n json.Number
		if err := dec.Decode(&n); err != nil {
			return Value{}, fmt.Errorf("expected a whole number, got %s", raw)
		}
		i, err := n.Int64()
		if err != nil {
			return Value{}, fmt.Errorf("expected a whole number, got %s", raw)
		}
		return Int64(i), nil
	case KindDuration:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return Value{}, fmt.Errorf(`expected a duration string such as "15m" or "365d", got %s`, raw)
		}
		d, err := ParseDuration(s)
		if err != nil {
			return Value{}, err
		}
		return Duration(d), nil
	case KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return Value{}, fmt.Errorf("expected true or false, got %s", raw)
		}
		return Bool(b), nil
	case KindEnum:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return Value{}, fmt.Errorf("expected a string, got %s", raw)
		}
		return Enum(s), nil
	}
	return Value{}, fmt.Errorf("unknown setting type %q", kind)
}

// ParseDuration is time.ParseDuration extended with a `d` (day) unit, because
// section 15.1 writes horizons and TTLs in days and `8760h` is not a value an
// operator should have to compute.
func ParseDuration(s string) (time.Duration, error) {
	orig := s
	if s == "" {
		return 0, fmt.Errorf(`expected a duration such as "15m" or "365d", got ""`)
	}
	var total time.Duration
	var neg bool
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	// Peel leading `<n>d` groups, then hand the tail to the standard parser.
	for {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(s) || s[i] != 'd' {
			break
		}
		n, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", orig)
		}
		total += time.Duration(n) * Day
		s = s[i+1:]
	}
	if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", orig)
		}
		total += d
	}
	if neg {
		total = -total
	}
	return total, nil
}

// formatDuration writes a duration the way section 15.1 does: whole days as
// `Nd`, and otherwise the largest whole unit that divides it exactly. It is
// unit-by-unit rather than a suffix trim on time.Duration.String() because
// trimming "0m" off "1h30m0s" turns ninety minutes into "1h3".
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	switch {
	case d%Day == 0:
		return strconv.FormatInt(int64(d/Day), 10) + "d"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	case d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}
