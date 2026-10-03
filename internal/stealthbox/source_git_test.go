package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := sourceGitCommand(context.Background(), directory, args...)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Test Author", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test Author", "GIT_COMMITTER_EMAIL=test@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initTestGit(t *testing.T, root, relative string) string {
	t.Helper()
	writeSourceFile(t, root, relative+"/main.txt", "committed\n")
	directory := filepath.Join(root, relative)
	testGit(t, directory, "init", "-b", "main")
	testGit(t, directory, "add", "main.txt")
	testGit(t, directory, "commit", "-m", "Initial")
	return directory
}

func TestSourceGitImportsNestedReposAndPreservesWorkingFiles(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	c := sourceTestConfig(t)
	directory := initTestGit(t, c.Workspace.LocalRoot, "group/repo")
	initTestGit(t, c.Workspace.LocalRoot, "group/other")
	testGit(t, directory, "branch", "feature")
	testGit(t, directory, "tag", "v1")
	testGit(t, directory, "remote", "add", "origin", "https://user:private-token@example.test/team/repo.git?secret=token")
	testGit(t, directory, "update-ref", "refs/remotes/origin/main", "HEAD")
	testGit(t, directory, "branch", "--set-upstream-to=origin/main", "main")
	testGit(t, directory, "config", "user.name", "Portable Author")
	testGit(t, directory, "config", "user.email", "portable@example.test")
	writeSourceFile(t, c.Workspace.LocalRoot, "group/repo/.git/hooks/private-hook", "private-hook")
	writeSourceFile(t, c.Workspace.LocalRoot, "group/repo/stash-only.txt", "private-stash")
	testGit(t, directory, "add", "stash-only.txt")
	testGit(t, directory, "stash", "push", "-m", "private")
	stash := testGit(t, directory, "rev-parse", "refs/stash")
	writeSourceFile(t, c.Workspace.LocalRoot, "group/repo/main.txt", "uncommitted\n")
	writeSourceFile(t, c.Workspace.LocalRoot, "group/repo/untracked.txt", "keep\n")
	plan := filepath.Join(t.TempDir(), "import.json")
	var output bytes.Buffer
	if err := PreviewSourceSync(context.Background(), c, "import", "group", false, plan, &output); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(plan)
	if strings.Contains(string(data), "private-token") || strings.Contains(string(data), "secret=token") {
		t.Fatal("plan leaked a credential")
	}
	if !strings.Contains(output.String(), "git-init\trepo") || !strings.Contains(output.String(), "git-init\tother") {
		t.Fatal("missing nested Git previews", output.String())
	}
	if err := ApplySourceSync(context.Background(), c, plan, &output); err != nil {
		t.Fatal(err)
	}
	vm := filepath.Join(c.Workspace.VMRoot, "group/repo")
	if testGit(t, vm, "rev-parse", "HEAD") != testGit(t, directory, "rev-parse", "HEAD") {
		t.Fatal("history changed")
	}
	if testGit(t, vm, "rev-parse", "--is-inside-work-tree") != "true" {
		t.Fatal("installer prerequisite failed")
	}
	if testGit(t, vm, "symbolic-ref", "HEAD") != "refs/heads/main" {
		t.Fatal("branch changed")
	}
	if testGit(t, vm, "rev-parse", "feature", "v1", "@{upstream}") == "" {
		t.Fatal("missing references")
	}
	if testGit(t, vm, "remote", "get-url", "origin") != "https://example.test/team/repo.git" {
		t.Fatal("remote URL not sanitized")
	}
	if testGit(t, vm, "config", "user.email") != "portable@example.test" {
		t.Fatal("author not copied")
	}
	status := testGit(t, vm, "status", "--porcelain")
	if !strings.Contains(status, "M main.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatal("working files lost", status)
	}
	for _, private := range []string{"hooks/private-hook", "refs/stash"} {
		if _, err := os.Stat(filepath.Join(vm, ".git", private)); !os.IsNotExist(err) {
			t.Fatal("private metadata copied", private)
		}
	}
	if sourceGitCommand(context.Background(), vm, "cat-file", "-e", stash).Run() == nil {
		t.Fatal("stash objects copied")
	}
	// Later imports update files while retaining VM commits and branch state.
	testGit(t, vm, "checkout", "-b", "vm-work")
	testGit(t, vm, "add", "main.txt")
	testGit(t, vm, "commit", "-m", "VM work")
	vmHead := testGit(t, vm, "rev-parse", "HEAD")
	plan = filepath.Join(t.TempDir(), "again.json")
	if err := PreviewSourceSync(context.Background(), c, "import", "group", false, plan, &output); err != nil {
		t.Fatal(err)
	}
	if err := ApplySourceSync(context.Background(), c, plan, &output); err != nil {
		t.Fatal(err)
	}
	if testGit(t, vm, "rev-parse", "HEAD") != vmHead {
		t.Fatal("existing VM Git overwritten")
	}
}

func TestSourceGitPreviewRejectsChangedRefsAndSupportsWorktrees(t *testing.T) {
	c := sourceTestConfig(t)
	directory := initTestGit(t, c.Workspace.LocalRoot, "origin")
	testGit(t, directory, "worktree", "add", "-b", "work", filepath.Join(c.Workspace.LocalRoot, "work"))
	plan := filepath.Join(t.TempDir(), "work.json")
	if err := PreviewSourceSync(context.Background(), c, "import", "work", false, plan, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(plan)
	var preview SourceSyncPlan
	if err := json.Unmarshal(data, &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.GitBootstrap) != 1 || preview.GitBootstrap[0].Branch != "refs/heads/work" {
		t.Fatal("worktree not recognized", string(data))
	}
	testGit(t, filepath.Join(c.Workspace.LocalRoot, "work"), "tag", "after-preview")
	if err := ApplySourceSync(context.Background(), c, plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "changed since preview") {
		t.Fatal("changed Git refs were not rejected", err)
	}
	if _, err := os.Stat(filepath.Join(c.Workspace.VMRoot, "work")); !os.IsNotExist(err) {
		t.Fatal("destination modified before validation")
	}
}

func TestSourceGitDetachedAndUnborn(t *testing.T) {
	for _, unborn := range []bool{false, true} {
		t.Run(map[bool]string{true: "unborn", false: "detached"}[unborn], func(t *testing.T) {
			c := sourceTestConfig(t)
			var dir string
			if unborn {
				writeSourceFile(t, c.Workspace.LocalRoot, "repo/README", "new")
				dir = filepath.Join(c.Workspace.LocalRoot, "repo")
				testGit(t, dir, "init", "-b", "new")
			} else {
				dir = initTestGit(t, c.Workspace.LocalRoot, "repo")
				testGit(t, dir, "checkout", "--detach")
			}
			r, err := sourceGitRepository(context.Background(), dir, ".")
			if err != nil {
				t.Fatal(err)
			}
			bundles, clean, err := prepareSourceGitBundles(context.Background(), c, "repo", []SourceGitRepository{r})
			if err != nil {
				t.Fatal(err)
			}
			defer clean()
			writeSourceFile(t, c.Workspace.VMRoot, "repo/main.txt", "do not reset")
			if err = applySourceGitBundles(context.Background(), c, "repo", []SourceGitRepository{r}, bundles, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			vm := filepath.Join(c.Workspace.VMRoot, "repo")
			if unborn {
				if testGit(t, vm, "symbolic-ref", "HEAD") != "refs/heads/new" {
					t.Fatal("unborn HEAD lost")
				}
			} else {
				if sourceGitValue(context.Background(), vm, "symbolic-ref", "-q", "HEAD") != "" {
					t.Fatal("HEAD reattached")
				}
			}
			data, _ := os.ReadFile(filepath.Join(vm, "main.txt"))
			if string(data) != "do not reset" {
				t.Fatal("working file reset")
			}
		})
	}
}

func TestSourceGitInstallNeverReplacesDestination(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	os.Mkdir(src, 0700)
	os.Mkdir(dst, 0700)
	os.WriteFile(filepath.Join(dst, "keep"), []byte("keep"), 0600)
	if err := installGitDirectory(src, dst); err == nil {
		t.Fatal("replaced existing destination")
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "keep")); string(data) != "keep" {
		t.Fatal("existing Git removed")
	}
}

func TestPortableGitURLs(t *testing.T) {
	for _, raw := range []string{"git@gitlab.example.test:team/repo.git", "host:repo.git", "git@[::1]:team/repo.git", "ssh://git@host:2222/team/repo.git", "/local/repo.git"} {
		if portableGitURL(raw) != raw {
			t.Fatal("portable Git URL changed", raw, portableGitURL(raw))
		}
	}
	if value := portableGitURL("https://oauth2:secret@host/team/repo.git?token=secret#fragment"); value != "https://host/team/repo.git" {
		t.Fatal("HTTP credential leaked", value)
	}
}
