package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testMCPURL = "http://127.0.0.1:19001/mcp"

func seedManagedMCP(t *testing.T, c Config, configPath string) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	if err := Save(configPath, c); err != nil {
		t.Fatal(err)
	}
	fixture := startMCPHTTPFixture(t, c)
	_, location, err := managedMCPPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	generation, err := managedMCPGeneration(c, bin)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(fixture.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(managedMCPState{Generation: generation, Port: port, Key: fixture.key, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(location, state); err != nil {
		t.Fatal(err)
	}
}

func TestManagedMCPRejectsReusedPortWithoutLeakingBearer(t *testing.T) {
	key := bytes.Repeat([]byte{0x71}, 32)
	var previousChallenge string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenge := r.Header.Get(mcpHTTPChallengeHeader)
		if r.URL.Path != "/health" || r.Header.Get("Authorization") != "" || challenge == "" {
			t.Error("health sent a tool capability or omitted its challenge")
		}
		// Replay a real response from another request at this port.
		if previousChallenge != "" {
			w.Header().Set(mcpHTTPProofHeader, mcpHTTPHealthProof(key, previousChallenge))
		}
		previousChallenge = challenge
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	state := managedMCPState{Port: port, Key: key}
	for range 2 {
		if state.ready(context.Background()) {
			t.Fatal("accepted a 204 without proof for the current challenge")
		}
	}
}

func TestManagedMCPDropsLauncherScopeFromSharedEnv(t *testing.T) {
	original := []string{"PATH=/usr/bin", "STEALTHBOX_SSH_CONFIG=/tmp/fixture", "STEALTHBOX_RUNNER=local", "STEALTHBOX_SCOPE=workspace", managedMCPTokenEnv + "=private"}
	got := strings.Join(sharedMCPServiceEnv(original), "\n")
	if got != "PATH=/usr/bin\nSTEALTHBOX_SSH_CONFIG=/tmp/fixture" {
		t.Fatal("shared service environment did not keep the profile's base environment", got)
	}
}

func TestManagedAgentTimeoutCoversBridgeQueueAndOperation(t *testing.T) {
	c := bridgeConfig(t)
	c.Bridge.TimeoutSeconds = 240
	t.Setenv("MCP_TOOL_TIMEOUT", "")
	codex, err := AgentCommand(c, testMCPURL, "test", "codex", "mac", nil)
	if err != nil || !strings.Contains(strings.Join(codex, " "), "tool_timeout_sec=740") {
		t.Fatalf("Codex did not budget for queue and execution: %q, %v", codex, err)
	}
	claude, err := AgentCommand(c, testMCPURL, "test", "claude", "mac", nil)
	if err != nil || !strings.Contains(strings.Join(claude, " "), `"timeout":740000`) {
		t.Fatalf("Claude did not budget for queue and execution: %q, %v", claude, err)
	}
	t.Setenv("MCP_TOOL_TIMEOUT", "2000")
	claude, err = AgentCommand(c, testMCPURL, "test", "claude", "mac", nil)
	if err != nil || !strings.Contains(strings.Join(claude, " "), `"timeout":2000`) {
		t.Fatalf("Claude ignored an explicitly configured timeout: %q, %v", claude, err)
	}
}

func TestManagedMCPLifetimeLockProtectsEndpointReplacement(t *testing.T) {
	c := sourceTestConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "state", "endpoint.json")
	directory, err := lockManagedMCPDirectory(ctx, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	lease, err := acquireManagedMCPDaemon(ctx, path, "first")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := serveManagedMCP(ctx, c, path, "first", directory)
		lease.Close()
		done <- err
	}()
	for {
		if state, ready := readyManagedMCPInstance(ctx, path, "first"); ready && state.PID == os.Getpid() {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("MCP service exited during startup: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	// A same-generation requester cannot acquire a second lifetime lock.
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if second, err := acquireManagedMCPDaemon(short, path, "first"); err == nil {
		second.Close()
		t.Fatal("healthy daemon shared its lifetime lock with a second owner")
	}
	// A different generation retires the endpoint before obtaining the lock.
	replacement, err := acquireManagedMCPDaemon(ctx, path, "next")
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old endpoint remained published after retirement", err)
	}
}
