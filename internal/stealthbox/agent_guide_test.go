package stealthbox

import (
	"context"
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
				argv, err := AgentCommand(cfg, testMCPURL, project, agent, "local", []string{"resume"})
				if err != nil {
					t.Fatal(err)
				}
				var guide string
				if agent == "codex" {
					for _, arg := range argv {
						if strings.HasPrefix(arg, "developer_instructions=") {
							t.Fatal("startup replaces the user's Codex instructions")
						}
					}
					scope := MCPScope{Project: project, Runner: "local", Workspace: project == ""}
					response := dispatchMCP(context.Background(), cfg, scope, mcpRequest{Method: "initialize"}, "2025-11-25", 0)
					guide, _ = response.Result.(map[string]any)["instructions"].(string)
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
