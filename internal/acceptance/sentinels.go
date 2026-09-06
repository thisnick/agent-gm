// Package acceptance holds the Slice 2 acceptance tests that need a whole
// server rather than one package: the sentinel-secret scan of spec section
// 12.2, and the end-to-end walks of section 16 that cross every layer.
//
// Nothing here needs a phone, Docker or a network, so nothing here is behind
// a gate. Parking a test behind a gate it does not need is how a clause stays
// unverified for a phase (spec section 13.2).
package acceptance

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sentinel is one value that must never appear at rest or in a log.
type Sentinel struct {
	// Name is what a failure calls it: "acct A's OSID cookie".
	Name string
	// Value is the value itself. It is a fixture, never a real secret.
	Value string
}

// Sighting is one place a sentinel was found.
type Sighting struct {
	Sentinel Sentinel
	Where    string
	// Context is a short excerpt with the sentinel itself replaced, so a
	// failure message can be read without leaking the value a second time
	// into a test log that may itself be captured.
	Context string
}

func (s Sighting) String() string {
	return fmt.Sprintf("%s appears in %s: %s", s.Sentinel.Name, s.Where, s.Context)
}

// ScanFiles walks root and reports every sentinel found in any file under it.
//
// It reads whole files as bytes rather than as text, deliberately. The
// acceptance test of spec section 12.2 is a `strings | grep`: a value written
// into a SQLite page, a WAL frame or a JSON blob is still a disclosure even
// though nothing would print it, and a scan that only looked at what a
// formatter would emit would miss exactly the cases that matter.
//
// A file it cannot read is reported as an error rather than skipped: a scan
// that quietly skips is a scan that passes for the wrong reason.
func ScanFiles(root string, sentinels []Sentinel) ([]Sighting, error) {
	var out []Sighting
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		out = append(out, scan(string(data), path, sentinels)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Where < out[j].Where })
	return out, nil
}

// ScanText reports every sentinel in one body of text -- a captured log, an
// audit payload, an error message.
func ScanText(text, where string, sentinels []Sentinel) []Sighting {
	return scan(text, where, sentinels)
}

func scan(haystack, where string, sentinels []Sentinel) []Sighting {
	var out []Sighting
	for _, s := range sentinels {
		if s.Value == "" {
			// An empty sentinel matches everything, which would make the
			// scan pass or fail for no reason at all.
			continue
		}
		i := strings.Index(haystack, s.Value)
		if i < 0 {
			continue
		}
		out = append(out, Sighting{
			Sentinel: s,
			Where:    where,
			Context:  redactedContext(haystack, i, len(s.Value)),
		})
	}
	return out
}

// redactedContext quotes the neighbourhood of a sighting with the sentinel
// itself blanked out. A failure message has to be readable in CI output that
// may be archived, so it must not be the second place the value leaks.
func redactedContext(haystack string, at, length int) string {
	const window = 24
	start := at - window
	if start < 0 {
		start = 0
	}
	end := at + length + window
	if end > len(haystack) {
		end = len(haystack)
	}
	before := printable(haystack[start:at])
	after := printable(haystack[at+length : end])
	return "..." + before + "<<REDACTED SENTINEL>>" + after + "..."
}

// printable keeps a byte run readable when it came out of a SQLite page.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteByte('.')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// DatabaseArtifacts are the three files spec section 12.2 names by hand: the
// database, its write-ahead log and its shared-memory index. They are listed
// explicitly as well as walked, because a `-wal` that has been checkpointed
// away between the write and the scan would make the scan pass for the wrong
// reason -- so a test asserts these exist before trusting a clean result.
func DatabaseArtifacts(dataDir string) []string {
	db := filepath.Join(dataDir, "agent-gm.sqlite3")
	return []string{db, db + "-wal", db + "-shm"}
}
