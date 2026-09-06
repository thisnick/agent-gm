package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/acceptance"
)

// The scanner has its own tests, because a scanner that silently found
// nothing would make section 16 Slice 2 test 41 pass on a server that was
// leaking every cookie it held. A check that cannot fail is not a check.
func TestScannerFindsWhatIsThereAndNothingThatIsNot(t *testing.T) {
	dir := t.TempDir()
	const secret = "FIXTURE-OSID-VALUE-NEVER-REAL"
	sentinels := []acceptance.Sentinel{
		{Name: "account A's OSID cookie", Value: secret},
		{Name: "a value nothing wrote", Value: "FIXTURE-ABSENT-VALUE"},
	}

	clean := filepath.Join(dir, "clean.bin")
	if err := os.WriteFile(clean, []byte("nothing to see"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := acceptance.ScanFiles(dir, sentinels); err != nil || len(found) != 0 {
		t.Fatalf("a clean directory reported %v (err %v)", found, err)
	}

	// The value is written inside binary noise, the way it would land in a
	// SQLite page or a WAL frame -- not as a tidy line a formatter would
	// print. That is the case the scan exists for.
	dirty := filepath.Join(dir, "dirty.sqlite3")
	body := append([]byte{0x00, 0x01, 0xff, 0xfe}, []byte(secret)...)
	body = append(body, 0x00, 0x7f)
	if err := os.WriteFile(dirty, body, 0o600); err != nil {
		t.Fatal(err)
	}

	found, err := acceptance.ScanFiles(dir, sentinels)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d sightings, want 1: %v", len(found), found)
	}
	if found[0].Sentinel.Name != "account A's OSID cookie" {
		t.Errorf("found %q", found[0].Sentinel.Name)
	}
	if !strings.Contains(found[0].Where, "dirty.sqlite3") {
		t.Errorf("the sighting does not name the file: %s", found[0].Where)
	}

	// A failure message must not be the second place the value leaks: CI
	// output is archived.
	if strings.Contains(found[0].String(), secret) {
		t.Errorf("the failure message repeats the sentinel: %s", found[0].String())
	}
	if !strings.Contains(found[0].Context, "REDACTED") {
		t.Errorf("the context does not mark the redaction: %s", found[0].Context)
	}
}

// An empty sentinel would match everything, so the scan would fail for no
// reason at all -- or, worse, a test would "prove" a leak that is not there
// and its next author would relax the check.
func TestAnEmptySentinelIsIgnored(t *testing.T) {
	got := acceptance.ScanText("anything at all", "somewhere",
		[]acceptance.Sentinel{{Name: "empty", Value: ""}})
	if len(got) != 0 {
		t.Errorf("an empty sentinel matched: %v", got)
	}
}

// A file that cannot be read is an error, not a skip. A scan that quietly
// skips is a scan that passes for the wrong reason.
func TestAnUnreadableFileIsAnErrorNotASkip(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read anything")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "unreadable")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	if _, err := acceptance.ScanFiles(dir, []acceptance.Sentinel{{Name: "x", Value: "y"}}); err == nil {
		t.Error("an unreadable file was skipped rather than reported")
	}
}

// The three files spec section 12.2 names by hand.
func TestDatabaseArtifactsNamesTheThree(t *testing.T) {
	got := acceptance.DatabaseArtifacts("/data")
	want := []string{
		"/data/agent-gm.sqlite3",
		"/data/agent-gm.sqlite3-wal",
		"/data/agent-gm.sqlite3-shm",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("DatabaseArtifacts = %v, want %v", got, want)
	}
}
