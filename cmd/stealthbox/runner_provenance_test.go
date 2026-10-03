//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mcoder33/stealthbox/internal/stealthbox"
)

func TestManagedDefaultRunnerReloadsConfig(t *testing.T) {
	c, err := stealthbox.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.Workspace.Host = ""
	c.Workspace.VMRoot = t.TempDir()
	c.Workspace.RunnerRoot = filepath.Join(t.TempDir(), "runners")
	c.Bridge.Enabled = true
	if err := os.Mkdir(filepath.Join(c.Workspace.VMRoot, "repo"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(c.Workspace.VMRoot, "repo/.git"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("STEALTHBOX_PROJECT", "")
	for _, initial := range []string{"vm", "mac"} {
		next := "mac"
		if initial == "mac" {
			next = "vm"
		}
		t.Setenv("STEALTHBOX_RUNNER", initial)
		c.Workspace.Runner = next
		if err := stealthbox.Save(config, c); err != nil {
			t.Fatal(err)
		}
		for _, command := range [][]string{{"agent", "codex"}, {"agent", "claude"}, {"run"}} {
			for _, source := range []string{"default", "captured", "explicit", ""} {
				t.Setenv("STEALTHBOX_RUNNER_SOURCE", source)
				args := append(append([]string{}, command...), "--config", config, "--path", "repo", "--dry-run", "--", "true")
				want := initial
				if source == "default" {
					want = next
				}
				if output := captureRunnerPlan(t, args); !strings.Contains(output, "runner="+want) {
					t.Fatalf("%v source=%q: %s", command, source, output)
				}
			}
			t.Setenv("STEALTHBOX_RUNNER_SOURCE", "default")
			args := append(append([]string{}, command...), "--config", config, "--path", "repo", "--runner", initial, "--dry-run", "--", "true")
			if output := captureRunnerPlan(t, args); !strings.Contains(output, "runner="+initial) {
				t.Fatal("explicit override lost", output)
			}
		}
	}
}

func captureRunnerPlan(t *testing.T, args []string) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = old }()
	if err := run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
