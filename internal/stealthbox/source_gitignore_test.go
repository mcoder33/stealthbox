package stealthbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSourceGitIgnoreImportExportAndDelete(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	for _, checkout := range []string{"team", "team/repo"} {
		for _, direction := range []string{"import", "export"} {
			t.Run(checkout+"/"+direction, func(t *testing.T) {
				c := sourceTestConfig(t)
				source, destination := c.Workspace.LocalRoot, c.Workspace.VMRoot
				if direction == "export" {
					source, destination = destination, source
				}
				writeSourceFile(t, source, "team/repo/.gitignore", "runtime/\n*.log\n!keep.log\n**/_output/\nliteral\\[1\\].txt\n")
				writeSourceFile(t, source, "team/repo/nested/.gitignore", "/cache/\n")
				writeSourceFile(t, destination, "team/repo/.gitignore", "private/\n")
				for name, content := range map[string]string{
					"main.txt": "source", "keep.log": "keep", "ignored.log": "sender log",
					"literal[1].txt": "ignored literal", "literal[2].txt": "included literal", "literal11.txt": "neighbor",
					"nested/cache/value": "cache", "nested/code.txt": "code", "nested/_output/value": "generated",
					"private/sender": "receiver policy excludes this",
				} {
					writeSourceFile(t, source, "team/repo/"+name, content)
				}
				for name, content := range map[string]string{
					"runtime/local.txt": "receiver runtime", "ignored.log": "receiver log",
					"literal[1].txt": "receiver literal", "private/local.txt": "receiver private", "stale.txt": "delete",
				} {
					writeSourceFile(t, destination, "team/repo/"+name, content)
				}
				if err := os.Symlink("missing-target", filepath.Join(destination, "team/repo/runtime/broken")); err != nil {
					t.Fatal(err)
				}
				plan := filepath.Join(t.TempDir(), "plan.json")
				var output bytes.Buffer
				if err := PreviewSourceSync(context.Background(), c, direction, checkout, true, plan, &output); err != nil {
					t.Fatal(err)
				}
				for _, hidden := range []string{"runtime/", "ignored.log", "literal[1]", "nested/cache", "_output", "private/"} {
					if strings.Contains(output.String(), hidden) {
						t.Fatalf("ignored path appeared in preview: %s", output.String())
					}
				}
				// New ignored files after preview neither invalidate the plan nor leak
				// into rsync. Receiver-only ignored directories remain protected.
				writeSourceFile(t, source, "team/repo/new.log", "new log")
				if err := ApplySourceSync(context.Background(), c, plan, &output); err != nil {
					t.Fatal(err)
				}
				for name, want := range map[string]string{
					"main.txt": "source", "keep.log": "keep", "literal[2].txt": "included literal", "literal11.txt": "neighbor", "nested/code.txt": "code",
					"runtime/local.txt": "receiver runtime", "ignored.log": "receiver log",
					"literal[1].txt": "receiver literal", "private/local.txt": "receiver private",
				} {
					got, err := os.ReadFile(filepath.Join(destination, "team/repo", name))
					if err != nil || string(got) != want {
						t.Fatalf("%s: got %q, want %q: %v", name, got, want, err)
					}
				}
				for _, name := range []string{"stale.txt", "new.log", "nested/cache/value", "nested/_output/value", "private/sender"} {
					if _, err := os.Lstat(filepath.Join(destination, "team/repo", name)); !os.IsNotExist(err) {
						t.Fatalf("%s should be absent: %v", name, err)
					}
				}
				if info, err := os.Lstat(filepath.Join(destination, "team/repo/runtime/broken")); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("ignored receiver symlink was removed", err)
				}
			})
		}
	}
}

func TestSourceGitIgnoreAncestorRulesAndNegationWithoutGit(t *testing.T) {
	c := sourceTestConfig(t)
	writeSourceFile(t, c.Workspace.LocalRoot, "team/.gitignore", "*.tmp\n!keep.tmp\nblocked/\n!blocked/keep.txt\n")
	writeSourceFile(t, c.Workspace.LocalRoot, "team/repo/.gitignore", "/root.txt\nleaves/*\n!leaves/keep.txt\n")
	writeSourceFile(t, c.Workspace.LocalRoot, "team/repo/nested/.gitignore", "!nested.tmp\n")
	for _, name := range []string{"root.txt", "keep.tmp", "drop.tmp", "leaves/drop.txt", "leaves/keep.txt", "blocked/keep.txt", "nested/root.txt", "nested/nested.tmp", "nested/drop.tmp"} {
		writeSourceFile(t, c.Workspace.LocalRoot, "team/repo/"+name, "value")
	}
	state, err := sourceStateAt(context.Background(), c.Workspace.LocalRoot, "team/repo")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range state.Entries {
		paths = append(paths, entry.Path)
	}
	for _, name := range []string{"keep.tmp", "leaves/keep.txt", "nested/root.txt", "nested/nested.tmp"} {
		if !slices.Contains(paths, name) {
			t.Fatalf("included path missing: %s, %v", name, paths)
		}
	}
	for _, name := range []string{"root.txt", "drop.tmp", "blocked/keep.txt", "leaves/drop.txt", "nested/drop.tmp"} {
		if slices.Contains(paths, name) {
			t.Fatalf("ignored path included: %s, %v", name, paths)
		}
	}
	if _, err := sourceStateAt(context.Background(), c.Workspace.LocalRoot, "team/repo/blocked"); err == nil || !strings.Contains(err.Error(), "ignored by .gitignore") {
		t.Fatal("ignored checkout was accepted", err)
	}
}

func TestSourceGitIgnoreRuleChangesInvalidatePlan(t *testing.T) {
	c := sourceTestConfig(t)
	// Even a .gitignore file hidden by its own rules is part of the policy hash.
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/.gitignore", ".*\ncache/\n")
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/main.txt", "value")
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := PreviewSourceSync(context.Background(), c, "import", "repo", false, plan, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/.gitignore", ".*\ncache/\n# changed policy\n")
	if err := ApplySourceSync(context.Background(), c, plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "changed since preview") {
		t.Fatal("changed ignore rules did not invalidate preview", err)
	}
	if _, err := os.Stat(filepath.Join(c.Workspace.VMRoot, "repo")); !os.IsNotExist(err) {
		t.Fatal("stale plan created destination", err)
	}
}

func TestSourceGitIgnoreRejectsLinksToIgnoredTargets(t *testing.T) {
	c := sourceTestConfig(t)
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/.gitignore", "hidden.txt\n")
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/hidden.txt", "hidden")
	if err := os.Symlink("hidden.txt", filepath.Join(c.Workspace.LocalRoot, "repo/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStateAtWithOptions(context.Background(), c.Workspace.LocalRoot, "repo", nil, true); err == nil || !strings.Contains(err.Error(), "excluded by .gitignore") {
		t.Fatal("link to ignored target was accepted", err)
	}
}

func TestSourceGitIgnoreRejectsInvalidRulePathsAndHonorsCancellation(t *testing.T) {
	for _, path := range []string{"../.gitignore", "/.gitignore", "repo/../.gitignore", "repo/main.txt", "repo\n/.gitignore"} {
		if m, err := newSourceIgnoreMatcher(context.Background(), []SourceIgnoreFile{{Path: path, Content: "*"}}); err == nil {
			m.Close()
			t.Fatalf("invalid rule path accepted: %q", path)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m, err := newSourceIgnoreMatcher(ctx, []SourceIgnoreFile{{Path: ".gitignore", Content: "*"}}); err == nil {
		m.Close()
		t.Fatal("cancelled matcher was started")
	}
}

func TestSourceGitIgnoreTransferOnlyUsesCurrentManifests(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	c := sourceTestConfig(t)
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/.gitignore", "*.log\n")
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/name*.txt", "literal filename")
	if err := os.MkdirAll(filepath.Join(c.Workspace.VMRoot, "repo"), 0700); err != nil {
		t.Fatal(err)
	}
	source, destination, err := sourceSyncStates(context.Background(), c, "repo", "import")
	if err != nil {
		t.Fatal(err)
	}
	includes, err := sourceIgnoreIncludeFile(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(includes)
	command, err := sourceSyncCommand(c, SourceSyncPlan{RelativePath: "repo", Direction: "import", SourceGitIgnore: true, GitIgnoreIncludeFile: includes})
	if err != nil {
		t.Fatal(err)
	}
	// A runtime writer runs after the final fingerprints, before rsync starts.
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/new.log", "generated after fingerprint")
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/name-other.txt", "not the literal filename")
	if err := ExecuteContext(context.Background(), command, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(c.Workspace.VMRoot, "repo/name*.txt")); err != nil || string(got) != "literal filename" {
		t.Fatal("literal wildcard filename was not transferred", string(got), err)
	}
	for _, path := range []string{"repo/new.log", "repo/name-other.txt"} {
		if _, err := os.Stat(filepath.Join(c.Workspace.VMRoot, path)); !os.IsNotExist(err) {
			t.Fatal("a path absent from the manifests was transferred", path, err)
		}
	}
}

func TestSourceGitIgnoreRejectsReceiverDirectoryTypeCollision(t *testing.T) {
	for _, direction := range []string{"import", "export"} {
		t.Run(direction, func(t *testing.T) {
			c := sourceTestConfig(t)
			source, destination := c.Workspace.LocalRoot, c.Workspace.VMRoot
			if direction == "export" {
				source, destination = destination, source
			}
			writeSourceFile(t, source, "repo/runtime", "source file with a directory name")
			writeSourceFile(t, destination, "repo/.gitignore", "runtime/\n")
			writeSourceFile(t, destination, "repo/runtime/local.txt", "receiver data")
			plan := filepath.Join(t.TempDir(), "plan.json")
			if err := PreviewSourceSync(context.Background(), c, direction, "repo", true, plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "conflicts with an ignored path") {
				t.Fatal("ignored receiver directory could be replaced by a file", err)
			}
			if got, err := os.ReadFile(filepath.Join(destination, "repo/runtime/local.txt")); err != nil || string(got) != "receiver data" {
				t.Fatal("preview changed ignored receiver data", string(got), err)
			}
			if _, err := os.Stat(plan); !os.IsNotExist(err) {
				t.Fatal("unsafe plan was created", err)
			}
		})
	}
}
