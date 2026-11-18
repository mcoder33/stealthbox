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

func TestSetupCreatesResumableProfileBeforeSSH(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config.json")
	log := filepath.Join(root, "ssh.log")
	t.Setenv("STEALTHBOX_TEST_SSH_LOG", log)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$STEALTHBOX_TEST_SSH_LOG\"\nexit 255\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	setup := []string{"setup", "--host", "fixture-vm", "--runner", "vm", "--config", config}
	if err := run(ctx, append(setup, "--dry-run")); err != nil {
		t.Fatalf("dry run from missing profile: %v", err)
	}
	for _, path := range []string{config, log} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("dry run touched %s: %v", path, err)
		}
	}
	if err := run(ctx, setup); err == nil || !strings.Contains(err.Error(), "SSH probe failed") {
		t.Fatalf("first setup should reach the simulated SSH failure: %v", err)
	}
	c, err := stealthbox.Load(config)
	if err != nil || c.Workspace.Host != "fixture-vm" || c.Workspace.Runner != "vm" || c.Workspace.VMRoot != "~/Projects" {
		t.Fatalf("profile must retain the requested host and defaults: host=%q runner=%q root=%q err=%v", c.Workspace.Host, c.Workspace.Runner, c.Workspace.VMRoot, err)
	}
	if info, err := os.Stat(config); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("profile must be private: %v, %v", info, err)
	}
	if err := run(ctx, setup); err == nil || !strings.Contains(err.Error(), "SSH probe failed") {
		t.Fatalf("same alias should reach SSH again: %v", err)
	}
	if err := run(ctx, []string{"setup", "--host", "wrong-vm", "--config", config}); err == nil || !strings.Contains(err.Error(), "fixture-vm") {
		t.Fatalf("existing profile should refuse another alias: %v", err)
	}
	after, err := stealthbox.Load(config)
	if err != nil || after.Workspace.Host != c.Workspace.Host || after.Bridge.Token != c.Bridge.Token {
		t.Fatal("reruns changed the saved host or regenerated its token", err)
	}
	out, err := os.ReadFile(log)
	if err != nil || strings.Count(string(out), "fixture-vm\n") != 2 || strings.Contains(string(out), "wrong-vm") {
		t.Fatalf("unexpected SSH calls %q: %v", out, err)
	}
}

func TestInitRequiresExplicitAliasForWorkspace(t *testing.T) {
	ctx := context.Background()
	config := filepath.Join(t.TempDir(), "config.json")
	if err := run(ctx, []string{"init", "--config", config}); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("init must reject an implicit workspace host: %v", err)
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatalf("init without alias touched the config: %v", err)
	}
	if err := run(ctx, []string{"init", "--vm-root", "", "--config", config}); err != nil {
		t.Fatalf("explicit legacy initialization: %v", err)
	}
	c, err := stealthbox.Load(config)
	if err != nil || c.Workspace.VMRoot != "" || c.Workspace.Host != "" {
		t.Fatalf("legacy profile should have no implicit VM: root=%q host=%q err=%v", c.Workspace.VMRoot, c.Workspace.Host, err)
	}
}

func TestFreshProfilesUseSeparateBridgeSockets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	paths := []string{filepath.Join(root, "one.json"), filepath.Join(root, "two.json")}
	sockets := map[string]bool{}
	defaults, err := stealthbox.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if err := run(ctx, []string{"init", "--host", "fixture-vm", "--runner", "local", "--config", path}); err != nil {
			t.Fatal(err)
		}
		c, err := stealthbox.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Bridge.Enabled || c.Bridge.Socket == defaults.Bridge.Socket || sockets[c.Bridge.Socket] || len(c.Bridge.Socket) > 100 {
			t.Fatalf("custom profile did not get its own valid socket: %q", c.Bridge.Socket)
		}
		sockets[c.Bridge.Socket] = true
	}
}
