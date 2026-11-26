package stealthbox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSerenaManifestRequiresWorktreeOnlyForWorkspace(t *testing.T) {
	m := serenaFixtureManifest("/bin/false")
	p, err := newSerenaPool(m, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	for _, workspace := range []bool{true, false} {
		encoded, err := json.Marshal(p.inventory(workspace))
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Tools []struct {
				InputSchema struct {
					Required   []string
					Properties map[string]any
				}
			}
		}
		if err = json.Unmarshal(encoded, &r); err != nil || len(r.Tools) != 1 {
			t.Fatal(err, string(encoded))
		}
		_, hasPath := r.Tools[0].InputSchema.Properties["worktree"]
		if hasPath != workspace {
			t.Fatalf("scope %v: %s", workspace, encoded)
		}
	}
}
func serenaFixtureManifest(cmd string) *SerenaManifest {
	return &SerenaManifest{Command: cmd, Tools: []json.RawMessage{json.RawMessage(`{"name":"find_symbol","inputSchema":{"type":"object","properties":{"name_path_pattern":{"type":"string"}},"required":["name_path_pattern"]}}`)}}
}

func serenaToolRequest(root, tool string, args map[string]any) json.RawMessage {
	if args == nil {
		args = map[string]any{}
	}
	if root != "" {
		args["worktree"] = root
	}
	data, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	return data
}

// A real stdio MCP child. Its cwd/pid in the reply makes process sharing,
// scope isolation, reaping and lazy restart observable without a language server.
func fixtureSerena(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	file := filepath.Join(d, "serena")
	script := `#!/usr/bin/env python3
import json,os,sys,time
if "--project" not in sys.argv: sys.exit(1)
root=sys.argv[sys.argv.index("--project")+1]
with open(os.path.join(root,"starts"),"a") as f: print(os.getpid(),file=f)
for line in sys.stdin:
    req=json.loads(line)
    if "id" not in req: continue
    if req["method"]=="initialize":
        result={"protocolVersion":req["params"]["protocolVersion"],"capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1"}}
    elif req["method"]=="tools/call":
        time.sleep(req["params"]["arguments"].get("delay",0))
        result={"content":[{"type":"text","text":root+"|"+str(os.getpid())}]}
    else: result={}
    print(json.dumps({"jsonrpc":"2.0","id":req["id"],"result":result}),flush=True)
`
	if err := os.WriteFile(file, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestSharedSerenaColdStartIdleAndReactivation(t *testing.T) {
	c := rootConfig(t)
	root := createCheckout(t, c, "a", "a")
	createCheckout(t, c, "b", "b")
	c.Workspace.Serena = serenaFixtureManifest(fixtureSerena(t))
	p, err := newSerenaPool(c.Workspace.Serena, 120*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	scope := MCPScope{Workspace: true, Runner: "vm"}
	call := func(path string, delay float64) (string, error) {
		res, err := p.call(context.Background(), c, scope, serenaToolRequest(path, "find_symbol", map[string]any{"delay": delay}))
		if err != nil {
			return "", err
		}
		return res.Content[0].(*mcp.TextContent).Text, nil
	}
	out := make(chan string, 2)
	for range 2 {
		go func() {
			text, err := call("a", 0.2)
			if err != nil {
				text = err.Error()
			}
			out <- text
		}()
	}
	other, err := call("b", 0.0)
	if err != nil {
		t.Fatal(err)
	}
	one, two := <-out, <-out
	if one != two || !strings.HasPrefix(one, root+"|") || one == other {
		t.Fatal(one, two, other)
	}
	pid, err := strconv.Atoi(strings.Split(one, "|")[1])
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(4 * time.Second)
	for syscall.Kill(pid, 0) != syscall.ESRCH && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("idle worker still alive", pid)
	}
	again, err := call("a", 0)
	if err != nil || again == one {
		t.Fatal(again, err)
	}
	log, err := os.ReadFile(filepath.Join(root, "starts"))
	if err != nil || len(strings.Fields(string(log))) != 2 {
		t.Fatal(string(log), err)
	}
}

func TestSharedSerenaHTTPScopesAndLazyDiscovery(t *testing.T) {
	c := rootConfig(t)
	a := createCheckout(t, c, "a", "a")
	b := createCheckout(t, c, "b", "b")
	c.Projects["first"] = Project{Source: Endpoint{Path: a}, Runners: map[string]Endpoint{"vm": {Path: a}}}
	c.Workspace.Serena = serenaFixtureManifest(fixtureSerena(t))
	f := startMCPHTTPFixture(t, c)
	for _, scope := range []MCPScope{{Workspace: true, Runner: "vm"}, {Project: "first", Runner: "vm"}} {
		token := f.bearer(scope)
		headers, _ := f.exchange(f.request(context.Background(), "POST", "/serena", token, "", mcpHTTPInitialize), 200)
		session := headers.Get(mcpSessionHeader)
		send := func(req mcpRequest) string {
			req.JSONRPC = "2.0"
			req.ID = json.RawMessage("2")
			buf, _ := json.Marshal(req)
			_, res := f.exchange(f.request(context.Background(), "POST", "/serena", token, session, string(buf)), 200)
			return res
		}
		if out := send(mcpRequest{Method: "tools/list"}); !strings.Contains(out, "find_symbol") {
			t.Fatal(out)
		}
		if _, err := os.Stat(filepath.Join(a, "starts")); !os.IsNotExist(err) {
			t.Fatal("discovery started worker")
		}
		if scope.Workspace {
			for _, path := range []string{"", "../outside"} {
				if s := send(mcpRequest{Method: "tools/call", Params: serenaToolRequest(path, "find_symbol", nil)}); !strings.Contains(s, `"isError":true`) {
					t.Fatal(s)
				}
			}
			if s := send(mcpRequest{Method: "tools/call", Params: serenaToolRequest("b", "find_symbol", nil)}); !strings.Contains(s, b+"|") {
				t.Fatal(s)
			}
		} else {
			if s := send(mcpRequest{Method: "tools/call", Params: serenaToolRequest("b", "find_symbol", nil)}); !strings.Contains(s, `"isError":true`) {
				t.Fatal(s)
			}
			if s := send(mcpRequest{Method: "tools/call", Params: serenaToolRequest("", "find_symbol", nil)}); !strings.Contains(s, a+"|") {
				t.Fatal(s)
			}
		}
		f.exchange(f.request(context.Background(), "POST", "/mcp", token, session, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`), 404)
	}
}

func TestInstalledSerenaWithTwoGitWorktrees(t *testing.T) {
	exe := os.Getenv("STEALTHBOX_TEST_SERENA")
	if exe == "" {
		t.Skip("set STEALTHBOX_TEST_SERENA to test the installed VM Serena binary")
	}
	t.Setenv("SERENA_HOME", t.TempDir())
	t.Setenv("SERENA_USAGE_REPORTING", "false")
	c := rootConfig(t)
	a := filepath.Join(c.Workspace.VMRoot, "a")
	b := filepath.Join(a, ".worktree", "branch-b")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	git("init", "-q", a)
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(a, "fixture.py"), "def Alpha():\n    return 1\n")
	git("-C", a, "add", ".")
	git("-C", a, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")
	git("-C", a, "worktree", "add", "--detach", "-q", b, "HEAD")
	write(filepath.Join(b, "fixture.py"), "def Beta():\n    return 2\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	m, err := DiscoverSerena(ctx, exe, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Workspace.Serena = m
	p, err := newSerenaPool(m, time.Second*2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	ask := func(root string) (string, error) {
		r, err := p.call(ctx, c, MCPScope{Workspace: true, Runner: "vm"}, serenaToolRequest(root, "get_symbols_overview", map[string]any{"relative_path": "fixture.py"}))
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(r)
		return string(data), nil
	}
	both := make(chan string, 2)
	for range 2 {
		go func() {
			s, err := ask(a)
			if err != nil {
				s = err.Error()
			}
			both <- s
		}()
	}
	for range 2 {
		if s := <-both; !strings.Contains(s, "Alpha") || strings.Contains(s, "Beta") {
			t.Fatal(s)
		}
	}
	first := 0
	for _, pid := range serenaChildren(t, os.Getpid()) {
		command, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
		if err == nil && strings.Contains(string(command), "start-mcp-server") && strings.Contains(string(command), "--project "+a) {
			if first != 0 {
				t.Fatal("multiple Serena processes for one worktree", first, pid)
			}
			first = pid
		}
	}
	if first == 0 {
		t.Fatal("no Serena process for first worktree")
	}
	lsPIDs := serenaChildren(t, first)
	if len(lsPIDs) == 0 {
		t.Fatal("no child processes under active Serena", first)
	}
	t.Logf("Serena PID %d; descendant PIDs %v", first, lsPIDs)
	if s, err := ask(b); err != nil || !strings.Contains(s, "Beta") || strings.Contains(s, "Alpha") {
		t.Fatal(s, err)
	}
	// Wait for native children to be collected, then use the same logical checkout again.
	deadline := time.Now().Add(15 * time.Second)
	for {
		p.mu.Lock()
		size := len(p.entries)
		p.mu.Unlock()
		if size == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle Serena workers were not reaped")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range append(lsPIDs, first) {
		for syscall.Kill(pid, 0) != syscall.ESRCH && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
		}
		if syscall.Kill(pid, 0) != syscall.ESRCH {
			t.Fatalf("idle process still exists: %d", pid)
		}
	}
	if s, err := ask(a); err != nil || !strings.Contains(s, "Alpha") {
		t.Fatal(s, err)
	}
}

func serenaChildren(t *testing.T, root int) []int {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		t.Fatal(err)
	}
	parents := map[int]int{}
	var candidates, found []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		parent, _ := strconv.Atoi(fields[1])
		parents[pid] = parent
		if pid != root {
			candidates = append(candidates, pid)
		}
	}
	for _, candidate := range candidates {
		for p := candidate; p > 1; p = parents[p] {
			if p == root {
				found = append(found, candidate)
				break
			}
		}
	}
	return found
}

func TestSharedSerenaCapacityAndWaitingCancellation(t *testing.T) {
	p, err := newSerenaPool(serenaFixtureManifest("/bin/false"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	ctx := context.Background()
	for i := range maxSerenaWorktrees {
		root := strconv.Itoa(i)
		e, err := p.acquire(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { <-e.gate; p.release(root, e) }()
	}
	if _, err = p.acquire(ctx, "overflow"); err == nil {
		t.Fatal("unbounded worktree count")
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err = p.acquire(wait, "0"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries["0"].refs != 1 {
		t.Fatal("abandoned request still holds worker")
	}
}

func TestManagedClientsSelectSharedSerenaOnlyWhenConfigured(t *testing.T) {
	c := rootConfig(t)
	for _, agent := range []string{"codex", "claude"} {
		argv, err := AgentCommand(c, testMCPURL, "", agent, "vm", nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(argv, " "), "/serena") {
			t.Fatal("registered before opt-in", argv)
		}
		opted := c
		opted.Workspace.Serena = serenaFixtureManifest("/bin/false")
		argv, err = AgentCommand(opted, testMCPURL, "", agent, "vm", nil)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(argv, " ")
		for _, want := range []string{strings.TrimSuffix(testMCPURL, "/mcp") + "/serena", testMCPURL, managedMCPTokenEnv} {
			if !strings.Contains(joined, want) {
				t.Fatal(agent, want, argv)
			}
		}
	}
}

func TestSharedSerenaCancellationReapsWorker(t *testing.T) {
	c := rootConfig(t)
	a := createCheckout(t, c, "a", "a")
	c.Workspace.Serena = serenaFixtureManifest(fixtureSerena(t))
	p, err := newSerenaPool(c.Workspace.Serena, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.call(ctx, c, MCPScope{Workspace: true, Runner: "vm"}, serenaToolRequest("a", "find_symbol", map[string]any{"delay": 20}))
		done <- err
	}()
	pid := waitForMCPHTTPProcess(t, filepath.Join(a, "starts"))
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled tool reported success")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("worker still owns canceled call")
	}
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("canceled worker still exists", pid)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) != 0 {
		t.Fatal("failed worker kept its worktree slot")
	}
}

func TestSharedSerenaRejectsInvalidCatalog(t *testing.T) {
	for _, raw := range []string{
		`{"name":"activate_project","inputSchema":{"type":"object","properties":{}}}`,
		`{"name":"find_symbol","inputSchema":{"type":"object","properties":{"worktree":{}}}}`,
		`{"name":"find_symbol","inputSchema":null}`,
	} {
		m := SerenaManifest{Command: "/bin/false", Tools: []json.RawMessage{json.RawMessage(raw)}}
		if _, err := newSerenaPool(&m, time.Second); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	m := serenaFixtureManifest("/bin/false")
	m.Tools = append(m.Tools, m.Tools[0])
	if m.validate() == nil {
		t.Fatal("accepted duplicate")
	}
}
