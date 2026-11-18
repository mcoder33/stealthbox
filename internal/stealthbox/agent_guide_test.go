package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentStartupGuide(t *testing.T) {
	cfg := bridgeConfig(t)
	cfg.Workspace.VMRoot = "/home/dev/projects"
	for _, agent := range []string{"claude", "codex"} {
		for _, project := range []string{"", "test"} {
			name := agent + "/workspace"
			if project != "" {
				name = agent + "/named"
			}
			t.Run(name, func(t *testing.T) {
				argv, err := AgentCommand(cfg, "/home/dev/config.json", project, agent, "local", []string{"resume"})
				if err != nil {
					t.Fatal(err)
				}
				var guide string
				if agent == "codex" {
					guide = codexStartupGuide(t, cfg, argv)
				} else {
					for i, arg := range argv[:len(argv)-1] {
						if arg == "--append-system-prompt" {
							guide = argv[i+1]
						}
					}
				}
				for _, want := range []string{
					"default runner=local",
					"only through the stealthbox runner_run MCP tool",
					"Docker, builds, test suites, dependency installation, full lint",
					"Do not fall back to shell commands or switch to the VM",
					"only through the stealthbox read_pasted_image MCP tool",
					"paste or upload the image again",
				} {
					if !strings.Contains(guide, want) {
						t.Errorf("startup guide lacks %q: %s", want, guide)
					}
				}
				if (project == "") != strings.Contains(guide, "workspace scope") {
					t.Errorf("unexpected startup scope: %s", guide)
				}
				if guide != Guide(project, "local") || argv[len(argv)-1] != "resume" {
					t.Fatal("startup guide or resume argument differs")
				}
			})
		}
	}
}

func codexStartupGuide(t *testing.T, cfg Config, argv []string) string {
	t.Helper()
	for _, name := range []string{"STEALTHBOX_PROJECT", "STEALTHBOX_RUNNER", "STEALTHBOX_SCOPE"} {
		found := false
		for _, arg := range argv {
			if strings.HasPrefix(arg, "developer_instructions=") {
				t.Fatal("startup replaces the user's Codex instructions")
			}
			raw, ok := strings.CutPrefix(arg, "mcp_servers.stealthbox.env."+name+"=")
			if !ok {
				continue
			}
			var value string
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				t.Fatal(err)
			}
			t.Setenv(name, value)
			found = true
		}
		if !found {
			t.Fatalf("Codex launch lacks MCP environment %s", name)
		}
	}
	var out bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if err := ServeMCP(context.Background(), cfg, in, &out); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			Instructions string
		}
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Result.Instructions
}
