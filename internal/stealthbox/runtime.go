package stealthbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func ExecuteContext(ctx context.Context, c Command, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, c.Program, c.Args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// ExecuteNoninteractiveContext owns the local command's process group. This
// does not guarantee cancellation of a command behind an SSH connection.
func ExecuteNoninteractiveContext(ctx context.Context, c Command, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, c.Program, c.Args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	// The context watcher may stop when the leader exits, before inherited
	// pipes close. WaitDelay only closes those pipes and can be hidden by an
	// ExitError, so clean up the remaining group on every failed execution.
	if cmd.Process != nil && (ctx.Err() != nil || err != nil) {
		_ = cmd.Cancel()
	}
	return err
}
func Run(ctx context.Context, c Config, name, runner string, args []string, snapshot bool, cwd string, stdout, stderr io.Writer) error {
	resolved, err := ResolveProject(ctx, c, name, cwd)
	if err != nil {
		return err
	}
	p := resolved.Project
	runner = normalizeRunner(runner)
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
	if !snapshot && runner == "mac" && !c.Bridge.AllowExec {
		return fmt.Errorf("direct local execution is disabled in local settings")
	}
	commandArgs := shellArgs(args)
	if resolved.StaticName == "" {
		commandArgs = "env COMPOSE_PROJECT_NAME=" + Quote(resolved.ID) + " " + commandArgs
	}
	execute := func() error {
		commandDirectory, err := executionDirectory(ctx, dst, resolved.RelativeCWD)
		if err != nil {
			return err
		}
		return ExecuteNoninteractiveContext(ctx, At(dst, "cd "+Quote(commandDirectory)+" && "+commandArgs, false), nil, stdout, stderr)
	}
	if !snapshot || (p.Source.Host == dst.Host && filepath.Clean(p.Source.Path) == filepath.Clean(dst.Path)) {
		return execute()
	}
	cmds, err := PlanRun(p, runner, args)
	if err != nil {
		return err
	}
	for _, cmd := range cmds[:len(cmds)-1] {
		if err = ExecuteNoninteractiveContext(ctx, cmd, nil, stdout, stderr); err != nil {
			return err
		}
	}
	return execute()

}

func Output(ctx context.Context, cmd Command) ([]byte, error) {
	out, err := exec.CommandContext(ctx, cmd.Program, cmd.Args...).Output()
	if failed, ok := err.(*exec.ExitError); ok && len(failed.Stderr) > 0 {
		return out, fmt.Errorf("%s: %w: %s", cmd.Program, err, failed.Stderr)
	}
	return out, err
}
func Guide(project, runner string) string {
	if project == "" {
		return fmt.Sprintf(`Stealth Box workspace scope, default runner=%s. One agent may work across repositories under the configured VM workspace root. Before editing or testing each repository, read its AGENTS.md and applicable parent/child instructions; switching repositories does not load those instructions automatically. Call projects_list to discover relative checkout paths. Every runner_run, vm_exec or mac_exec call must identify its checkout using path; never guess from a repeated basename. Run tests with runner_run; runner=local synchronizes only that checkout to a separate local runner and runner=vm executes on the VM. cwd is relative to the selected checkout. Git worktrees and same-named repositories have separate runner directories. Source changes remain on the VM; the local runner never overwrites the user's local source checkout. Commands execute with the selected user's permissions, not in a security sandbox. Do not retry after a disconnected result without checking whether it ran.`, runner)
	}
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
	if nativeAgentUtility(extra) {
		return append(args, extra...), nil
	}
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if agent == "codex" {
		args = append(args, "-c", "mcp_servers.stealthbox.command="+JSONString(bin), "-c", "mcp_servers.stealthbox.args="+JSONString([]string{"mcp", "--config", configPath}), "-c", "mcp_servers.stealthbox.env.STEALTHBOX_PROJECT="+JSONString(project), "-c", "mcp_servers.stealthbox.env.STEALTHBOX_RUNNER="+JSONString(runner))
	}
	if agent == "codex" {
		args = append(args, "-c", "mcp_servers.stealthbox.env.STEALTHBOX_SCOPE="+JSONString(agentScope(c, project)))
	}
	if agent == "claude" {
		args = append(args, "--append-system-prompt", Guide(project, runner), "--mcp-config", JSONString(map[string]any{"mcpServers": map[string]any{"stealthbox": map[string]any{"command": bin, "args": []string{"mcp", "--config", configPath}, "env": map[string]string{"STEALTHBOX_PROJECT": project, "STEALTHBOX_RUNNER": runner, "STEALTHBOX_SCOPE": agentScope(c, project)}}}}))
	}
	return append(args, extra...), nil
}

func nativeAgentUtility(extra []string) bool {
	if len(extra) == 0 {
		return false
	}
	switch extra[0] {
	case "--version", "-V", "--help", "-h", "help", "login", "logout", "auth", "doctor", "update", "mcp", "plugin", "completion":
		return true
	}
	return false
}

func agentScope(c Config, project string) string {
	if WorkspaceRootEnabled(c) && project == "" {
		return "workspace"
	}
	return "project"
}
