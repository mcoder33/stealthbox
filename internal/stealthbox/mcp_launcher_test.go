package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	lease, err := acquireManagedMCPDaemon(context.Background(), location, generation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
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
	repo := canonicalTemp(t)
	c.Projects["repo"] = Project{Source: Endpoint{Path: repo}, Runners: map[string]Endpoint{"vm": {Path: repo}}}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
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
	var instance managedMCPState
	for {
		if state, ready := readyManagedMCPInstance(ctx, path, "first"); ready && state.PID == os.Getpid() {
			instance = state
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
	fixture := mcpHTTPFixture{t: t, url: instance.url(), key: instance.Key, http: &http.Client{Timeout: 8 * time.Second}}
	defer fixture.http.CloseIdleConnections()
	bearer := fixture.bearer(MCPScope{Runner: "vm", Project: "repo"})
	fixture.exchange(fixture.request(ctx, http.MethodPost, mcpHTTPRetirePath, bearer, "", ""), http.StatusUnauthorized)
	session := fixture.initialize(bearer)
	markerDir := t.TempDir()
	started, release := filepath.Join(markerDir, "started"), filepath.Join(markerDir, "release")
	defer os.WriteFile(release, nil, 0600)
	command := "touch " + Quote(started) + "; while [ ! -f " + Quote(release) + " ]; do sleep .02; done; printf completed"
	params := map[string]any{"name": "runner_run", "arguments": map[string]any{"argv": []string{"sh", "-c", command}}}
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":` + JSONString(params) + `}`
	request := fixture.request(ctx, http.MethodPost, "/mcp", bearer, session, body)
	result := make(chan []byte, 1)
	go func() {
		response, err := fixture.http.Do(request)
		if err != nil {
			result <- nil
			return
		}
		defer response.Body.Close()
		content, _ := io.ReadAll(response.Body)
		result <- content
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case early := <-result:
			t.Fatalf("tool call returned before starting: %s", early)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if replacement, err := acquireManagedMCPDaemon(ctx, path, "next"); err == nil || !strings.Contains(err.Error(), "busy") {
		if replacement != nil {
			replacement.Close()
		}
		t.Fatalf("active tool call did not block replacement: %v", err)
	}
	if state, ready := readyManagedMCPInstance(ctx, path, "first"); !ready || state.Port != instance.Port {
		t.Fatal("busy replacement changed the live endpoint")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case output := <-result:
		if !strings.Contains(string(output), "completed") || strings.Contains(string(output), `"isError":true`) {
			t.Fatalf("live tool call did not finish after attempted replacement: %s", output)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
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

func TestManagedMCPStaleLauncherKeepsCurrentEndpoint(t *testing.T) {
	stale := sourceTestConfig(t)
	current := stale
	current.Workspace.SourceExcludes = []string{"fixture"}
	configPath := filepath.Join(t.TempDir(), "config.json")
	seedManagedMCP(t, current, configPath)
	_, path, err := managedMCPPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := readManagedMCPState(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := managedMCPInstance(ctx, stale, configPath); err == nil || !strings.Contains(err.Error(), "configuration changed") {
		t.Fatalf("stale launcher was not rejected before replacement: %v", err)
	}
	if after, ready := readyManagedMCPInstance(context.Background(), path, before.Generation); !ready || after.Port != before.Port {
		t.Fatal("stale launcher retired the current service")
	}
}

func TestManagedMCPNamedLaunchDefaultsKeepCurrentEndpoint(t *testing.T) {
	c := sourceTestConfig(t)
	repo := canonicalTemp(t)
	project := Project{Source: Endpoint{Path: repo}, Runners: map[string]Endpoint{"vm": {Path: repo}, "alternate": {Path: repo}}, Runner: "vm", Agent: "codex"}
	c.Projects["repo"] = project
	configPath := filepath.Join(t.TempDir(), "config.json")
	seedManagedMCP(t, c, configPath)
	_, path, err := managedMCPPaths(configPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := readManagedMCPState(path)
	if err != nil {
		t.Fatal(err)
	}
	project.Agent, project.Runner = "claude", "alternate"
	c.Projects["repo"] = project
	if err := Save(configPath, c); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	after, err := managedMCPInstance(ctx, c, configPath)
	if err != nil || after.Port != before.Port {
		t.Fatalf("named project defaults replaced the service: %v", err)
	}
	if p := c.Projects["repo"]; p.Agent != "claude" || p.Runner != "alternate" {
		t.Fatal("computing the service generation changed the caller's defaults")
	}
}
