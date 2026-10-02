package stealthbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceBridgeRejectsUntrustedScopeAndPaths(t *testing.T) {
	c := rootConfig(t)
	root := createCheckout(t, c, "foo", "workspace")
	other := createCheckout(t, c, "other", "static")
	c.Projects["foo"] = Project{Source: Endpoint{Path: other}, Runners: map[string]Endpoint{"mac": {Path: filepath.Join(canonicalTemp(t), "static-runner")}}}
	handler := BridgeHandler(c)
	for _, request := range []RunRequest{
		{WorkspaceID: "unknown", Path: "foo", Args: []string{"true"}, Sync: true},
		{WorkspaceID: c.Workspace.ID, Path: "../escape", Args: []string{"true"}, Sync: true},
		{WorkspaceID: c.Workspace.ID, Path: root, Args: []string{"true"}, Sync: true},
	} {
		response := requestBridge(t, handler, c, request, bytes.NewReader(nil), c.Bridge.Token)
		if response.Code != 403 {
			t.Fatal(request, response.Code, response.Body.String())
		}
	}
	response := requestBridge(t, handler, c, RunRequest{WorkspaceID: c.Workspace.ID, Path: "foo", Args: []string{"cat", "name"}, Sync: true}, snapshotFor(t, root), c.Bridge.Token)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "workspace") {
		t.Fatal(response.Code, response.Body.String())
	}
	resolved, err := ResolveProject(context.Background(), c, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(resolved.RunnerPath + "/name"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(c.Projects["foo"].Runners["mac"].Path + "/name"); !os.IsNotExist(err) {
		t.Fatal("static runner received dynamic checkout")
	}
}

func TestWorkspaceBridgeLocksCheckoutAndAllowsOtherProject(t *testing.T) {
	c := rootConfig(t)
	c.Bridge.AllowExec = true
	first := createCheckout(t, c, "a/api", "first")
	second := createCheckout(t, c, "b/api", "second")
	a, err := ResolveProject(context.Background(), c, first, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolveProject(context.Background(), c, second, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := BridgeHandler(c)
	done := make(chan *httptest.ResponseRecorder, 1)
	meta, _ := json.Marshal(RunRequest{WorkspaceID: c.Workspace.ID, Path: "a/api", Sync: true, Args: []string{"sh", "-c", "printf ready > started; while [ ! -f release ]; do sleep 0.01; done"}})
	request := httptest.NewRequest("POST", "http://unix/run", snapshotFor(t, first))
	request.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	request.Header.Set("X-Stealthbox-Request", base64.RawURLEncoding.EncodeToString(meta))
	go func() { response := httptest.NewRecorder(); handler.ServeHTTP(response, request); done <- response }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(a.RunnerPath + "/started"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer os.WriteFile(a.RunnerPath+"/release", nil, 0600)
	same := requestBridge(t, handler, c, RunRequest{WorkspaceID: c.Workspace.ID, Path: "a/api", Args: []string{"true"}}, nil, c.Bridge.Token)
	if same.Code != 409 {
		t.Fatal(same.Code, same.Body.String())
	}
	different := requestBridge(t, handler, c, RunRequest{WorkspaceID: c.Workspace.ID, Path: "b/api", Sync: true, Args: []string{"cat", "name"}}, snapshotFor(t, second), c.Bridge.Token)
	if different.Code != 200 || !strings.Contains(different.Body.String(), "second") {
		t.Fatal(different.Code, different.Body.String())
	}
	query := url.Values{"workspace_id": {c.Workspace.ID}, "checkout": {"a/api"}, "path": {"started"}}
	fileRequest := httptest.NewRequest("GET", "http://unix/file?"+query.Encode(), nil)
	fileRequest.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	busy := httptest.NewRecorder()
	handler.ServeHTTP(busy, fileRequest)
	if busy.Code != 409 {
		t.Fatal("artifact escaped command lock", busy.Code)
	}
	if err = os.WriteFile(a.RunnerPath+"/release", nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-done:
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first command remained blocked")
	}
	artifact := httptest.NewRecorder()
	handler.ServeHTTP(artifact, fileRequest)
	if artifact.Code != 200 || artifact.Body.String() != "ready" {
		t.Fatal(artifact.Code, artifact.Body.String())
	}
	if a.ID == b.ID || a.RunnerPath == b.RunnerPath {
		t.Fatal("same-basename runners collided")
	}
}

func TestWorkspaceUnixBridgeSyncsWholeCheckoutAndCommandSubdirectory(t *testing.T) {
	c := rootConfig(t)
	c.Bridge.Socket = filepath.Join(canonicalTemp(t), "workspace.sock")
	if len(c.Bridge.Socket) > 100 {
		t.Skip("socket temporary path too long")
	}
	root := createCheckout(t, c, "repo", "checkout-root")
	if err := os.Mkdir(root+"/tests", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/tests/test", []byte("test-subdir"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- ServeBridge(ctx, c, ready) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("bridge not ready")
	}
	var stdout, stderr bytes.Buffer
	if err := Run(ctx, c, root+"/tests", "local", []string{"sh", "-c", "cat ../name; printf ':'; cat test; printf ':'; printf '%s' \"$COMPOSE_PROJECT_NAME\""}, true, "", &stdout, &stderr); err != nil {
		t.Fatal(err, stderr.String())
	}
	resolved, err := ResolveProject(ctx, c, "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "checkout-root:test-subdir:"+resolved.ID {
		t.Fatal(stdout.String())
	}
	if err := Fetch(ctx, c, "repo", "tests/test", filepath.Join(canonicalTemp(t), "artifact")); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, c, "repo", "local", []string{"true"}, false, "", io.Discard, io.Discard); err == nil {
		t.Fatal("direct local execution bypassed policy")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestArchiveAndRsyncDefaultAgentStateExclusions(t *testing.T) {
	src := canonicalTemp(t)
	for _, name := range []string{".envrc", ".claude/settings.json", ".codex/auth.json", ".agents/credentials", ".opencode/state", ".kimi-code/state"} {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("secret-fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(src+"/code", []byte("public"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := snapshotFor(t, src)
	dst := filepath.Join(canonicalTemp(t), "runner")
	if err := SyncSnapshot(dst, archive, 1<<20); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatal("agent state transferred", entries)
	}

}
