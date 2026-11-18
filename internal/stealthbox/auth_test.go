package stealthbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentAuthStatus(t *testing.T) {
	cases := []struct {
		name   string
		agent  string
		reply  string
		status string
		ready  bool
	}{
		{"Codex signed in", "codex", "printf '%s\\n' 'Logged in using an API key: test-secret' >&2; exit 0", "signed in", true},
		{"Codex signed out", "codex", "printf '%s\\n' 'Not logged in' >&2; exit 1", "sign-in needed", false},
		{"Codex access failure", "codex", "printf '%s\\n' 'Error checking login status: test-secret' >&2; exit 1", "status unavailable", false},
		{"Claude signed in", "claude", "printf '%s\\n' '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"email\":\"test-secret\"}'; exit 0", "signed in", true},
		{"Claude signed out", "claude", "printf '%s\\n' '{\"loggedIn\":false,\"authMethod\":\"none\"}'; exit 1", "sign-in needed", false},
		{"Claude third party", "claude", "printf '%s\\n' '{\"loggedIn\":true,\"authMethod\":\"third_party\"}'; exit 0", "external provider", false},
		{"Claude changed format", "claude", "printf '%s\\n' 'test-secret'; exit 0", "status unavailable", false},
		{"Claude access failure", "claude", "printf '%s\\n' '{\"loggedIn\":false,\"authMethod\":\"none\"}'; exit 70", "status unavailable", false},
		{"native launcher exited 127", "codex", "exit 127", "status unavailable", false},
		{"missing executable", "codex", "printf %s stealthbox-native-cli-missing; exit 127", "not installed", false},
		{"SSH refused", "claude", "printf '%s\\n' 'test-secret' >&2; exit 255", "SSH connection failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("STEALTHBOX_SSH_CONFIG", filepath.Join(root, "ssh.config"))
			args := "login status"
			if tc.agent == "claude" {
				args = "auth status --json"
			}
			ssh := "#!/bin/sh\n" +
				"test \"$1 $2 $3 $4\" = '-T -o BatchMode=yes -F' && test \"$5\" = \"$STEALTHBOX_SSH_CONFIG\" && test \"$6\" = fixture-vm || exit 20\n" +
				"case \"$7\" in *'exec " + tc.agent + " " + args + "') ;; *) exit 21 ;; esac\n" +
				"if read -r unused; then exit 22; fi\n" + tc.reply + "\n"
			if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(ssh), 0700); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := AgentAuthStatus(context.Background(), Config{Workspace: WorkspaceConfig{Host: "fixture-vm"}}, tc.agent, &out)
			if (err == nil) != tc.ready || !strings.Contains(out.String(), tc.agent+": "+tc.status) {
				t.Fatalf("status %q, err=%v; want %q and ready=%t", out.String(), err, tc.status, tc.ready)
			}
			if strings.Contains(out.String(), "test-secret") || err != nil && strings.Contains(err.Error(), "test-secret") {
				t.Fatal("status surfaced raw credential or account data")
			}
		})
	}
}

func TestAuthStatusSkipsCustomLauncherAndChecksOtherAgent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	ssh := "#!/bin/sh\nwhile [ \"$#\" -gt 1 ]; do shift; done\n" +
		"case \"$1\" in *'exec codex login status') printf 'Logged in using ChatGPT\\n' >&2 ;; *) exit 24 ;; esac\n"
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(ssh), 0700); err != nil {
		t.Fatal(err)
	}
	c := Config{
		Workspace: WorkspaceConfig{Host: "fixture-vm"},
		Agents:    map[string][]string{"claude": {"env", "CLAUDE_CONFIG_DIR=test-secret", "claude"}},
	}
	var out bytes.Buffer
	err := AgentAuthStatus(context.Background(), c, "", &out)
	if err == nil || !strings.Contains(out.String(), "codex: signed in") ||
		!strings.Contains(out.String(), "claude: status unavailable (custom launcher") ||
		!strings.Contains(out.String(), "auth status --json") {
		t.Fatalf("result %q, err=%v; want one verified agent and one unsupported launcher", out.String(), err)
	}
	if strings.Contains(out.String(), "test-secret") || strings.Contains(err.Error(), "test-secret") {
		t.Fatal("status showed custom agent argv")
	}
}
