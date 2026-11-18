//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mcoder33/stealthbox/internal/stealthbox"
)

func newWorkspaceProfile(path, host, vmRoot string, vmRootSet, enableMac, allowExec bool, apply func(*stealthbox.Config) error) (stealthbox.Config, error) {
	if host == "" && !(vmRootSet && vmRoot == "") {
		return stealthbox.Config{}, fmt.Errorf("provide your SSH alias with --host")
	}
	c, err := stealthbox.DefaultConfig()
	if err != nil {
		return c, err
	}
	c.Bridge.Socket, err = profileBridgeSocket(path, c.Bridge.Socket)
	if err != nil {
		return c, err
	}
	c.Workspace.Host = host
	c.Workspace.VMRoot = "~/Projects"
	if err := apply(&c); err != nil {
		return c, err
	}
	c.Bridge.Enabled = c.Bridge.Enabled || enableMac
	c.Bridge.AllowExec = allowExec
	return c, c.Validate()
}

// Keep the established default socket, and use short names for custom profiles
// even when the configuration file itself has a long path.
func profileBridgeSocket(path, defaultSocket string) (string, error) {
	profile, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(defaultSocket)
	if profile == filepath.Join(dir, "config.json") {
		return defaultSocket, nil
	}
	digest := sha256.Sum256([]byte(profile))
	name := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10])
	// Config.Validate allows at most 100 bytes for Unix-domain socket paths.
	width := min(len(name), 100-len(dir)-1)
	if width < len("mac.sock") {
		return "", fmt.Errorf("local bridge directory is too long for a Unix socket: %q", dir)
	}
	return filepath.Join(dir, name[:width]), nil
}

func setupProfile(path, host string, newProfile func() (stealthbox.Config, error), apply func(*stealthbox.Config) error) (stealthbox.Config, bool, error) {
	if host == "" {
		return stealthbox.Config{}, false, fmt.Errorf("provide your SSH alias with --host")
	}
	c, err := stealthbox.Load(path)
	if os.IsNotExist(err) {
		c, err = newProfile()
		return c, true, err
	}
	if err != nil {
		return c, false, err
	}
	if c.Workspace.Host != host {
		return c, false, fmt.Errorf("profile %s already uses SSH alias %q; choose another --config path", path, c.Workspace.Host)
	}
	if err := apply(&c); err != nil {
		return c, false, err
	}
	return c, false, c.Validate()
}

func setupFromAlias(ctx context.Context, path, host, binary string, dry bool, newProfile func() (stealthbox.Config, error), apply func(*stealthbox.Config) error) error {
	c, firstSetup, err := setupProfile(path, host, newProfile, apply)
	if err != nil {
		return err
	}
	if dry {
		fmt.Printf("Would prepare %s using profile %s.\n", host, path)
		return nil
	}
	if firstSetup {
		if err := stealthbox.CreateConfig(path, c); err != nil {
			return err
		}
		fmt.Println("Created", path, "(mode 0600).")
	}
	if firstSetup || binary != "" {
		err = stealthbox.Setup(ctx, &c, path, binary, os.Stdout)
	} else {
		err = stealthbox.EnsureReady(ctx, &c, path, binary, os.Stdout)
	}
	if err != nil {
		return err
	}
	profileFlag := " --config " + stealthbox.Quote(path)
	fmt.Printf("VM prepared. Check agent sign-in:\n  stealthbox auth-status%s\nOpen the workspace in your terminal:\n  stealthbox%s\n", profileFlag, profileFlag)
	if c.Bridge.Enabled {
		fmt.Printf("Opening the workspace starts the local runner bridge. For a headless check:\n  stealthbox bridge-start%s\n  stealthbox doctor%s\n", profileFlag, profileFlag)
	}
	return nil
}
