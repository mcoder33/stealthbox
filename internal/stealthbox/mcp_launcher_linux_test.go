package stealthbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagedMCPReplacedLinuxExecutableKeepsCurrentEndpoint(t *testing.T) {
	const readyEnv = "STEALTHBOX_TEST_REPLACED_EXECUTABLE_READY"
	if ready := os.Getenv(readyEnv); ready != "" {
		BuildVersion = "stale-executable-fixture"
		config := os.Getenv("STEALTHBOX_CONFIG")
		c, err := Load(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ready, nil, 0600); err != nil {
			t.Fatal(err)
		}
		waitForFixtureFile(t, ready+"-launch")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := managedMCPInstance(ctx, c, config); err == nil || !strings.Contains(err.Error(), "executable changed") {
			t.Fatalf("obsolete executing image was not detected: %v", err)
		}
		return
	}

	c := sourceTestConfig(t)
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	seedManagedMCP(t, c, configPath)
	_, statePath, err := managedMCPPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := readManagedMCPState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	binary, next := filepath.Join(root, "stealthbox-test"), filepath.Join(root, "next-release")
	image, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{binary, next} {
		if err := os.WriteFile(path, image, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	startup, err := lockManagedMCPDirectory(ctx, filepath.Dir(statePath))
	if err != nil {
		t.Fatal(err)
	}
	defer startup.Close()
	ready := filepath.Join(root, "old-process-ready")
	old := exec.CommandContext(ctx, binary, "-test.run=^TestManagedMCPReplacedLinuxExecutableKeepsCurrentEndpoint$")
	old.Env = append(os.Environ(), readyEnv+"="+ready, "STEALTHBOX_CONFIG="+configPath)
	var diagnostics bytes.Buffer
	old.Stdout, old.Stderr = &diagnostics, &diagnostics
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- old.Wait() }()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("old launcher exited before replacement: %v: %s", err, &diagnostics)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err := os.Rename(next, binary); err != nil {
		t.Fatal(err)
	}
	generation, err := managedMCPGeneration(c, binary)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation = generation
	if err := writePrivate(statePath, []byte(JSONString(state))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ready+"-launch", []byte("continue"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := startup.Close(); err != nil {
		t.Fatal(err)
	}
	resultErr := <-result
	if current, ready := readyManagedMCPInstance(ctx, statePath, generation); !ready || current.Port != state.Port {
		t.Fatalf("obsolete executing image retired the healthy replacement service: %s", &diagnostics)
	}
	if resultErr != nil {
		t.Fatalf("old launcher subprocess failed: %v: %s", resultErr, &diagnostics)
	}
}
