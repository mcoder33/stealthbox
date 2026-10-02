//go:build !windows

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mcoder33/stealthbox/internal/stealthbox"
)

func TestExplicitPathOverridesInheritedProject(t *testing.T) {
	c, err := stealthbox.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	c.Workspace.Host = ""
	c.Workspace.VMRoot = filepath.Join(root, "Projects")
	c.Workspace.RunnerRoot = filepath.Join(root, "runners")
	if err = os.MkdirAll(filepath.Join(c.Workspace.VMRoot, "beta"), 0700); err != nil {
		t.Fatal(err)
	}
	c.Projects["bound"] = stealthbox.Project{Source: stealthbox.Endpoint{Path: filepath.Join(root, "legacy")}, Runners: map[string]stealthbox.Endpoint{"vm": {Path: filepath.Join(root, "legacy")}}}
	c.Projects["beta"] = c.Projects["bound"]
	config := filepath.Join(root, "config.json")
	if err = stealthbox.Save(config, c); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEALTHBOX_PROJECT", "bound")
	if err = run(context.Background(), []string{"run", "--config", config, "--path", "beta", "--runner", "vm", "--dry-run", "--", "true"}); err != nil {
		t.Fatal(err)
	}
	if err = run(context.Background(), []string{"run", "--config", config, "--project", "bound", "--path", "beta", "--dry-run", "--", "true"}); err == nil {
		t.Fatal("accepted explicitly conflicting flags")
	}
}

func TestSourceFingerprintExplicitExcludesOverrideRemoteConfig(t *testing.T) {
	c, err := stealthbox.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.Workspace.Host = ""
	c.Workspace.VMRoot = t.TempDir()
	c.Workspace.RunnerRoot = filepath.Join(t.TempDir(), "runners")
	c.Workspace.SourceExcludes = []string{"team/repo"}
	if err = os.MkdirAll(filepath.Join(c.Workspace.VMRoot, "team/repo/data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("missing-python", filepath.Join(c.Workspace.VMRoot, "team/repo/data/python")); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.json")
	if err = stealthbox.Save(config, c); err != nil {
		t.Fatal(err)
	}
	// Observe the real internal CLI: a stale deployed exclusion must be replaced.
	stdout := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	defer func() { os.Stdout = stdout; read.Close(); write.Close() }()
	args := []string{"source", "fingerprint", "--config", config, "--path", "team/repo", "--source-excludes", `["team/repo/data"]`}
	if err = run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	write.Close()
	output, err := io.ReadAll(read)
	if err != nil || !strings.Contains(string(output), `"hash"`) || strings.Contains(string(output), "python") {
		t.Fatal("bad remote fingerprint", string(output), err)
	}
	args[len(args)-1] = `[]`
	if err = run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "refuses symlinks") {
		t.Fatal("empty explicit exclusions did not replace remote config", err)
	}
	args[len(args)-1] = `["team/*"]`
	if err = run(context.Background(), args); err == nil {
		t.Fatal("internal CLI accepted glob exclusion")
	}
}

func TestSourceInternalSafeLinksOverridesRemoteConfig(t *testing.T) {
	c, err := stealthbox.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.Workspace.Host = ""
	c.Workspace.VMRoot = t.TempDir()
	c.Workspace.RunnerRoot = filepath.Join(t.TempDir(), "runners")
	c.Workspace.SourceSafeLinks = true
	repo := filepath.Join(c.Workspace.VMRoot, "repo")
	if err = os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("tracked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("AGENTS.md", filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.json")
	if err = stealthbox.Save(config, c); err != nil {
		t.Fatal(err)
	}
	args := []string{"source", "fingerprint", "--config", config, "--path", "repo", "--source-safe-links=false"}
	if err = run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "refuses symlinks") {
		t.Fatal("false override did not disable stale remote mode", err)
	}
	state, err := stealthbox.SourceFingerprint(context.Background(), c, "repo")
	if err != nil {
		t.Fatal(err)
	}
	c.Workspace.SourceSafeLinks = false
	if err = stealthbox.Save(config, c); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "fingerprint-*")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = stdout; output.Close() }()
	args[len(args)-1] = "--source-safe-links=true"
	if err = run(context.Background(), args); err != nil {
		t.Fatal("true override did not enable safe remote mode", err)
	}
	data, err := os.ReadFile(output.Name())
	if err != nil || !strings.Contains(string(data), `"target":"AGENTS.md"`) {
		t.Fatal("remote manifest omitted target", string(data), err)
	}
	args[1] = "prepare"
	args = append(args, "--expected-hash", state.Hash)
	if err = run(context.Background(), args); err != nil {
		t.Fatal("prepare did not use explicit safe mode", err)
	}
}

func TestAgentCommandNameBeforeFlags(t *testing.T) {
	c, err := stealthbox.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	c.Workspace.Host = ""
	c.Workspace.VMRoot = root
	c.Workspace.RunnerRoot = filepath.Join(t.TempDir(), "runner")
	config := filepath.Join(t.TempDir(), "config.json")
	if err = stealthbox.Save(config, c); err != nil {
		t.Fatal(err)
	}
	if err = run(context.Background(), []string{"agent", "codex", "--config", config, "--dry-run", "--", "resume", "--last"}); err != nil {
		t.Fatal(err)
	}
}
