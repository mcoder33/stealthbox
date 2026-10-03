//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mcoder33/stealthbox/internal/stealthbox"
)

func TestListDiscoveryKeepsFullPathsAndDoesNotMutateConfig(t *testing.T) {
	c, _ := stealthbox.DefaultConfig()
	c.Workspace.Host = ""
	c.Workspace.VMRoot = t.TempDir()
	c.Workspace.RunnerRoot = filepath.Join(t.TempDir(), "runners")
	for _, path := range []string{"a/api", "b/api"} {
		if err := os.MkdirAll(filepath.Join(c.Workspace.VMRoot, path, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(t.TempDir(), "config.json")
	if err := stealthbox.Save(config, c); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(config)
	ordinary := captureRunnerPlan(t, []string{"list", "--config", config})
	wantOrdinary := "workspace\t\t" + c.Workspace.VMRoot + "\n"
	if ordinary != wantOrdinary {
		t.Fatal("ordinary list changed", ordinary)
	}
	discovery := captureRunnerPlan(t, []string{"list", "--config", config, "--discover"})
	if discovery != wantOrdinary+"checkout\ta/api\ncheckout\tb/api\n" {
		t.Fatal("same-basename paths are ambiguous", discovery)
	}
	after, _ := os.ReadFile(config)
	if !bytes.Equal(before, after) {
		t.Fatal("discovery changed the configuration")
	}
}

func TestCLIExecutionReceiptGoesToStderr(t *testing.T) {
	c, _ := stealthbox.DefaultConfig()
	root := t.TempDir()
	c.Projects = map[string]stealthbox.Project{"fixture": {Source: stealthbox.Endpoint{Path: root}, Runners: map[string]stealthbox.Endpoint{"vm": {Path: root}}}}
	config := filepath.Join(t.TempDir(), "config.json")
	if err := stealthbox.Save(config, c); err != nil {
		t.Fatal(err)
	}
	stdout, _ := os.CreateTemp(t.TempDir(), "stdout")
	stderr, _ := os.CreateTemp(t.TempDir(), "stderr")
	defer stdout.Close()
	defer stderr.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	if err := run(context.Background(), []string{"exec", "--config", config, "--project", "fixture", "--", "printf", "payload"}); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(stdout.Name())
	diagnostic, _ := os.ReadFile(stderr.Name())
	if string(out) != "payload" || strings.Count(string(diagnostic), "[execution] ") != 1 || !strings.Contains(string(diagnostic), `"sync":"not-requested"`) {
		t.Fatal("receipt missing or stdout changed", string(out), string(diagnostic))
	}
}
