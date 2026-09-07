package lint_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The deployment-host lint, tested the way the name lint is.
//
// A lint nothing exercises is a lint that can be deleted or weakened
// invisibly -- the lesson the Slice 3b reviewer taught with the SDK pin guard,
// whose whole block could be removed with CI staying green. So this drives the
// real script over a temporary tree and requires it to fail on a file that
// names the owner's deployment hostname, and to pass on one that does not.
func TestNoDeploymentHostCatchesTheHostname(t *testing.T) {
	// Assembled rather than written literally, so that this file does not
	// itself trip the check it is testing.
	host := "gm." + "agent" + "-wx.app"

	for _, c := range []struct {
		name    string
		path    string
		body    string
		wantErr bool
	}{
		{"a Go file naming it", "internal/api/thing.go",
			"package api\n\nconst base = \"https://" + host + "\"\n", true},
		{"a test naming it", "internal/api/thing_test.go",
			"package api\n\n// https://" + host + "\n", true},
		{"a docs page naming it", "docs/api.md",
			"Base: `https://" + host + "/v1`\n", true},
		{"a comment naming it", "internal/gm/thing.go",
			"package gm\n\n// see https://" + host + " for the deployment\n", true},
		// The two places it is allowed, because they are ABOUT the owner's
		// deployment rather than stating a contract against it.
		{"the spec", "plans/AGENT_GM_SPEC.md",
			"The owner's deployment is `https://" + host + "`.\n", false},
		{"the deployment guide", "docs/deploy.md",
			"For example, `https://" + host + "`.\n", false},
		// And the fixture origin is never a finding.
		{"the fixture origin", "internal/api/thing.go",
			"package api\n\nconst base = \"https://gm.example.test\"\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := newTree(t)
			write(t, root, c.path, c.body)
			out, err := runLint(t, root)
			if c.wantErr && err == nil {
				t.Fatalf("the lint passed a tree whose %s names the deployment hostname:\n%s",
					c.path, out)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("the lint failed a tree it should accept (%s):\n%s", c.path, out)
			}
			if c.wantErr && !strings.Contains(out, c.path) {
				t.Errorf("the failure does not name %s, so a reader cannot find it:\n%s",
					c.path, out)
			}
		})
	}
}

// newTree is a git repository with the lint script in it. It is a real
// repository because the script uses `git grep`, which is what makes it fast
// and what makes it respect .gitignore.
func newTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.test"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	body, err := os.ReadFile("../../scripts/no-deployment-host.sh")
	if err != nil {
		t.Fatalf("reading the script: %v", err)
	}
	write(t, root, "scripts/no-deployment-host.sh", string(body))
	return root
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("writing %s: %v", rel, err)
	}
	// `git grep` only sees tracked files, which is also why an untracked
	// scratch file cannot trip the job in a developer's tree.
	cmd := exec.Command("git", "add", rel)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", rel, err, out)
	}
}

func runLint(t *testing.T, root string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "no-deployment-host.sh"))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}
