package stealthbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowIdentity(t *testing.T) {
	a := WindowName("project", "codex", "vm", "main", nil)
	for _, b := range []string{WindowName("project", "claude", "vm", "main", nil), WindowName("project", "codex", "mac", "main", nil), WindowName("project", "codex", "vm", "other", nil), WindowName("project", "codex", "vm", "main", []string{"resume", "--last"})} {
		if a == b {
			t.Fatal("window collision")
		}
	}
}
func TestRemoteConfig(t *testing.T) {
	c := bridgeConfig(t)
	c.Workspace.Host = "vm"
	c.Workspace.RemoteDir = "/home/dev/.stealthbox"
	c.Bridge.RemoteSocket = "/home/dev/.stealthbox/run/mac.sock"
	p := c.Projects["test"]
	p.Source.Host = "vm"
	p.Runners["vm"] = p.Source
	c.Projects["test"] = p
	r, e := remoteConfig(c)
	if e != nil {
		t.Fatal(e)
	}
	if r.Projects["test"].Source.Host != "" || !r.Projects["test"].Runners["mac"].Bridge || r.Bridge.Socket != c.Bridge.RemoteSocket {
		t.Fatal("wrong remote mapping")
	}
	if c.Projects["test"].Source.Host != "vm" {
		t.Fatal("input mutated")
	}
}
func TestConfigPermissions(t *testing.T) {
	c := bridgeConfig(t)
	p := filepath.Join(t.TempDir(), "config.json")
	if e := Save(p, c); e != nil {
		t.Fatal(e)
	}
	s, e := os.Stat(p)
	if e != nil || s.Mode().Perm() != 0600 {
		t.Fatal(s, e)
	}
	loaded, e := Load(p)
	if e != nil || loaded.Bridge.Token != c.Bridge.Token {
		t.Fatal(e)
	}
}
func TestMCPExecutionAndBinding(t *testing.T) {
	c := bridgeConfig(t)
	t.Setenv("STEALTHBOX_PROJECT", "test")
	t.Setenv("STEALTHBOX_RUNNER", "vm")
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"vm_exec","arguments":{"argv":["printf","mcp-ok"]}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"vm_exec","arguments":{"project":"other","argv":["true"]}}}
`
	var out bytes.Buffer
	if e := ServeMCP(context.Background(), c, strings.NewReader(in), &out); e != nil {
		t.Fatal(e)
	}
	s := out.String()
	if !strings.Contains(s, "mcp-ok") || !strings.Contains(s, "bound to project") || strings.Contains(s, `"name":"mac_exec"`) {
		t.Fatal(s)
	}
}
func TestAgentMCPWiring(t *testing.T) {
	c := bridgeConfig(t)
	for _, a := range []string{"codex", "claude"} {
		cmd, e := AgentCommand(c, "/home/user/config.json", "test", a, "mac", nil)
		if e != nil {
			t.Fatal(e)
		}
		s := strings.Join(cmd, " ")
		if !strings.Contains(s, "mcp") || !strings.Contains(s, "STEALTHBOX_PROJECT") || strings.Contains(s, c.Bridge.Token) {
			t.Fatal(s)
		}
	}
}
