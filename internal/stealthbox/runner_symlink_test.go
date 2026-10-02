package stealthbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRsyncRemovesDanglingReceiverAssetsAndPreservesProtectedLinks(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	source := canonicalTemp(t)
	runner := filepath.Join(canonicalTemp(t), "runner")
	outside := canonicalTemp(t)
	if err := validateRunnerRoot(runner); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(source, "backend"), filepath.Join(runner, "backend/web/assets")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(source, "backend/code.php"): "updated source",
		filepath.Join(runner, "backend/code.php"): "stale source",
		filepath.Join(outside, "proof"):           "untouched outside",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	assetLink := filepath.Join(runner, "backend/web/assets/c5e42a75")
	if err := os.Symlink("/app/vendor/bower-asset/jquery.tablesorter/dist", assetLink); err != nil {
		t.Fatal(err)
	}
	protected := []string{"vendor", "node_modules", ".env.local", ".git", ".claude", ".codex", ".serena", ".agents", ".opencode", ".kimi-code", ".stealthbox-qa"}
	for _, name := range protected {
		if err := os.Symlink(outside, filepath.Join(runner, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(runner, "unprotected-outside-link")); err != nil {
		t.Fatal(err)
	}
	command := runnerRsyncCommand(Endpoint{Path: source}, Endpoint{Path: runner})
	// Exercise the failing Apple implementation, not an emulation of its error.
	if runtime.GOOS == "darwin" {
		command.Program = "/usr/bin/rsync"
		var stderr strings.Builder
		if err := ExecuteContext(context.Background(), command, nil, io.Discard, &stderr); err == nil || !strings.Contains(strings.ToLower(stderr.String()), "directory not empty") {
			t.Fatal("Apple rsync did not reproduce dangling receiver deletion failure", err, stderr.String())
		}
	}
	if err := removeUnprotectedRunnerSymlinks(context.Background(), runner); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if err := ExecuteContext(context.Background(), command, nil, io.Discard, &stderr); err != nil {
		t.Fatal("rsync after anchored cleanup failed", err, stderr.String())
	}
	if content, err := os.ReadFile(filepath.Join(runner, "backend/code.php")); err != nil || string(content) != "updated source" {
		t.Fatal("source edit lost", err)
	}
	for _, name := range []string{"backend/web", "unprotected-outside-link"} {
		if _, err := os.Lstat(filepath.Join(runner, name)); !os.IsNotExist(err) {
			t.Fatal("stale directory/link retained", name, err)
		}
	}
	for _, name := range protected {
		if target, err := os.Readlink(filepath.Join(runner, name)); err != nil || target != outside {
			t.Fatal("protected receiver link changed", name, target, err)
		}
	}
	if content, err := os.ReadFile(filepath.Join(outside, "proof")); err != nil || string(content) != "untouched outside" {
		t.Fatal("cleanup touched outside link target", err)
	}
}

func TestPullRunnerDoesNotCleanLinksBeforeSourceSizePreflight(t *testing.T) {
	root := canonicalTemp(t)
	runner := filepath.Join(root, "runner")
	if err := validateRunnerRoot(runner); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(runner, "dangling")
	if err := os.Symlink("/app/vendor/fixture-missing", link); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "rsync"), []byte("#!/bin/sh\nprintf 'Total file size: 9,999,999,999 bytes\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := PullRunner(context.Background(), Config{}, Endpoint{Host: "fixture", Path: root}, runner, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "selected source exceeds") {
		t.Fatal("oversized source accepted", err)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Fatal("receiver link removed before source passed size preflight", err)
	}
}

func TestPullRunnerReturnsBoundedRsyncStderr(t *testing.T) {
	root := canonicalTemp(t)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case " $* " in
  *" --dry-run "*) printf 'Total file size: 1 bytes\n'; exit 0 ;;
esac
printf 'stdout must not be returned\n'
i=0
while [ "$i" -lt 1000 ]; do
  printf 'fixture repeated rsync diagnostic line\n' >&2
  i=$((i+1))
done
printf 'fixture unlinkat Directory not empty\n' >&2
exit 23
`
	if err := os.WriteFile(filepath.Join(bin, "rsync"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := PullRunner(context.Background(), Config{}, Endpoint{Host: "fixture", Path: root}, filepath.Join(root, "runner"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rsync source pull") || !strings.Contains(err.Error(), "fixture unlinkat Directory not empty") {
		t.Fatal("rsync diagnostics lost", err)
	}
	if len(err.Error()) > runnerSyncErrorOutputLimit+200 || strings.Contains(err.Error(), "stdout must not be returned") {
		t.Fatal("unbounded/wrong stream returned", len(err.Error()))
	}
}
