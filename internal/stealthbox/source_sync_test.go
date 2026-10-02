package stealthbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sourceTestConfig(t *testing.T) Config {
	t.Helper()
	base := t.TempDir()
	c, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.Workspace.Host = ""
	c.Workspace.VMRoot = filepath.Join(base, "vm")
	c.Workspace.LocalRoot = filepath.Join(base, "sources")
	c.Workspace.RunnerRoot = filepath.Join(base, "runners")
	for _, path := range []string{c.Workspace.VMRoot, c.Workspace.LocalRoot, c.Workspace.RunnerRoot} {
		if err = os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = EnsureWorkspaceIdentity(&c); err != nil {
		t.Fatal(err)
	}
	return c
}
func writeSourceFile(t *testing.T, root, path, content string) {
	t.Helper()
	file := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSourcePreviewAndExplicitRsyncApply(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	c := sourceTestConfig(t)
	for path, content := range map[string]string{"repo/main.txt": "new", "repo/.git/config": "private-git", "repo/.env": "secret", "repo/.codex/auth.json": "agent-secret", "repo/vendor/dependency": "local-dependency"} {
		writeSourceFile(t, c.Workspace.LocalRoot, path, content)
	}
	plan := filepath.Join(t.TempDir(), "plan.json")
	var output bytes.Buffer
	if err := PreviewSourceSync(context.Background(), c, "import", "repo", false, plan, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Workspace.VMRoot, "repo")); !os.IsNotExist(err) {
		t.Fatalf("preview created destination: %v", err)
	}
	if strings.Contains(output.String(), "secret") || strings.Contains(output.String(), ".git") {
		t.Fatal("preview exposed excluded paths")
	}
	if err := ApplySourceSync(context.Background(), c, plan, &output); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(c.Workspace.VMRoot, "repo/main.txt"))
	if err != nil || string(b) != "new" {
		t.Fatal(string(b), err)
	}
	for _, name := range []string{".git", ".env", ".codex", "vendor"} {
		if _, err := os.Stat(filepath.Join(c.Workspace.VMRoot, "repo", name)); !os.IsNotExist(err) {
			t.Fatal("excluded file copied", name)
		}
	}
	// no-delete preserves destination-only files; an explicit delete plan still preserves exclusions.
	writeSourceFile(t, c.Workspace.VMRoot, "repo/stale.txt", "stale")
	writeSourceFile(t, c.Workspace.VMRoot, "repo/vendor/dependency", "keep-vendor")
	writeSourceFile(t, c.Workspace.VMRoot, "repo/.env.local", "keep-env")
	plan2 := filepath.Join(t.TempDir(), "keep.json")
	if err := PreviewSourceSync(context.Background(), c, "import", "repo", false, plan2, &output); err != nil {
		t.Fatal(err)
	}
	if err := ApplySourceSync(context.Background(), c, plan2, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Workspace.VMRoot, "repo/stale.txt")); err != nil {
		t.Fatal("no-delete removed stale file")
	}
	plan3 := filepath.Join(t.TempDir(), "delete.json")
	outside := t.TempDir()
	writeSourceFile(t, outside, "dependency", "keep-vendor")
	if err := os.RemoveAll(filepath.Join(c.Workspace.VMRoot, "repo/vendor")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(c.Workspace.VMRoot, "repo/vendor")); err != nil {
		t.Fatal(err)
	}
	if err := PreviewSourceSync(context.Background(), c, "import", "repo", true, plan3, &output); err != nil {
		t.Fatal(err)
	}
	if err := ApplySourceSync(context.Background(), c, plan3, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Workspace.VMRoot, "repo/stale.txt")); !os.IsNotExist(err) {
		t.Fatal("explicit deletion did not happen")
	}
	for _, name := range []string{"vendor/dependency", ".env.local"} {
		if _, err := os.Stat(filepath.Join(c.Workspace.VMRoot, "repo", name)); err != nil {
			t.Fatal("excluded destination removed", name)
		}
	}
	if info, err := os.Lstat(filepath.Join(c.Workspace.VMRoot, "repo/vendor")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("excluded dependency symlink removed", err)
	}
}

func TestSourceApplyDetectsSameSizeSameTimeEdits(t *testing.T) {
	for _, side := range []string{"source", "destination"} {
		t.Run(side, func(t *testing.T) {
			c := sourceTestConfig(t)
			writeSourceFile(t, c.Workspace.LocalRoot, "repo/file", "aaaa")
			writeSourceFile(t, c.Workspace.VMRoot, "repo/file", "bbbb")
			plan := filepath.Join(t.TempDir(), "plan.json")
			if err := PreviewSourceSync(context.Background(), c, "import", "repo", false, plan, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			root := c.Workspace.LocalRoot
			if side == "destination" {
				root = c.Workspace.VMRoot
			}
			file := filepath.Join(root, "repo/file")
			before, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(file, []byte("cccc"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.Chtimes(file, before.ModTime(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err = ApplySourceSync(context.Background(), c, plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "changed since preview") {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(filepath.Join(c.Workspace.VMRoot, "repo/file"))
			expected := "bbbb"
			if side == "destination" {
				expected = "cccc"
			}
			if string(b) != expected {
				t.Fatal("conflict changed destination", string(b))
			}
		})
	}
}

func TestSourceSyncContainment(t *testing.T) {
	c := sourceTestConfig(t)
	for _, path := range []string{"", ".", "..", "../outside", "team/../repo", "/absolute", ".git", "repo/vendor", "repo/.env.secret"} {
		if _, err := sourceRelativePath(path); err == nil {
			t.Fatalf("accepted unsafe source path %q", path)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(c.Workspace.LocalRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStateAt(context.Background(), c.Workspace.LocalRoot, "escape/project"); err == nil {
		t.Fatal("accepted symlink path escape")
	}
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/main", "hello")
	if err := os.Symlink(filepath.Join(outside, "other"), filepath.Join(c.Workspace.LocalRoot, "repo/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStateAt(context.Background(), c.Workspace.LocalRoot, "repo"); err == nil {
		t.Fatal("accepted source symlink")
	}
}

func TestSourceExportAndChangedConfig(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	c := sourceTestConfig(t)
	writeSourceFile(t, c.Workspace.VMRoot, "team/repo/file", "vm-authoritative")
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := PreviewSourceSync(context.Background(), c, "export", "team/repo", false, plan, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	changed := c
	changed.Workspace.ID = "different"
	if err := ApplySourceSync(context.Background(), changed, plan, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted foreign config")
	}
	if err := ApplySourceSync(context.Background(), c, plan, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(c.Workspace.LocalRoot, "team/repo/file"))
	if err != nil || string(b) != "vm-authoritative" {
		t.Fatal(string(b), err)
	}
}

func TestSourceRsyncSSHConfigAndDelete(t *testing.T) {
	c := sourceTestConfig(t)
	c.Workspace.Host = "dev-vm"
	c.Workspace.VMRoot = "/home/dev/Projects"
	t.Setenv("STEALTHBOX_SSH_CONFIG", "/tmp/config with spaces")
	command, err := sourceSyncCommand(c, SourceSyncPlan{RelativePath: "repo", Direction: "export", Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command.Args, " ")
	if !strings.Contains(joined, "--delete-delay") || !strings.Contains(joined, "'/tmp/config with spaces'") || !strings.Contains(joined, "dev-vm:/home/dev/Projects/repo/") {
		t.Fatal(joined)
	}
}

func TestSourceSyncRejectsCanonicalRootOverlap(t *testing.T) {
	c := sourceTestConfig(t)
	alias := filepath.Join(t.TempDir(), "vm-alias")
	if err := os.Symlink(c.Workspace.LocalRoot, alias); err != nil {
		t.Fatal(err)
	}
	c.Workspace.VMRoot = alias
	if err := validateSourceSyncConfig(c); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatal(err)
	}
}

func TestSourcePreviewRejectsPlanInsideCheckout(t *testing.T) {
	c := sourceTestConfig(t)
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/file", "source")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(c.Workspace.LocalRoot, "repo"), alias); err != nil {
		t.Fatal(err)
	}
	for _, plan := range []string{filepath.Join(c.Workspace.LocalRoot, "repo/plan.json"), filepath.Join(alias, "plan.json")} {
		if err := PreviewSourceSync(context.Background(), c, "import", "repo", false, plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatal(err)
		}
		if _, err := os.Stat(plan); !os.IsNotExist(err) {
			t.Fatal("preview wrote plan into checkout")
		}
	}
}
