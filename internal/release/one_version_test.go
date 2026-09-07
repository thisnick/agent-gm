package release_test

// One version, read off real artefacts.
//
// Decision D39 makes `npm/package.json` the version authority for everything
// Agent GM ships: the container image, the `agent-gm` server, the `agm`
// command line and `@agent-gm/cli`. Four separate mechanisms carry it there --
// an ldflags stamp, a Docker build argument, an archive filename and an npm
// manifest -- and any one of them can drift on its own without breaking a
// build. The version they carry is the string an operator matches a running
// server against a release page with, so a drift is discovered by somebody
// trying to work out what is deployed.
//
// So this builds, packs and RUNS, rather than reading source: the version is
// taken out of `agm version`, out of `agent-gm version`, out of a real
// `GET /v1/health`, out of the archive names and out of the packed npm
// tarball, and all of them are compared with the manifest.
//
// The matrix is narrowed to this machine's own platform with
// AGENT_GM_RELEASE_TARGETS -- six cross compiles is a slow test and this test
// is not about the matrix (`matrix_consistency_test.go` is). release.sh
// refuses that narrowing on a tag, so a release still builds all six.

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// manifestVersion is the authority itself.
func manifestVersion(t *testing.T) string {
	t.Helper()
	var pkg struct {
		Version string `json:"version"`
	}
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "npm", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Version == "" {
		t.Fatal("npm/package.json declares no version, and it is the version authority (D39)")
	}
	return pkg.Version
}

// `release.sh version` is the one reader of the authority every other script
// and workflow goes through.
func TestReleaseScriptReportsTheManifestVersion(t *testing.T) {
	code, out := runRelease(t, nil, "version")
	if code != 0 {
		t.Fatalf("release.sh version failed:\n%s", out)
	}
	if got, want := strings.TrimSpace(out), manifestVersion(t); got != want {
		t.Fatalf("release.sh version says %q; npm/package.json says %q", got, want)
	}
}

// The one that matters: a real build, and every artefact asked what version it
// is.
func TestEveryArtefactCarriesTheOneVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries and packs the npm tarball")
	}
	root := repoRoot(t)
	want := manifestVersion(t)

	host := runtime.GOOS + ":" + runtime.GOARCH
	code, out := runRelease(t, []string{
		"AGENT_GM_RELEASE_TARGETS=agent-gm:" + host + " agm:" + host,
		// This build is not a release build, said out loud rather than
		// inherited. `ci.yml` also runs on a `vX.Y.Z` tag, so on that run the
		// environment carries GITHUB_REF_TYPE=tag -- and release.sh refuses
		// to narrow the matrix on a tag, exactly as it should, which turned
		// every tag's `ci` run red the first time one happened (v1.0.2, run
		// 34169055217). The refusal is right and stays; what was wrong was a
		// test taking the ref from whatever ran it, when the ref has nothing
		// to do with what it asserts.
		"GITHUB_REF_TYPE=branch",
		"GITHUB_REF_NAME=one-version-test",
	}, "build")
	if code != 0 {
		t.Fatalf("release.sh build failed:\n%s", out)
	}

	dist := filepath.Join(root, "dist")

	// 1 and 2. The archive NAMES, which are what the npm postinstall asks the
	// release page for: a name that disagrees with the package's version is a
	// 404 at install time, after npm has already reported success.
	for _, bin := range []string{"agent-gm", "agm"} {
		name := fmt.Sprintf("%s_%s_%s_%s.tar.gz", bin, want, runtime.GOOS, runtime.GOARCH)
		if _, err := os.Stat(filepath.Join(dist, name)); err != nil {
			entries, _ := filepath.Glob(filepath.Join(dist, bin+"_*.tar.gz"))
			for i := range entries {
				entries[i] = filepath.Base(entries[i])
			}
			t.Fatalf("the build produced no %s; it made %v", name, entries)
		}
	}

	// 3 and 4. The two binaries, EXTRACTED FROM THE ARCHIVE and executed. The
	// stamp is what `agm version` and `agent-gm version` print, and it is what
	// an operator compares against a release page.
	bindir := t.TempDir()
	for _, bin := range []string{"agent-gm", "agm"} {
		name := fmt.Sprintf("%s_%s_%s_%s.tar.gz", bin, want, runtime.GOOS, runtime.GOARCH)
		extract(t, filepath.Join(dist, name), bindir)
		got, err := exec.Command(filepath.Join(bindir, bin), "version").CombinedOutput()
		if err != nil {
			t.Fatalf("%s version: %v\n%s", bin, err, got)
		}
		if !strings.Contains(string(got), want) {
			t.Errorf("`%s version` says %q, which does not carry %s", bin, strings.TrimSpace(string(got)), want)
		}
	}

	// 5. `GET /v1/health`, from the server that was just built. Section 7.5
	// makes this the answer to "what is deployed", and it comes from a
	// different function than `agent-gm version` does -- so it is asked
	// separately rather than assumed.
	if v := healthVersion(t, filepath.Join(bindir, "agent-gm")); v != want {
		t.Errorf("GET /v1/health reports version %q, want %s", v, want)
	}

	// 6. The npm package, packed the way a release packs it. The manifest is
	// committed at the released version and nothing rewrites it (D39), so the
	// tarball's own package.json must already agree.
	code, out = runRelease(t, nil, "npm-pack")
	if code != 0 {
		t.Fatalf("release.sh npm-pack failed:\n%s", out)
	}
	tarball := filepath.Join(dist, "agent-gm-cli-"+want+".tgz")
	if _, err := os.Stat(tarball); err != nil {
		t.Fatalf("npm pack produced no agent-gm-cli-%s.tgz: %v", want, err)
	}
	packed := t.TempDir()
	extract(t, tarball, packed)
	var pkg struct {
		Version string `json:"version"`
	}
	b, err := os.ReadFile(filepath.Join(packed, "package", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Version != want {
		t.Errorf("the packed @agent-gm/cli says %s, want %s", pkg.Version, want)
	}

	// And the tree was not edited to make that true. The old flow rewrote
	// npm/package.json at pack time, which meant the committed manifest said
	// `0.0.0-dev` and the version lived in the tag; D39 inverts that, and a
	// rewrite creeping back would restore the old two-sources-of-truth
	// silently.
	if now := manifestVersion(t); now != want {
		t.Errorf("npm/package.json now says %s and said %s before the pack; nothing may "+
			"rewrite the version authority", now, want)
	}
}

// The image label, which no test can read without a registry, tied to the same
// authority through the script that sets it.
func TestTheImageVersionLabelComesFromTheManifest(t *testing.T) {
	sh := repoFile(t, "scripts/image.sh")
	dockerfile := repoFile(t, "Dockerfile")

	if !strings.Contains(dockerfile, `org.opencontainers.image.version="${VERSION}"`) {
		t.Error("the Dockerfile does not set org.opencontainers.image.version from ${VERSION}")
	}
	if strings.Contains(sh, `version="${ref_name#v}"`) {
		t.Error("scripts/image.sh still derives the version from the ref name. D39 makes " +
			"npm/package.json the authority; a tag is a label for the version, not its source")
	}
	if !strings.Contains(sh, `npm/package.json`) {
		t.Error("scripts/image.sh does not read npm/package.json, so the image's version label " +
			"and the binaries' version can disagree with nothing to catch it")
	}
	// And the tag it is asked to build must be the tag for THAT version.
	if !strings.Contains(sh, `[ "$ref_name" = "v$version" ]`) {
		t.Error("scripts/image.sh does not assert the tag it is building matches the manifest " +
			"version; an image tagged v1.0.3 whose binary answers 1.0.2 passes every later check")
	}
}

// --- helpers ------------------------------------------------------------------

func extract(t *testing.T, archive, into string) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		out := filepath.Join(into, filepath.Clean("/"+h.Name))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			t.Fatal(err)
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(w, tr); err != nil { //nolint:gosec
			t.Fatal(err)
		}
		_ = w.Close()
	}
}

// healthVersion starts the built server against the fake backend on an
// ephemeral port and reads the `version` field of GET /v1/health.
// adminSecret is a throwaway: this server has a temporary data directory, a
// port the operating system picked, the fake backend and a lifetime of a few
// seconds. It is 48 characters because section 12.1 refuses fewer than 43.
const adminSecret = "one-version-test-secret-one-version-test-secret1"

func healthVersion(t *testing.T, server string) string {
	t.Helper()

	// An ephemeral port the operating system picked, released immediately: the
	// server binds it a moment later. Nothing here may go near 8080, 8081,
	// 8090 or 8787, which belong to whatever else is on this machine.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	data := t.TempDir()
	cmd := exec.Command(server, "serve", "--addr", addr)
	cmd.Env = append(os.Environ(),
		"AGENT_GM_PUBLIC_URL=http://"+addr,
		"AGENT_GM_LISTEN_ADDR="+addr,
		"AGENT_GM_DATA_DIR="+data,
		"AGENT_GM_ADMIN_SECRET="+adminSecret,
		"AGENT_GM_DATA_KEY="+strings.Repeat("ab", 32),
		"AGENT_GM_BACKEND=fake",
		"AGENT_GM_ALLOW_FAKE=1",
		"AGENT_GM_LOG_LEVEL=error",
	)
	var log strings.Builder
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The only process this test ever signals is the one it started, by the
	// handle it started it with.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + addr

	// Liveness first: /healthz touches no database and answers as soon as the
	// listener is up.
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := client.Get(base + "/healthz") //nolint:noctx
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the built server never served /healthz on %s: %v\n%s", addr, err, log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// /v1/health carries the `read` scope, so it is read the way anything
	// reads it: through an admin session minted from the admin secret.
	resp, err := client.Post(base+"/v1/auth/admin-session", "application/json", //nolint:noctx
		strings.NewReader(`{"secret":"`+adminSecret+`"}`))
	if err != nil {
		t.Fatalf("minting an admin session: %v\n%s", err, log.String())
	}
	var session struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatalf("decoding the admin session: %v", err)
	}
	_ = resp.Body.Close()
	if session.Data.AccessToken == "" {
		t.Fatalf("the server minted no admin session (status %s)\n%s", resp.Status, log.String())
	}

	req, err := http.NewRequest(http.MethodGet, base+"/v1/health", nil) //nolint:noctx
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session.Data.AccessToken)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/health: %v\n%s", err, log.String())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/health answered %s\n%s", resp.Status, log.String())
	}
	var body struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding /v1/health: %v", err)
	}
	return body.Data.Version
}

// The guard the test above says "branch" to get past, asserted directly so
// that nobody makes a tag's `ci` run green by weakening it instead.
//
// `AGENT_GM_RELEASE_TARGETS` exists so a test can build one platform rather
// than six. On a tag it is refused, because section 14.3 lists six archives
// and a release that published five of them would be missing a platform on
// the release page with everything else looking normal.
func TestTheMatrixNarrowingIsRefusedOnATag(t *testing.T) {
	host := runtime.GOOS + ":" + runtime.GOARCH
	code, out := runRelease(t, []string{
		"AGENT_GM_RELEASE_TARGETS=agm:" + host,
		"GITHUB_REF_TYPE=tag",
		"GITHUB_REF_NAME=v" + manifestVersion(t),
	}, "build")
	if code == 0 {
		t.Fatalf("release.sh built a narrowed matrix on a tag:\n%s", out)
	}
	if !strings.Contains(out, "refused on a tag") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
}
