package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rootConfig(t *testing.T) Config {
	t.Helper()
	c, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.Workspace.Host = ""
	c.Workspace.VMRoot = filepath.Join(canonicalTemp(t), "Projects")
	c.Workspace.RunnerRoot = filepath.Join(canonicalTemp(t), "runners")
	c.Bridge.Enabled = true
	if err = os.MkdirAll(c.Workspace.VMRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err = EnsureWorkspaceIdentity(&c); err != nil {
		t.Fatal(err)
	}
	return c
}
func createCheckout(t *testing.T, c Config, path, contents string) string {
	t.Helper()
	root := filepath.Join(c.Workspace.VMRoot, path)
	if err := os.MkdirAll(root+"/.git", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/name", []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestWorkspaceResolverCheckoutIdentity(t *testing.T) {
	c := rootConfig(t)
	paths := []string{"a/api", "b/api", "a/api/nested", "worktree"}
	seen := map[string]bool{}
	for _, path := range paths {
		root := createCheckout(t, c, path, path)
		if path == "worktree" {
			if err := os.Remove(root + "/.git"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(root+"/.git", []byte("gitdir: ../main/.git/worktrees/test\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		resolved, err := ResolveProject(context.Background(), c, path, "")
		if err != nil {
			t.Fatal(err)
		}
		if seen[resolved.ID] || resolved.RelativePath != path || resolved.SourcePath != root || resolved.RunnerPath != filepath.Join(c.Workspace.RunnerRoot, resolved.ID, "tree") {
			t.Fatal(resolved)
		}
		seen[resolved.ID] = true
		again, err := ResolveProject(context.Background(), c, root, "")
		if err != nil || again.ID != resolved.ID {
			t.Fatal(again, err)
		}
	}
	sub := filepath.Join(c.Workspace.VMRoot, "b/api/tests")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProject(context.Background(), c, sub, "")
	if err != nil || resolved.RelativePath != "b/api" || resolved.RelativeCWD != "tests" {
		t.Fatal(resolved, err)
	}
}
func TestWorkspaceResolverRejectsEscapes(t *testing.T) {
	c := rootConfig(t)
	createCheckout(t, c, "repo", "repo")
	outside := canonicalTemp(t)
	if err := os.Symlink(outside, filepath.Join(c.Workspace.VMRoot, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside", c.Workspace.VMRoot + "-other/repo", "link", c.Workspace.VMRoot, "repo\nother"} {
		if resolved, err := ResolveProject(context.Background(), c, path, ""); err == nil {
			t.Fatalf("escape accepted %q: %+v", path, resolved)
		}
	}
	if _, err := ResolveProject(context.Background(), c, "repo", "../outside"); err == nil {
		t.Fatal("cwd traversal accepted")
	}
	c.Workspace.VMRoot = "~/Projects"
	if err := c.Validate(); err != nil {
		t.Fatal("home placeholder invalid", err)
	}
	if _, err := ResolveProject(context.Background(), c, "repo", ""); err == nil {
		t.Fatal("unresolved home root used")
	}
}
func TestWorkspaceConfigProtectsLocalSources(t *testing.T) {
	c := rootConfig(t)
	local := canonicalTemp(t)
	alias := filepath.Join(canonicalTemp(t), "source-alias")
	if err := os.Symlink(local, alias); err != nil {
		t.Fatal(err)
	}
	c.Workspace.LocalRoot = alias
	c.Workspace.RunnerRoot = filepath.Join(local, "runners")
	if err := c.Validate(); err == nil {
		t.Fatal("symlink alias source overlap accepted")
	}
	c.Workspace.RunnerRoot = filepath.Join(canonicalTemp(t), "runners")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Workspace.SyncTransport = "unexpected"
	if err := c.Validate(); err == nil {
		t.Fatal("invalid transport accepted")
	}
}
func TestWorkspaceMCPRoutesEachCheckoutAndPreservesBinding(t *testing.T) {
	c := rootConfig(t)
	repo := createCheckout(t, c, "foo", "workspace-foo")
	createCheckout(t, c, "a/api", "first-api")
	createCheckout(t, c, "b/api", "second-api")
	static := createCheckout(t, c, "other", "static-foo")
	c.Projects["foo"] = Project{Source: Endpoint{Path: static}, Runners: map[string]Endpoint{"vm": {Path: static}}}
	if err := os.MkdirAll(repo+"/tests", 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEALTHBOX_SCOPE", "workspace")
	t.Setenv("STEALTHBOX_PROJECT", "foo")
	if !strings.Contains(mcpGuide(c), "workspace scope") {
		t.Fatal("inherited binding leaked into workspace MCP guide")
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vm_exec","arguments":{"path":"foo","argv":["cat","name"]}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"vm_exec","arguments":{"path":"a/api","argv":["cat","name"]}}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"runner_run","arguments":{"path":"b/api","runner":"vm","argv":["cat","name"]}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"vm_exec","arguments":{"path":"foo/tests","argv":["cat","../name"]}}}
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"vm_exec","arguments":{"argv":["true"]}}}
{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"projects_list","arguments":{}}}
`
	marker := filepath.Join(canonicalTemp(t), "ambiguous-command-ran")
	ambiguous, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": "vm_exec", "arguments": map[string]any{"project": "foo", "path": "a/api", "argv": []string{"touch", marker}}}})
	input += string(ambiguous) + "\n"
	var out bytes.Buffer
	if err := ServeMCP(context.Background(), c, strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	responses := map[int]string{}
	responseErrors := map[int]bool{}
	dec := json.NewDecoder(&out)
	for dec.More() {
		var response struct {
			ID     int `json:"id"`
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := dec.Decode(&response); err != nil {
			t.Fatal(err)
		}
		responses[response.ID] = response.Result.Content[0].Text
		responseErrors[response.ID] = response.Result.IsError
	}
	for id, want := range map[int]string{1: "workspace-foo", 2: "first-api", 3: "second-api", 4: "workspace-foo", 5: "require a checkout path", 6: "a/api", 7: "choose either project or path"} {
		if !strings.Contains(responses[id], want) {
			t.Fatal(id, responses[id])
		}
	}
	if !responseErrors[7] {
		t.Fatal("ambiguousselectionwasnotmarkederror")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("ambiguousselectorcommandwasstarted", err)
	}
	if strings.Contains(responses[1], "static-foo") {
		t.Fatal("explicit workspace path captured by static alias")
	}
	t.Setenv("STEALTHBOX_SCOPE", "project")
	out.Reset()
	if err := ServeMCP(context.Background(), c, strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "bound to project foo") {
		t.Fatal(out.String())
	}
	for _, agent := range []string{"codex", "claude"} {
		args, err := AgentCommand(c, "/tmp/config.json", "foo", agent, "vm", nil)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		if agent == "codex" && !strings.Contains(joined, `STEALTHBOX_SCOPE="project"`) {
			t.Fatal(joined)
		}
		if agent == "claude" && !strings.Contains(joined, `"STEALTHBOX_SCOPE":"project"`) {
			t.Fatal(joined)
		}
	}
}
func TestVMExecutionRejectsCWDSymlinkEscape(t *testing.T) {
	c := rootConfig(t)
	root := createCheckout(t, c, "repo", "repo")
	outside := canonicalTemp(t)
	if err := os.Symlink(outside, root+"/escape"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), c, "repo", "vm", []string{"pwd"}, false, "escape", &out, &out); err == nil {
		t.Fatal("VM cwd symlink accepted", out.String())
	}
	if err := os.Mkdir(root+"/tests", 0700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(context.Background(), c, "repo", "vm", []string{"pwd"}, false, "tests", &out, &out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != root+"/tests" {
		t.Fatal(out.String())
	}
}
func TestWorkspaceDiscoverySkipsDependenciesAndState(t *testing.T) {
	c := rootConfig(t)
	createCheckout(t, c, "repo", "repo")
	for _, path := range []string{"repo/vendor/library", "repo/node_modules/library", "repo/.claude/state"} {
		createCheckout(t, c, path, "excluded")
	}
	createCheckout(t, c, "a/repo", "a-repo")
	paths, err := WorkspaceProjects(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "a/repo,repo" {
		t.Fatal(paths)
	}
}

func TestWorkspaceNonGitDirectoryUsesDirectChildCheckout(t *testing.T) {
	c := rootConfig(t)
	subdirectory := filepath.Join(c.Workspace.VMRoot, "plain", "src", "tests")
	if err := os.MkdirAll(subdirectory, 0700); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProject(context.Background(), c, "plain/src/tests", "")
	if err != nil || resolved.RelativePath != "plain" || resolved.RelativeCWD != filepath.Join("src", "tests") {
		t.Fatal(resolved, err)
	}
}

func TestWorkspaceCheckoutCannotBypassDefaultExclusions(t *testing.T) {
	c := rootConfig(t)
	for _, path := range []string{".codex/credentials", ".env-state/repo", "repo/vendor/library"} {
		root := createCheckout(t, c, path, "secret-fixture")
		if _, err := ResolveProject(context.Background(), c, root, ""); err == nil {
			t.Fatal("excludedancestorbecamecheckout", path)
		}
	}
}
