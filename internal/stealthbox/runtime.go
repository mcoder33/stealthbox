package stealthbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

func ExecuteContext(ctx context.Context, c Command, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, c.Program, c.Args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
func Run(ctx context.Context, c Config, name, runner string, args []string, snapshot bool, cwd string, stdout, stderr io.Writer) error {
	p, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("unknown project %q", name)
	}
	if len(args) == 0 {
		return fmt.Errorf("provide command arguments")
	}
	dst, ok := p.Runners[runner]
	if !ok {
		return fmt.Errorf("unknown runner %q", runner)
	}
	if dst.Bridge {
		return RunBridge(ctx, c, name, args, snapshot, cwd, stdout, stderr)
	}
	if !snapshot {
		if runner == "mac" && !c.Bridge.AllowExec {
			return fmt.Errorf("direct Mac execution is disabled in local settings")
		}
		if cwd != "" {
			return fmt.Errorf("--cwd is available only with the Mac bridge")
		}
		return ExecuteContext(ctx, At(dst, "cd "+Quote(dst.Path)+" && "+shellArgs(args), false), nil, stdout, stderr)
	}
	if cwd != "" {
		return fmt.Errorf("--cwd is available only with direct Mac execution")
	}
	cmds, err := PlanRun(p, runner, args)
	if err != nil {
		return err
	}
	for _, cmd := range cmds {
		if err = ExecuteContext(ctx, cmd, nil, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}
func Output(ctx context.Context, cmd Command) ([]byte, error) {
	return exec.CommandContext(ctx, cmd.Program, cmd.Args...).Output()
}
func Guide(project, runner string) string {
	return fmt.Sprintf(`Stealth Box environment: project=%s, default runner=%s.
The agent and authoritative source code live on the VM. Use the stealthbox runner_run MCP tool (or "stealthbox run -- COMMAND ARGS") for Docker/test commands so the configured runner is honored. The Mac runner synchronizes uncommitted VM files into a disposable Mac checkout, executes there and returns output. Volumes and databases are not moved. Use mac_exec for explicit Mac tasks only when that tool is enabled. Mac commands execute with the Mac user's permissions, not in a security sandbox. Use vm_exec for explicit VM commands. Do not automatically retry a command after a disconnected result; verify whether it ran first.`, project, runner)
}
func AgentCommand(c Config, configPath, project, agent, runner string, extra []string) ([]string, error) {
	args, ok := c.Agents[agent]
	if !ok {
		switch agent {
		case "codex", "claude":
			args = []string{agent}
		case "shell":
			args = []string{"sh", "-l"}
		default:
			return nil, fmt.Errorf("agent %q is not configured", agent)
		}
	}
	args = append([]string{}, args...)
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if agent == "codex" {
		args = append(args, "-c", "mcp_servers.stealthbox.command="+JSONString(bin), "-c", "mcp_servers.stealthbox.args="+JSONString([]string{"mcp", "--config", configPath}), "-c", "mcp_servers.stealthbox.env.STEALTHBOX_PROJECT="+JSONString(project), "-c", "mcp_servers.stealthbox.env.STEALTHBOX_RUNNER="+JSONString(runner))
	}
	if agent == "claude" {
		args = append(args, "--append-system-prompt", Guide(project, runner), "--mcp-config", JSONString(map[string]any{"mcpServers": map[string]any{"stealthbox": map[string]any{"command": bin, "args": []string{"mcp", "--config", configPath}, "env": map[string]string{"STEALTHBOX_PROJECT": project, "STEALTHBOX_RUNNER": runner}}}}))
	}
	return append(args, extra...), nil
}
