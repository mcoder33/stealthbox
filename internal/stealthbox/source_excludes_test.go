package stealthbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSourceSymlinkModeCanonicalAcrossPlatforms(t *testing.T) {
	c := sourceTestConfig(t)
	c.Workspace.SourceSafeLinks = true
	writeSourceFile(t, c.Workspace.VMRoot, "repo/AGENTS.md", "tracked instructions")
	if err := os.Symlink("AGENTS.md", filepath.Join(c.Workspace.VMRoot, "repo/CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	actual, err := SourceFingerprint(context.Background(), c, "repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, permissions := range []fs.FileMode{0755, 0777} {
		platformState := SourceState{Exists: true, Entries: slices.Clone(actual.Entries)}
		found := false
		for index, entry := range platformState.Entries {
			if entry.Kind == "symlink" {
				found = true
				if entry.Mode != 0 {
					t.Fatal("live manifest has platform-specific symlink mode", entry)
				}
				platformState.Entries[index].Mode = sourceEntryMode(fs.ModeSymlink | permissions)
			}
		}
		if !found {
			t.Fatal("live fingerprint omitted the symlink")
		}
		serialized, err := json.Marshal(struct {
			Entries []SourceEntry
			Rules   SourceIgnoreRules
		}{platformState.Entries, actual.IgnoreRules})
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(serialized)
		platformState.Hash = hex.EncodeToString(hash[:])
		if platformState.Hash != actual.Hash {
			t.Fatalf("%#o link metadata changed fingerprint: %s != %s", permissions, platformState.Hash, actual.Hash)
		}
		if changes := sourceChanges(actual, platformState, true); len(changes) != 0 {
			t.Fatalf("%#o link metadata produced changes: %+v", permissions, changes)
		}
		if sourceEntryMode(permissions) != uint32(permissions) || sourceEntryMode(fs.ModeDir|permissions) != uint32(permissions) {
			t.Fatal("normal file or directory permissions were normalized")
		}
	}
}

func TestSourceExcludesValidateAndRoundTrip(t *testing.T) {
	for _, exclude := range []string{"", ".", "..", "../repo", "/repo/data", "repo/../data", "repo/./data", "repo//data", "repo/data/", "repo/*", "repo/?", "repo/[ab]", "repo\\data", "C:/data", "repo\ndata", "repo\x00data"} {
		t.Run(exclude, func(t *testing.T) {
			c := sourceTestConfig(t)
			c.Workspace.SourceExcludes = []string{exclude}
			if err := c.Validate(); err == nil {
				t.Fatalf("accepted invalid exclusion %q", exclude)
			}
		})
	}
	c := sourceTestConfig(t)
	c.Workspace.SourceExcludes = []string{"team/repo/data", "team/repo/local state.txt"}
	c.Workspace.SourceSafeLinks = true
	config := filepath.Join(t.TempDir(), "config.json")
	if err := Save(config, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(config)
	if err != nil || !slices.Equal(loaded.Workspace.SourceExcludes, c.Workspace.SourceExcludes) || !loaded.Workspace.SourceSafeLinks {
		t.Fatalf("load lost exclusions: %+v %v", loaded.Workspace, err)
	}
	remote, err := remoteConfig(loaded)
	if err != nil || !slices.Equal(remote.Workspace.SourceExcludes, c.Workspace.SourceExcludes) || !remote.Workspace.SourceSafeLinks {
		t.Fatalf("remote config lost exclusions: %+v %v", remote.Workspace, err)
	}
	data, err := json.Marshal(remote)
	if err != nil || !bytes.Contains(data, []byte(`"source_excludes"`)) {
		t.Fatalf("deployment serialization lost exclusions: %s %v", data, err)
	}
}

func TestSourceExcludesCheckoutMapping(t *testing.T) {
	excludes := []string{"team/repo/data", "team/repo/local.txt", "other/data"}
	for _, test := range []struct {
		checkout string
		want     []string
	}{
		{"team", []string{"repo/data", "repo/local.txt"}},
		{"team/repo", []string{"data", "local.txt"}},
		{"team/repo-neighbor", []string{}},
	} {
		got, err := checkoutSourceExcludes(test.checkout, excludes)
		if err != nil || !slices.Equal(got, test.want) {
			t.Fatalf("%s: %v %v", test.checkout, got, err)
		}
	}
	for _, checkout := range []string{"team/repo/data", "team/repo/data/nested"} {
		if _, err := checkoutSourceExcludes(checkout, excludes); err == nil {
			t.Fatal("allowed excluded checkout", checkout)
		}
	}
}

func TestSourceExcludesRsyncImportExportAndDelete(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	for _, checkout := range []string{"team", "team/repo"} {
		for _, direction := range []string{"import", "export"} {
			t.Run(checkout+"/"+direction, func(t *testing.T) {
				c := sourceTestConfig(t)
				c.Workspace.SourceExcludes = []string{"team/repo/data", "team/repo/local state.txt"}
				source, destination := c.Workspace.LocalRoot, c.Workspace.VMRoot
				if direction == "export" {
					source, destination = destination, source
				}
				for _, root := range []string{source, destination} {
					writeSourceFile(t, root, "team/repo/data/cache/uv/archive-v0/bin/keep", "runtime")
					if err := os.Symlink("missing-python", filepath.Join(root, "team/repo/data/cache/uv/archive-v0/bin/python")); err != nil {
						t.Fatal(err)
					}
				}
				writeSourceFile(t, source, "team/repo/main.txt", "source")
				writeSourceFile(t, source, "team/repo/data-neighbor/file", "legitimate data")
				writeSourceFile(t, source, "team/repo/nested/data/file", "nested data")
				writeSourceFile(t, source, "team/repo/local state.txt", "do not transfer")
				writeSourceFile(t, source, "team/neighbor/data/file", "neighbor data")
				writeSourceFile(t, destination, "team/repo/main.txt", "old")
				writeSourceFile(t, destination, "team/repo/stale.txt", "delete")
				writeSourceFile(t, destination, "team/repo/local state.txt", "receiver local")
				planFile := filepath.Join(t.TempDir(), "plan.json")
				var output bytes.Buffer
				if err := PreviewSourceSync(context.Background(), c, direction, checkout, true, planFile, &output); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(planFile)
				if err != nil {
					t.Fatal(err)
				}
				var plan SourceSyncPlan
				if err = json.Unmarshal(data, &plan); err != nil || !slices.Equal(plan.SourceExcludes, c.Workspace.SourceExcludes) {
					t.Fatalf("plan lost exclusions: %+v %v", plan, err)
				}
				for _, change := range plan.Changes {
					if strings.Contains(change.Path, "uv/") || strings.Contains(change.Path, "local state") {
						t.Fatalf("excluded change: %+v", change)
					}
				}
				// Mutating excluded runtime files must not invalidate the reviewed source plan.
				writeSourceFile(t, source, "team/repo/data/new", "new runtime")
				if err = ApplySourceSync(context.Background(), c, planFile, &output); err != nil {
					t.Fatal(err)
				}
				for name, want := range map[string]string{"main.txt": "source", "data-neighbor/file": "legitimate data", "nested/data/file": "nested data", "data/cache/uv/archive-v0/bin/keep": "runtime", "local state.txt": "receiver local"} {
					got, err := os.ReadFile(filepath.Join(destination, "team/repo", name))
					if err != nil || string(got) != want {
						t.Fatalf("%s: %q %v", name, got, err)
					}
				}
				if _, err = os.Stat(filepath.Join(destination, "team/repo/stale.txt")); !os.IsNotExist(err) {
					t.Fatal("stale file survived --delete", err)
				}
				if _, err = os.Stat(filepath.Join(destination, "team/repo/data/new")); !os.IsNotExist(err) {
					t.Fatal("excluded runtime file transferred", err)
				}
				if info, err := os.Lstat(filepath.Join(destination, "team/repo/data/cache/uv/archive-v0/bin/python")); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("receiver runtime symlink removed", err)
				}
				if checkout == "team" {
					if got, err := os.ReadFile(filepath.Join(destination, "team/neighbor/data/file")); err != nil || string(got) != "neighbor data" {
						t.Fatal("neighbor source missing", string(got), err)
					}
				}
				local, remote, err := sourceSyncStates(context.Background(), c, checkout, direction)
				if err != nil || local.Hash != remote.Hash {
					t.Fatalf("post-transfer fingerprints differ: %s %s %v", local.Hash, remote.Hash, err)
				}
			})
		}
	}
}

func TestSourceExcludesKeepUnrelatedSymlinksRejected(t *testing.T) {
	c := sourceTestConfig(t)
	c.Workspace.SourceExcludes = []string{"team/repo/data"}
	writeSourceFile(t, c.Workspace.LocalRoot, "team/repo/main", "source")
	if err := os.Symlink("missing", filepath.Join(c.Workspace.LocalRoot, "team/repo/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStateAt(context.Background(), c.Workspace.LocalRoot, "team", c.Workspace.SourceExcludes...); err == nil || !strings.Contains(err.Error(), "refuses symlinks") {
		t.Fatal("unrelated symlink accepted", err)
	}
}

func TestSourceExcludesRejectCheckoutAndStalePlans(t *testing.T) {
	c := sourceTestConfig(t)
	writeSourceFile(t, c.Workspace.LocalRoot, "team/repo/main", "source")
	plan := filepath.Join(t.TempDir(), "legacy-plan.json")
	if err := PreviewSourceSync(context.Background(), c, "import", "team/repo", false, plan, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// A plan with no exclusion field remains compatible only with no exclusions.
	changed := c
	changed.Workspace.SourceExcludes = []string{"team/repo/data"}
	if err := ApplySourceSync(context.Background(), changed, plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "exclusions changed") {
		t.Fatal("accepted stale exclusions", err)
	}
	if _, err := exec.LookPath("rsync"); err == nil {
		if err := ApplySourceSync(context.Background(), c, plan, &bytes.Buffer{}); err != nil {
			t.Fatal("legacy no-exclusions plan rejected", err)
		}
	}
	c.Workspace.SourceExcludes = []string{"team/repo"}
	for _, checkout := range []string{"team/repo", "team/repo/data"} {
		if err := PreviewSourceSync(context.Background(), c, "import", checkout, false, filepath.Join(t.TempDir(), "plan.json"), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "excluded") {
			t.Fatal("excluded checkout accepted", err)
		}
	}
}

func TestSourceExcludesRemoteArgsAndRunnerIsolation(t *testing.T) {
	c := sourceTestConfig(t)
	c.Workspace.RemoteDir = "/home/dev/.stealthbox"
	for _, excludes := range [][]string{nil, {"team/repo/data"}} {
		c.Workspace.SourceExcludes = excludes
		for _, action := range []string{"fingerprint", "prepare"} {
			args := remoteSourceArgs(c, action, "team/repo")
			index := slices.Index(args, "--source-excludes")
			if index == -1 {
				t.Fatal("remote call relies on stale remote exclusions", args)
			}
			var got []string
			if err := json.Unmarshal([]byte(args[index+1]), &got); err != nil || got == nil || !slices.Equal(got, excludes) {
				t.Fatalf("bad remote exclusions: %v %v", args, err)
			}
			for _, enabled := range []bool{false, true} {
				c.Workspace.SourceSafeLinks = enabled
				args = remoteSourceArgs(c, action, "team/repo")
				want := "--source-safe-links=false"
				if enabled {
					want = "--source-safe-links=true"
				}
				if !slices.Contains(args, want) {
					t.Fatal("remote call relies on stale safe-link mode", args)
				}
			}
		}
	}
	command := runnerRsyncCommand(Endpoint{Path: c.Workspace.VMRoot}, Endpoint{Path: c.Workspace.RunnerRoot})
	if strings.Contains(strings.Join(command.Args, " "), "repo/data") {
		t.Fatal("source exclusions leaked into runner sync", command)
	}
	if !slices.Contains(command.Args, "--no-links") || slices.Contains(command.Args, "--links") {
		t.Fatal("safe source links changed runner behavior", command)
	}
}

func TestSourceSafeLinksRsyncImportExport(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	for _, checkout := range []string{"team", "team/repo"} {
		for _, direction := range []string{"import", "export"} {
			t.Run(checkout+"/"+direction, func(t *testing.T) {
				c := sourceTestConfig(t)
				c.Workspace.SourceSafeLinks = true
				c.Workspace.SourceExcludes = []string{"team/repo/data"}
				source, destination := c.Workspace.LocalRoot, c.Workspace.VMRoot
				if direction == "export" {
					source, destination = destination, source
				}
				writeSourceFile(t, source, "team/repo/AGENTS.md", "tracked agent instructions")
				writeSourceFile(t, source, "team/repo/assets/file", "tracked asset")
				for name, target := range map[string]string{"CLAUDE.md": "AGENTS.md", ".cursorrules": "AGENTS.md", "directory-link": "assets", "chain": "CLAUDE.md", "assets/relative-link": "../AGENTS.md"} {
					if err := os.Symlink(target, filepath.Join(source, "team/repo", name)); err != nil {
						t.Fatal(err)
					}
				}
				writeSourceFile(t, destination, "team/repo/data/keep", "receiver runtime")
				if err := os.Symlink("missing", filepath.Join(destination, "team/repo/data/python")); err != nil {
					t.Fatal(err)
				}
				planFile := filepath.Join(t.TempDir(), "plan.json")
				if err := PreviewSourceSync(context.Background(), c, direction, checkout, true, planFile, &bytes.Buffer{}); err != nil {
					t.Fatal(err)
				}
				if err := ApplySourceSync(context.Background(), c, planFile, &bytes.Buffer{}); err != nil {
					t.Fatal(err)
				}
				for name, target := range map[string]string{"CLAUDE.md": "AGENTS.md", ".cursorrules": "AGENTS.md", "directory-link": "assets", "chain": "CLAUDE.md", "assets/relative-link": "../AGENTS.md", "data/python": "missing"} {
					got, err := os.Readlink(filepath.Join(destination, "team/repo", name))
					if err != nil || got != target {
						t.Fatalf("%s not preserved as original link: %q %v", name, got, err)
					}
				}
				if got, err := os.ReadFile(filepath.Join(destination, "team/repo/data/keep")); err != nil || string(got) != "receiver runtime" {
					t.Fatal("excluded destination removed", string(got), err)
				}
				local, remote, err := sourceSyncStates(context.Background(), c, checkout, direction)
				if err != nil || local.Hash != remote.Hash {
					t.Fatal("safe-link transfer fingerprints differ", err, local.Hash, remote.Hash)
				}
				for _, entry := range local.Entries {
					if strings.Contains(entry.Path, "directory-link/") {
						t.Fatal("directory link was traversed", entry)
					}
					if strings.HasSuffix(entry.Path, "CLAUDE.md") && (entry.Kind != "symlink" || entry.Target != "AGENTS.md") {
						t.Fatal("manifest did not record link target", entry)
					}
				}
			})
		}
	}
}

func TestSourceSafeLinksRejectUnsafeTargets(t *testing.T) {
	for _, test := range []struct{ name, target, aliasTarget string }{
		{"absolute", "/etc/passwd", ""},
		{"outside", "../../outside", ""},
		{"escape-and-return", "../repo/AGENTS.md", ""},
		{"dangling", "missing", ""},
		{"self-cycle", "link", ""},
		{"chain-cycle", "alias", "link"},
		{"excluded-file", ".env.secret", ""},
		{"excluded-dir", "data/keep", ""},
		{"excluded-intermediate", "data/../AGENTS.md", ""},
		{"excluded-file-alias", "alias", ".env.secret"},
		{"excluded-dir-alias", "alias/keep", "data"},
		{"absolute-alias", "alias", "/etc/passwd"},
		{"dangling-alias", "alias", "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := sourceTestConfig(t)
			c.Workspace.SourceSafeLinks = true
			c.Workspace.SourceExcludes = []string{"team/repo/data"}
			writeSourceFile(t, c.Workspace.VMRoot, "team/repo/AGENTS.md", "safe")
			writeSourceFile(t, c.Workspace.VMRoot, "team/repo/.env.secret", "secret")
			writeSourceFile(t, c.Workspace.VMRoot, "team/repo/data/keep", "runtime")
			if test.aliasTarget != "" {
				if err := os.Symlink(test.aliasTarget, filepath.Join(c.Workspace.VMRoot, "team/repo/alias")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(test.target, filepath.Join(c.Workspace.VMRoot, "team/repo/link")); err != nil {
				t.Fatal(err)
			}
			if _, err := SourceFingerprint(context.Background(), c, "team/repo"); err == nil {
				t.Fatal("unsafe target accepted", test.target)
			}
		})
	}
}

func TestSourceSafeLinksPlansRejectModeAndTargetChanges(t *testing.T) {
	c := sourceTestConfig(t)
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/AGENTS.md", "safe")
	if err := os.Symlink("AGENTS.md", filepath.Join(c.Workspace.LocalRoot, "repo/CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := PreviewSourceSync(context.Background(), c, "import", "repo", false, filepath.Join(t.TempDir(), "default.json"), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "refuses symlinks") {
		t.Fatal("default accepted safe symlink", err)
	}
	c.Workspace.SourceSafeLinks = true
	plan := filepath.Join(t.TempDir(), "safe.json")
	if err := PreviewSourceSync(context.Background(), c, "import", "repo", false, plan, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	changed := c
	changed.Workspace.SourceSafeLinks = false
	if err := ApplySourceSync(context.Background(), changed, plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "mode changed") {
		t.Fatal("mode toggle accepted", err)
	}
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/other.md", "different target")
	// Re-preview before changing only the target text of the link.
	plan = filepath.Join(t.TempDir(), "target.json")
	if err := PreviewSourceSync(context.Background(), c, "import", "repo", false, plan, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(c.Workspace.LocalRoot, "repo/CLAUDE.md")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other.md", link); err != nil {
		t.Fatal(err)
	}
	if err := ApplySourceSync(context.Background(), c, plan, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "changed since preview") {
		t.Fatal("modified link target accepted", err)
	}
}

func TestSourceSafeLinksRsyncNeverFollowsChangedEscapingLink(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	c := sourceTestConfig(t)
	c.Workspace.SourceSafeLinks = true
	writeSourceFile(t, c.Workspace.LocalRoot, "repo/AGENTS.md", "safe")
	writeSourceFile(t, c.Workspace.LocalRoot, "outside", "must not copy")
	writeSourceFile(t, c.Workspace.VMRoot, "repo/existing", "receiver")
	link := filepath.Join(c.Workspace.LocalRoot, "repo/CLAUDE.md")
	if err := os.Symlink("AGENTS.md", link); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStateAtWithOptions(context.Background(), c.Workspace.LocalRoot, "repo", nil, true); err != nil {
		t.Fatal(err)
	}
	command, err := sourceSyncCommand(c, SourceSyncPlan{RelativePath: "repo", Direction: "import"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(command.Args, "--links") || !slices.Contains(command.Args, "--safe-links") || slices.Contains(command.Args, "--no-links") || slices.Contains(command.Args, "--copy-links") {
		t.Fatal("unsafe rsync link arguments", command)
	}
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("../outside", link); err != nil {
		t.Fatal(err)
	}
	if err = ExecuteContext(context.Background(), command, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(c.Workspace.VMRoot, "repo/CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("changed escaping symlink transferred or followed", err)
	}
	if got, err := os.ReadFile(filepath.Join(c.Workspace.VMRoot, "repo/AGENTS.md")); err != nil || string(got) != "safe" {
		t.Fatal("safe source did not transfer", string(got), err)
	}
}
