package mcp_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The MCP Go SDK pin (decision D36), and the guard that keeps it.
//
// D36 makes the pin "a fact CI keeps, like the libgm one" and says a bump is a
// deliberate slice, never a drive-by commit. `scripts/pin-consistency.sh`
// enforces it. These two tests are what stop the enforcement being removed in
// the same drive-by commit that does the bumping: the reviewer deleted the
// whole `go-sdk` block from the script and both the suite and the
// `pin-consistency` CI job stayed green, so the one mechanism behind D36 could
// be taken out without anything noticing.

const sdkModulePath = "github.com/modelcontextprotocol/go-sdk"

// TestSDKPinAgreesWithTheSpec is the claim itself, as an ordinary test, so
// that a developer who edits `go.mod` alone fails `devbox run test` and not
// only a CI job they may not have run.
func TestSDKPinAgreesWithTheSpec(t *testing.T) {
	fromMod := sdkVersionFromGoMod(t, "../../go.mod")
	fromSpec := sdkVersionFromSpec(t, "../../plans/AGENT_GM_SPEC.md")
	if fromMod != fromSpec {
		t.Fatalf("go.mod pins %s at %s and spec decision D36 states %s.\n"+
			"Bumping the SDK is a deliberate slice (D36), never a drive-by: both move together.",
			sdkModulePath, fromMod, fromSpec)
	}
}

// TestThePinGuardIsLoadBearing runs the real `scripts/pin-consistency.sh` over
// a copy of this tree whose SDK version has been desynchronised, and requires
// it to FAIL.
//
// A guard nothing exercises is a guard that can be deleted invisibly, which is
// exactly what the reviewer demonstrated. This is the same shape as
// `internal/lint`'s meta-test: it is not a test of the pin, it is a test of
// the thing that checks the pin.
func TestThePinGuardIsLoadBearing(t *testing.T) {
	root := t.TempDir()
	// The script reads exactly these, and cds to the directory above
	// `scripts/`, so the copy only needs the layout it walks.
	for _, rel := range []string{
		"go.mod",
		"plans/AGENT_GM_SPEC.md",
		"internal/gm/pin.go",
		"scripts/pin-consistency.sh",
	} {
		copyInto(t, root, rel)
	}

	script := filepath.Join(root, "scripts", "pin-consistency.sh")

	// Unmodified, the guard passes. Asserting this first means a failure
	// below is the desynchronisation and not a broken copy.
	if out, err := exec.Command("bash", script).CombinedOutput(); err != nil {
		t.Fatalf("pin-consistency failed on an unmodified tree: %v\n%s", err, out)
	}

	// Now move the SDK version in go.mod alone, which is what a drive-by
	// `go get -u` does.
	goModPath := filepath.Join(root, "go.mod")
	before, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("reading the copied go.mod: %v", err)
	}
	current := sdkVersionFromGoMod(t, goModPath)
	after := strings.Replace(string(before), sdkModulePath+" "+current, sdkModulePath+" v0.0.1", 1)
	if after == string(before) {
		t.Fatalf("could not desynchronise %s in the copied go.mod", sdkModulePath)
	}
	if err := os.WriteFile(goModPath, []byte(after), 0o600); err != nil {
		t.Fatalf("writing the copied go.mod: %v", err)
	}

	out, err := exec.Command("bash", script).CombinedOutput()
	if err == nil {
		t.Fatalf("pin-consistency exited 0 with %s desynchronised between go.mod and spec D36.\n"+
			"Either the guard was removed or it stopped working; D36's promise that a bump is a "+
			"deliberate slice rests entirely on it.\n%s", sdkModulePath, out)
	}
	if !strings.Contains(string(out), sdkModulePath) {
		t.Errorf("pin-consistency failed but did not name %s, so a reader cannot tell WHICH "+
			"pin drifted:\n%s", sdkModulePath, out)
	}
}

func copyInto(t *testing.T, root, rel string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("../..", rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	dst := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, body, 0o700); err != nil {
		t.Fatalf("writing %s: %v", dst, err)
	}
}

var sdkModRe = regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(sdkModulePath) + `\s+(v[^\s]+)\s*$`)

func sdkVersionFromGoMod(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	m := sdkModRe.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("%s does not require %s", path, sdkModulePath)
	}
	return m[1]
}

var sdkSpecRe = regexp.MustCompile(regexp.QuoteMeta(sdkModulePath) + "`, pinned at `(v[^`]+)`")

func sdkVersionFromSpec(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	m := sdkSpecRe.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("spec decision D36 does not state a %s version", sdkModulePath)
	}
	return m[1]
}
