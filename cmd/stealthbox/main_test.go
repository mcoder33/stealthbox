//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
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
