package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// Output formats, spec section 11.1. `--json` is a synonym for
// `--output json`.
const (
	FormatTable = "table"
	FormatJSON  = "json"
	FormatJSONL = "jsonl"
)

// Output is the one place anything is written, and it exists to make spec
// section 11.3's promise structural rather than remembered:
//
//	"--json writes the server envelope plus CLI metadata as one value on
//	 stdout; diagnostics, progress, warnings and confirmation prompts go to
//	 stderr, so stdout stays machine-readable."
//
// Every diagnostic method here writes to stderr. There is exactly one method
// that writes to stdout -- Emit -- and it writes one value. That is what
// makes `agm ... --json | jq` safe in a pipeline even when the command has
// something to say (section 16 Slice 2 test 26).
type Output struct {
	stdout  io.Writer
	stderr  io.Writer
	format  string
	quiet   bool
	verbose bool

	// profile and server are the CLI metadata section 11.3 adds to the
	// envelope.
	profile string
	server  string
}

// cliEnvelope is the server envelope of spec section 7.1 plus the CLI
// metadata of section 11.3: the effective profile and the effective server.
type cliEnvelope struct {
	Data       json.RawMessage `json:"data"`
	NextCursor *string         `json:"next_cursor"`
	Warnings   []string        `json:"warnings"`
	RequestID  string          `json:"request_id"`
	Profile    string          `json:"profile"`
	Server     string          `json:"server"`
}

// JSON reports whether stdout carries JSON rather than a table.
func (o *Output) JSON() bool { return o.format == FormatJSON || o.format == FormatJSONL }

// Warnf writes a warning. Stderr, always: a warning that lands on stdout is a
// parse error for whatever is reading the result (spec section 11.3).
func (o *Output) Warnf(format string, args ...any) {
	_, _ = fmt.Fprintf(o.stderr, "agm: warning: "+format+"\n", args...)
}

// Infof writes progress. Stderr, and silenced by --quiet.
func (o *Output) Infof(format string, args ...any) {
	if o.quiet {
		return
	}
	_, _ = fmt.Fprintf(o.stderr, format+"\n", args...)
}

// Verbosef writes diagnostic detail. Stderr, and only with --verbose.
func (o *Output) Verbosef(format string, args ...any) {
	if !o.verbose {
		return
	}
	_, _ = fmt.Fprintf(o.stderr, "agm: "+format+"\n", args...)
}

// Errorf writes the final error line. Stderr.
func (o *Output) Errorf(format string, args ...any) {
	_, _ = fmt.Fprintf(o.stderr, "agm: "+format+"\n", args...)
}

// Emit writes the one result value. It is the only method that touches
// stdout.
func (o *Output) Emit(r *Response) error {
	// The server's warnings are diagnostics: they go to stderr as well as
	// into the JSON envelope, so a human sees them and a pipeline does not
	// have to.
	for _, w := range r.Warnings {
		o.Warnf("%s", w)
	}

	switch o.format {
	case FormatJSON:
		return o.emitJSON(r)
	case FormatJSONL:
		return o.emitJSONL(r)
	default:
		return o.emitTable(r)
	}
}

func (o *Output) emitJSON(r *Response) error {
	data := r.Data
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	warnings := r.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	env := cliEnvelope{
		Data:       data,
		NextCursor: r.NextCursor,
		Warnings:   warnings,
		RequestID:  r.RequestID,
		Profile:    o.profile,
		Server:     o.server,
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		return &ContractError{Msg: fmt.Sprintf("the result could not be encoded as JSON: %v", err)}
	}
	_, err = fmt.Fprintln(o.stdout, string(encoded))
	return err
}

// emitJSONL streams one object per line for a listing, which is what
// docs/cli.md promises `--output jsonl` does. A result that is not a listing
// is one object, so jsonl degenerates to json for it.
func (o *Output) emitJSONL(r *Response) error {
	items, ok := itemsOf(r.Data)
	if !ok {
		return o.emitOneLine(r.Data)
	}
	for _, item := range items {
		if err := o.emitOneLine(item); err != nil {
			return err
		}
	}
	return nil
}

func (o *Output) emitOneLine(raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return &ContractError{Msg: fmt.Sprintf("the result is not valid JSON: %v", err)}
	}
	_, err := fmt.Fprintln(o.stdout, compact.String())
	return err
}

// itemsOf pulls `data.items` out of a listing envelope. A listing puts its
// rows in data.items, never in data as a bare array (spec section 7.1).
func itemsOf(raw json.RawMessage) ([]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	items, ok := obj["items"]
	if !ok {
		return nil, false
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(items, &rows); err != nil {
		return nil, false
	}
	return rows, true
}

// emitTable is the human rendering. It prints the operation ID first for
// every mutation (spec section 11.3), so a transcript always contains the
// thing to pass to `agm operations wait`.
func (o *Output) emitTable(r *Response) error {
	var obj map[string]json.RawMessage
	if len(r.Data) == 0 || string(r.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(r.Data, &obj); err != nil {
		// Not an object: print it as it came.
		_, err := fmt.Fprintln(o.stdout, strings.TrimSpace(string(r.Data)))
		return err
	}

	// The operation ID, first, for every mutation.
	if id, ok := operationID(obj); ok {
		_, _ = fmt.Fprintln(o.stdout, id)
	}

	// The effect sentence, byte for byte as the route returned it. It is
	// never rebuilt locally: a human reading the transcript and a model
	// reading the tool description must see the same claim (section 11.3,
	// section 16 Slice 2 test 28).
	if effect, ok := stringField(obj, "effect"); ok {
		_, _ = fmt.Fprintln(o.stdout, effect)
	}

	if rows, ok := itemsOf(r.Data); ok {
		return o.emitRows(rows)
	}
	return o.emitFields(obj)
}

// operationID finds the operation ID of a mutation response: either the
// `operation` object a write returns inline (spec section 6.5) or, for
// `agm operations show`, the operation itself.
func operationID(obj map[string]json.RawMessage) (string, bool) {
	if raw, ok := obj["operation"]; ok {
		var op map[string]json.RawMessage
		if err := json.Unmarshal(raw, &op); err == nil {
			if id, ok := stringField(op, "id"); ok {
				return id, true
			}
		}
	}
	if id, ok := stringField(obj, "id"); ok && strings.HasPrefix(id, "op_") {
		return id, true
	}
	return "", false
}

func stringField(obj map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := obj[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// columnOrder puts the columns a human scans for first. Anything not named
// here follows in sorted order, so a new field appears rather than hiding.
var columnOrder = []string{
	"id", "account_id", "conversation_id", "message_id", "attachment_id",
	"operation_id", "google_account", "label", "name", "type", "kind",
	"state", "status", "direction", "delivery_state", "unread", "text",
	"filename", "mime_type", "size", "sent_at", "last_activity",
}

func orderColumns(keys []string) []string {
	rank := map[string]int{}
	for i, k := range columnOrder {
		rank[k] = i
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, oki := rank[keys[i]]
		rj, okj := rank[keys[j]]
		switch {
		case oki && okj:
			return ri < rj
		case oki:
			return true
		case okj:
			return false
		default:
			return keys[i] < keys[j]
		}
	})
	return keys
}

func (o *Output) emitRows(rows []json.RawMessage) error {
	if len(rows) == 0 {
		return nil
	}
	// Columns are the union of every row's scalar keys, so a row with a
	// field the first one lacked is not silently dropped.
	seen := map[string]bool{}
	var keys []string
	decoded := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var m map[string]any
		if err := json.Unmarshal(row, &m); err != nil {
			continue
		}
		decoded = append(decoded, m)
		for k, v := range m {
			if !isScalar(v) || seen[k] {
				continue
			}
			seen[k] = true
			keys = append(keys, k)
		}
	}
	keys = orderColumns(keys)

	tw := tabwriter.NewWriter(o.stdout, 0, 0, 2, ' ', 0)
	header := make([]string, len(keys))
	for i, k := range keys {
		header[i] = strings.ToUpper(k)
	}
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, m := range decoded {
		cells := make([]string, len(keys))
		for i, k := range keys {
			cells[i] = scalarString(m[k])
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

func (o *Output) emitFields(obj map[string]json.RawMessage) error {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if k == "effect" {
			continue // already printed, first
		}
		keys = append(keys, k)
	}
	keys = orderColumns(keys)

	tw := tabwriter.NewWriter(o.stdout, 0, 0, 2, ' ', 0)
	for _, k := range keys {
		var v any
		if err := json.Unmarshal(obj[k], &v); err != nil {
			continue
		}
		if isScalar(v) {
			_, _ = fmt.Fprintf(tw, "%s:\t%s\n", k, scalarString(v))
			continue
		}
		// A nested object or array is printed as compact JSON rather than
		// dropped: a table that hides a field is worse than a busy one.
		compact, err := json.Marshal(v)
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s:\t%s\n", k, string(compact))
	}
	return tw.Flush()
}

func isScalar(v any) bool {
	switch v.(type) {
	case nil, bool, float64, string:
		return true
	default:
		return false
	}
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return "-"
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case string:
		if t == "" {
			return "-"
		}
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}
