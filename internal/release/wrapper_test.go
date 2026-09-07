// Package release_test holds the Slice 4 acceptance tests for the npm wrapper
// of spec section 14.3 and the release artefacts of section 16.
//
// Nothing here needs a phone, Docker, a network or a real release page, so
// nothing here is behind a gate. Parking a test behind a gate it does not
// need is how a clause stays unverified for a slice (section 13.2). The whole
// download path is exercised over `file://` against archives this test builds,
// which is the same code path an `https://` install takes right up to the
// bytes.
package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// --- fixtures -----------------------------------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	// A tree with no git (a vendored export) still has the layout.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func requireNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		// Devbox supplies node 22. A machine without it cannot run the
		// wrapper at all, so skipping is honest rather than convenient --
		// but it is announced, so a CI run that quietly stopped testing the
		// wrapper is visible in the log.
		t.Skipf("node is not on PATH, so the npm wrapper cannot be exercised here: %v", err)
	}
	return node
}

var (
	agmOnce sync.Once
	agmPath string
	agmErr  error
)

// buildAgm builds the real `agm` for this host once per test binary. The
// wrapper's whole job is to run this program and report its exit code, so a
// stub would test the stub.
func buildAgm(t *testing.T) string {
	t.Helper()
	agmOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agm-build-")
		if err != nil {
			agmErr = err
			return
		}
		out := filepath.Join(dir, "agm")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/agm")
		cmd.Dir = repoRoot(t)
		if b, err := cmd.CombinedOutput(); err != nil {
			agmErr = fmt.Errorf("go build ./cmd/agm: %v\n%s", err, b)
			return
		}
		agmPath = out
	})
	if agmErr != nil {
		t.Fatalf("building agm: %v", agmErr)
	}
	return agmPath
}

// stagePackage copies npm/ into a temporary directory at the given version
// and writes the checksums.txt that scripts/release.sh pins at publish time.
// It is the layout an installed package has, minus node_modules, which the
// wrapper does not use because it has no dependencies.
func stagePackage(t *testing.T, version string, checksums map[string]string) string {
	t.Helper()
	root := repoRoot(t)
	dst := t.TempDir()

	for _, rel := range []string{"bin/agm.js", "scripts/postinstall.js", "scripts/platform.js", "package.json"} {
		src := filepath.Join(root, "npm", rel)
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading %s: %v", src, err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The version in the staged package.json is the version the postinstall
	// will look for on the release page, exactly as `npm pack` stamps it.
	pj := filepath.Join(dst, "package.json")
	b, err := os.ReadFile(pj)
	if err != nil {
		t.Fatal(err)
	}
	var pkg map[string]any
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatalf("npm/package.json is not JSON: %v", err)
	}
	pkg["version"] = version
	b, _ = json.MarshalIndent(pkg, "", "  ")
	if err := os.WriteFile(pj, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if checksums != nil {
		var sb strings.Builder
		for name, sum := range checksums {
			fmt.Fprintf(&sb, "%s  %s\n", sum, name)
		}
		if err := os.WriteFile(filepath.Join(dst, "checksums.txt"), []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// makeArchive writes a gzipped tar holding one executable named `agm`, the
// way a release archive does, and returns its path and its SHA-256.
func makeArchive(t *testing.T, dir, name string, binary []byte) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "agm", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return path, hex.EncodeToString(sum[:])
}

// assetName is what the postinstall will ask for on this host.
func assetName(t *testing.T, version string) string {
	t.Helper()
	goarch := runtime.GOARCH
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("this test runs the real postinstall, which by design refuses %s", runtime.GOOS)
	}
	return fmt.Sprintf("agm_%s_%s_%s.tar.gz", version, runtime.GOOS, goarch)
}

type nodeResult struct {
	code   int
	stdout string
	stderr string
}

func runNode(t *testing.T, dir, script string, env []string, args ...string) nodeResult {
	t.Helper()
	node := requireNode(t)
	cmd := exec.Command(node, append([]string{script}, args...)...)
	cmd.Dir = dir
	// A clean-ish environment: PATH for tar, HOME for node's own bookkeeping,
	// and nothing inherited that could make an assertion pass by accident.
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}, env...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running %s: %v", script, err)
		}
	}
	return nodeResult{code: code, stdout: out.String(), stderr: errb.String()}
}

// --- the package itself ---------------------------------------------------------

// Section 14.3 states four things about package.json by name. They are cheap
// to get wrong in a hand-edit and expensive to notice: a missing `bin` ships a
// package that installs nothing, and a wrong licence on a package that
// distributes AGPL binaries is a licence violation rather than a typo
// (section 1.4).
func TestPackageManifestSaysWhatSection143Requires(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "npm", "package.json"))
	if err != nil {
		t.Fatalf("npm/package.json: %v", err)
	}
	var pkg struct {
		Name    string            `json:"name"`
		License string            `json:"license"`
		Bin     map[string]string `json:"bin"`
		Files   []string          `json:"files"`
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatalf("npm/package.json is not JSON: %v", err)
	}
	if pkg.Name != "@agent-gm/cli" {
		t.Errorf(`name = %q, want "@agent-gm/cli"`, pkg.Name)
	}
	if pkg.License != "AGPL-3.0-or-later" {
		t.Errorf(`license = %q, want "AGPL-3.0-or-later"; this package distributes AGPL binaries (section 1.4)`, pkg.License)
	}
	if pkg.Bin["agm"] != "bin/agm.js" {
		t.Errorf(`bin.agm = %q, want "bin/agm.js"`, pkg.Bin["agm"])
	}
	if pkg.Scripts["postinstall"] == "" {
		t.Error("there is no postinstall script, so nothing would ever download a binary")
	}
	// checksums.txt is written into the staging directory at publish time and
	// is not in the repository, so `files` is the only thing that decides
	// whether the pin of section 14.3 reaches the tarball at all.
	var hasChecksums, hasLicense bool
	for _, f := range pkg.Files {
		switch f {
		case "checksums.txt":
			hasChecksums = true
		case "LICENSE":
			hasLicense = true
		}
	}
	if !hasChecksums {
		t.Error(`"checksums.txt" is not in package.json "files"; the pinned checksums would not reach the published tarball and the postinstall would have nothing to verify against`)
	}
	if !hasLicense {
		t.Error(`"LICENSE" is not in package.json "files"`)
	}
}

// --- acceptance test 3: an unsupported platform ---------------------------------

// A wrapper that installed on Windows and then failed at run time would have
// converted a clear install error into a mysterious one, on the machine
// furthest from the person who could fix it. The message must NAME the
// platform: "unsupported platform" alone tells the reader nothing they can act
// on, and this repository has no Windows archive to point them at.
func TestUnsupportedPlatformIsRefusedByNameBeforeAnythingIsInstalled(t *testing.T) {
	pkg := stagePackage(t, "1.0.0", map[string]string{"agm_1.0.0_linux_amd64.tar.gz": strings.Repeat("0", 64)})

	// The map is asked directly for platforms this runner is not, so the
	// refusal is proved for every one of them rather than only for whatever
	// the CI runner happens to be.
	for _, tc := range []struct{ platform, arch string }{
		{"win32", "x64"},
		{"win32", "arm64"},
		{"linux", "ia32"},
		{"freebsd", "x64"},
		{"darwin", "ppc"},
	} {
		t.Run(tc.platform+"/"+tc.arch, func(t *testing.T) {
			script := filepath.Join(pkg, "check.js")
			body := fmt.Sprintf(`
const { target, UnsupportedPlatformError } = require("./scripts/platform.js");
try {
  target(%q, %q, "1.0.0");
  console.log("NO ERROR");
  process.exit(0);
} catch (e) {
  process.stderr.write((e instanceof UnsupportedPlatformError ? "TYPED " : "UNTYPED ") + e.message);
  process.exit(1);
}
`, tc.platform, tc.arch)
			if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			res := runNode(t, pkg, script, nil)
			if res.code == 0 {
				t.Fatalf("%s/%s was accepted; the wrapper ships no binary for it", tc.platform, tc.arch)
			}
			if !strings.HasPrefix(res.stderr, "TYPED ") {
				t.Errorf("the refusal is not an UnsupportedPlatformError, so the postinstall cannot label it: %s", res.stderr)
			}
			if !strings.Contains(res.stderr, tc.platform) || !strings.Contains(res.stderr, tc.arch) {
				t.Errorf("the message does not name %s/%s:\n%s", tc.platform, tc.arch, res.stderr)
			}
			if !strings.Contains(res.stderr, "github.com/thisnick/agent-gm/releases") {
				t.Errorf("the message does not point at the release page:\n%s", res.stderr)
			}
		})
	}
}

// The same refusal, through the actual postinstall entry point rather than
// through the module it calls, so that a postinstall which caught the error
// and carried on regardless would be caught. It runs the script with a forged
// process.platform, because a CI runner cannot be Windows on demand.
func TestPostinstallFailsOnAnUnsupportedPlatform(t *testing.T) {
	pkg := stagePackage(t, "1.0.0", map[string]string{"agm_1.0.0_linux_amd64.tar.gz": strings.Repeat("0", 64)})
	shim := filepath.Join(pkg, "as-windows.js")
	body := `
Object.defineProperty(process, "platform", { value: "win32" });
Object.defineProperty(process, "arch", { value: "x64" });
require("./scripts/postinstall.js");
`
	if err := os.WriteFile(shim, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runNode(t, pkg, shim, nil)
	if res.code == 0 {
		t.Fatalf("postinstall succeeded on win32/x64; it installed something that cannot run\nstdout: %s", res.stdout)
	}
	if !strings.Contains(res.stderr, "win32") || !strings.Contains(res.stderr, "x64") {
		t.Errorf("postinstall's failure does not name the platform:\n%s", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(pkg, "vendor", "agm")); !os.IsNotExist(err) {
		t.Error("postinstall left a vendor/agm behind on a platform it refused")
	}
}

// --- acceptance test 2: the checksum, and the two escape hatches -----------------

// The happy path. It is here so that the tamper test below is known to be
// failing for the right reason: a verification step that rejected everything
// would pass the tamper test and be worthless.
func TestPostinstallInstallsAnArchiveThatMatchesThePinnedChecksum(t *testing.T) {
	requireNode(t)
	const version = "1.2.3"
	asset := assetName(t, version)
	serveDir := t.TempDir()
	_, sum := makeArchive(t, serveDir, asset, []byte("#!/bin/sh\necho fixture agm\n"))

	pkg := stagePackage(t, version, map[string]string{asset: sum})
	res := runNode(t, pkg, filepath.Join(pkg, "scripts", "postinstall.js"),
		[]string{"AGENT_GM_CLI_BASE_URL=file://" + serveDir})
	if res.code != 0 {
		t.Fatalf("postinstall exited %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "verified "+asset) {
		t.Errorf("postinstall does not report verifying the asset:\n%s", res.stdout)
	}
	info, err := os.Stat(filepath.Join(pkg, "vendor", "agm"))
	if err != nil {
		t.Fatalf("vendor/agm was not installed: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("vendor/agm is mode %v, want it executable (chmod 0755)", info.Mode().Perm())
	}
}

// Acceptance test 2. The checksums.txt inside the package is what a download
// is judged against -- NOT one fetched beside the archive. A release page
// that is compromised after publication can serve a different binary and a
// matching checksums.txt; it cannot change what is already inside a published
// npm tarball. This test simulates exactly that: the served archive and the
// served checksums agree with each other and disagree with the pin.
func TestATamperedDownloadFailsInstallNamingTheFile(t *testing.T) {
	requireNode(t)
	const version = "1.2.3"
	asset := assetName(t, version)
	serveDir := t.TempDir()

	// What the package was published against.
	_, goodSum := makeArchive(t, serveDir, "reference.tar.gz", []byte("the release binary"))
	// What the release page serves today, with its own consistent checksum.
	_, badSum := makeArchive(t, serveDir, asset, []byte("a different binary entirely"))
	if goodSum == badSum {
		t.Fatal("the fixture archives are identical; the test would prove nothing")
	}
	if err := os.WriteFile(filepath.Join(serveDir, "checksums.txt"),
		[]byte(badSum+"  "+asset+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkg := stagePackage(t, version, map[string]string{asset: goodSum})
	res := runNode(t, pkg, filepath.Join(pkg, "scripts", "postinstall.js"),
		[]string{"AGENT_GM_CLI_BASE_URL=file://" + serveDir})
	if res.code == 0 {
		t.Fatalf("postinstall accepted an archive that does not match the pinned checksum\nstdout: %s", res.stdout)
	}
	if !strings.Contains(res.stderr, asset) {
		t.Errorf("the failure does not name the file:\n%s", res.stderr)
	}
	if !strings.Contains(res.stderr, goodSum) || !strings.Contains(res.stderr, badSum) {
		t.Errorf("the failure reports neither the expected nor the actual digest, so nobody can tell which side moved:\n%s", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(pkg, "vendor", "agm")); !os.IsNotExist(err) {
		t.Error("postinstall installed the binary anyway after the checksum failed")
	}
}

// A package with no checksums.txt must refuse rather than install unverified
// bytes. Without this, dropping the file from `files` would silently turn
// verification off and every test above would still pass.
func TestPostinstallRefusesWhenThePinnedChecksumsAreMissing(t *testing.T) {
	requireNode(t)
	const version = "1.2.3"
	asset := assetName(t, version)
	serveDir := t.TempDir()
	makeArchive(t, serveDir, asset, []byte("anything at all"))

	pkg := stagePackage(t, version, nil)
	res := runNode(t, pkg, filepath.Join(pkg, "scripts", "postinstall.js"),
		[]string{"AGENT_GM_CLI_BASE_URL=file://" + serveDir})
	if res.code == 0 {
		t.Fatal("postinstall installed a binary with no checksums.txt to verify it against")
	}
	if !strings.Contains(res.stderr, "checksums.txt") {
		t.Errorf("the failure does not say what is missing:\n%s", res.stderr)
	}
}

// AGENT_GM_CLI_SKIP_DOWNLOAD=1 is for a CI image that supplies the binary
// itself. "Skips cleanly" means exit 0 AND no download attempt: it is pointed
// at a base URL that does not exist, so a run that tried would fail.
func TestSkipDownloadSkipsCleanly(t *testing.T) {
	requireNode(t)
	pkg := stagePackage(t, "1.2.3", map[string]string{"agm_1.2.3_linux_amd64.tar.gz": strings.Repeat("a", 64)})
	res := runNode(t, pkg, filepath.Join(pkg, "scripts", "postinstall.js"), []string{
		"AGENT_GM_CLI_SKIP_DOWNLOAD=1",
		"AGENT_GM_CLI_BASE_URL=file:///nonexistent-on-purpose",
	})
	if res.code != 0 {
		t.Fatalf("postinstall exited %d with AGENT_GM_CLI_SKIP_DOWNLOAD=1\nstderr: %s", res.code, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(pkg, "vendor", "agm")); !os.IsNotExist(err) {
		t.Error("something was downloaded despite AGENT_GM_CLI_SKIP_DOWNLOAD=1")
	}
}

// AGENT_GM_CLI_BINARY points the shim at a binary that already exists. It has
// to work with NO vendor/agm present -- an image that sets it is exactly an
// image that never ran the download.
func TestBinaryOverrideRunsTheNamedBinaryWithNoVendoredCopy(t *testing.T) {
	requireNode(t)
	pkg := stagePackage(t, "1.2.3", nil)
	agm := buildAgm(t)

	res := runNode(t, pkg, filepath.Join(pkg, "bin", "agm.js"),
		[]string{"AGENT_GM_CLI_BINARY=" + agm}, "version")
	if res.code != 0 {
		t.Fatalf("the shim exited %d with AGENT_GM_CLI_BINARY set\nstderr: %s", res.code, res.stderr)
	}
	if !strings.HasPrefix(res.stdout, "agm ") {
		t.Errorf("the shim did not run the named binary; stdout was %q", res.stdout)
	}
}

// A shim with nothing to run must say so in a sentence an owner can act on,
// and must not exit 1: section 11.2 leaves 1 unassigned precisely so that a 1
// is never a diagnosis. 9 is "local configuration failure", which a missing
// local binary is.
func TestShimWithNoBinaryFailsWithADiagnosisRatherThanOne(t *testing.T) {
	requireNode(t)
	pkg := stagePackage(t, "1.2.3", nil)
	res := runNode(t, pkg, filepath.Join(pkg, "bin", "agm.js"), nil, "version")
	if res.code == 0 {
		t.Fatal("the shim succeeded with no binary installed")
	}
	if res.code == 1 {
		t.Error("the shim exited 1, which section 11.2 leaves unassigned so that a 1 is never Agent GM's answer")
	}
	if !strings.Contains(res.stderr, "vendor/agm") {
		t.Errorf("the shim does not say what is missing:\n%s", res.stderr)
	}
}

// --- acceptance test 4: exit codes survive the shim -----------------------------

// Section 11.2's codes are a contract with scripts, and a wrapper is exactly
// where a contract like that gets quietly lost -- a `try { … } catch { exit 1 }`
// is the natural way to write a shim and it destroys every one of the ten
// codes. Both halves of acceptance test 4 are here: a usage error, which needs
// no server, and a not_found, which needs one.
func TestExitCodesSurviveTheShim(t *testing.T) {
	requireNode(t)
	pkg := stagePackage(t, "1.2.3", nil)
	agm := buildAgm(t)
	shim := filepath.Join(pkg, "bin", "agm.js")

	// A stub server that answers one route with not_found in the envelope of
	// section 7.1. It never dials a phone and holds no real number.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no conversation with that id"},"request_id":"req_fixture"}`))
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		args []string
		env  []string
		want int
	}{
		{
			name: "a usage error is 2",
			args: []string{"--nonsense"},
			want: 2,
		},
		{
			name: "an absent resource is 5",
			args: []string{"conversations", "show", "conv_00000000000000000000000000"},
			env: []string{
				"AGENT_GM_URL=" + srv.URL,
				"AGENT_GM_ACCESS_TOKEN=fixture-access-token",
			},
			want: 5,
		},
		{
			name: "success is 0",
			args: []string{"version"},
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := append([]string{"AGENT_GM_CLI_BINARY=" + agm}, tc.env...)

			through := runNode(t, pkg, shim, env, tc.args...)
			if through.code != tc.want {
				t.Errorf("through the shim: exit %d, want %d\nstdout: %s\nstderr: %s",
					through.code, tc.want, through.stdout, through.stderr)
			}

			// "as it does natively" is half the clause, so the native run is
			// asserted too rather than assumed: if `agm` itself changed its
			// mind about the code, this test would otherwise start proving
			// that the shim faithfully propagates the wrong answer.
			cmd := exec.Command(agm, tc.args...)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, tc.env...)
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			native := 0
			if err := cmd.Run(); err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("running agm: %v", err)
				}
				native = ee.ExitCode()
			}
			if native != tc.want {
				t.Errorf("natively: exit %d, want %d\nstdout: %s\nstderr: %s",
					native, tc.want, out.String(), errb.String())
			}
			if native != through.code {
				t.Errorf("the shim changed the exit code: %d natively, %d through npm", native, through.code)
			}
		})
	}
}
