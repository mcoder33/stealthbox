package stealthbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type workspaceLaunch struct {
	directory, label, binding, scope, agent, runner string
}

func runnerAlias(runner string) string {
	switch runner {
	case "local":
		return "mac"
	case "remote":
		return "vm"
	}
	return runner
}

func resolveWorkspaceLaunch(ctx context.Context, c Config, selector, agent, runner string) (workspaceLaunch, error) {
	if p, ok := c.Projects[selector]; ok {
		if p.Source.Host != "" {
			return workspaceLaunch{}, fmt.Errorf("workspace command runs on the VM; use connect on the Mac")
		}
		agent, runner = Defaults(p, agent, runnerAlias(runner))
		if _, ok := p.Runners[runner]; !ok {
			return workspaceLaunch{}, fmt.Errorf("unknown runner %q", runner)
		}
		return workspaceLaunch{directory: p.Source.Path, label: selector, binding: selector, agent: agent, runner: runner}, nil
	}
	if !WorkspaceRootEnabled(c) {
		return workspaceLaunch{}, fmt.Errorf("unknown project %q", selector)
	}
	if err := EnsureWorkspaceIdentity(&c); err != nil {
		return workspaceLaunch{}, err
	}
	if c.Workspace.Host != "" {
		return workspaceLaunch{}, fmt.Errorf("workspace command runs on the VM; use connect on the local machine")
	}
	root, err := filepath.EvalSymlinks(c.Workspace.VMRoot)
	if err != nil {
		return workspaceLaunch{}, fmt.Errorf("workspace root: %w", err)
	}
	directory, label := root, "projects"
	if selector != "" && filepath.Clean(selector) != filepath.Clean(c.Workspace.VMRoot) && filepath.Clean(selector) != root {
		resolved, err := ResolveProject(ctx, c, selector, "")
		if err != nil {
			return workspaceLaunch{}, err
		}
		directory = filepath.Join(resolved.SourcePath, resolved.RelativeCWD)
		label = resolved.DisplayName + "-" + resolved.ID
	}
	if !within(root, directory) {
		return workspaceLaunch{}, fmt.Errorf("agent path is outside workspace root")
	}
	if agent == "" {
		agent = "shell"
	}
	if runner == "" {
		runner = "vm"
	}
	runner = runnerAlias(runner)
	if runner != "vm" && runner != "mac" {
		return workspaceLaunch{}, fmt.Errorf("root workspace runner must be local or vm")
	}
	if runner == "mac" && !c.Bridge.Enabled {
		return workspaceLaunch{}, fmt.Errorf("enable the local bridge before selecting the local runner")
	}
	// Root identity keeps separate configured workspaces out of the same tmux window.
	hash := sha256.Sum256([]byte(c.Workspace.ID + ":" + label))
	label = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, label)
	if len(label) > 48 {
		label = label[:48]
	}
	return workspaceLaunch{directory: directory, label: fmt.Sprintf("%s-%x", label, hash[:4]), scope: "workspace", agent: agent, runner: runner}, nil
}

func workspaceLaunchScript(c Config, configPath string, launch workspaceLaunch, extra []string) (string, error) {
	cmd, err := AgentCommand(c, configPath, launch.binding, launch.agent, launch.runner, extra)
	if err != nil {
		return "", err
	}
	if launch.scope == "workspace" && launch.agent == "shell" && c.Workspace.ShellIntegration {
		cmd, err = managedShellCommand(c, extra)
		if err != nil {
			return "", err
		}
	}
	env := "export PATH=" + Quote(filepath.Dir(mustExecutable())) + ":\"$PATH\" STEALTHBOX_CONFIG=" + Quote(configPath) + " STEALTHBOX_PROJECT=" + Quote(launch.binding) + " STEALTHBOX_SCOPE=" + Quote(launch.scope) + " STEALTHBOX_RUNNER=" + Quote(launch.runner) + " STEALTHBOX_AGENT=" + Quote(launch.agent) + " STEALTHBOX_VM_ROOT=" + Quote(c.Workspace.VMRoot)
	return env + "; exec " + shellArgs(cmd), nil
}

// LaunchAgent is used inside the managed VM shell. It does not open another tmux.
func LaunchAgent(ctx context.Context, c Config, configPath, selector, agent, runner string, extra []string, stdout, stderr io.Writer) error {
	if selector == "" && WorkspaceRootEnabled(c) {
		var err error
		selector, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	return PlainRemote(ctx, c, configPath, selector, agent, runner, extra, stdout, stderr)
}

func shellAgentFunctions() string {
	return `
# These functions exist only in the Stealth Box shell. exec launches the actual executable.
unalias codex claude 2>/dev/null || true
codex() { command stealthbox agent codex --runner "${STEALTHBOX_RUNNER:-vm}" -- "$@"; }
claude() { command stealthbox agent claude --runner "${STEALTHBOX_RUNNER:-vm}" -- "$@"; }
`
}

func managedShellCommand(c Config, extra []string) ([]string, error) {
	if c.Workspace.RemoteDir == "" {
		return nil, fmt.Errorf("shell integration needs remote_dir; run setup first")
	}
	dir := filepath.Join(c.Workspace.RemoteDir, "shell")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	functions := shellAgentFunctions()
	switch filepath.Base(shell) {
	case "bash":
		file := filepath.Join(dir, "bashrc")
		content := "[ ! -f \"$HOME/.bashrc\" ] || . \"$HOME/.bashrc\"\n" + functions
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			return nil, err
		}
		return append([]string{shell, "--rcfile", file, "-i"}, extra...), nil
	case "zsh":
		original := os.Getenv("ZDOTDIR")
		if original == "" {
			var err error
			original, err = os.UserHomeDir()
			if err != nil {
				return nil, err
			}
		}
		// An agent can spawn another shell; avoid sourcing our own startup file recursively.
		if filepath.Clean(original) == filepath.Clean(dir) {
			original = os.Getenv("STEALTHBOX_ORIGINAL_ZDOTDIR")
		}
		if original == "" {
			return nil, fmt.Errorf("cannot find original zsh startup directory")
		}
		for _, name := range []string{".zshenv", ".zprofile", ".zshrc", ".zlogin"} {
			// Source the user's files with their original ZDOTDIR, then pin ours for the next startup phase.
			content := "ZDOTDIR=\"$STEALTHBOX_ORIGINAL_ZDOTDIR\"\n[ ! -f \"$ZDOTDIR/" + name + "\" ] || . \"$ZDOTDIR/" + name + "\"\nexport STEALTHBOX_ORIGINAL_ZDOTDIR=\"${ZDOTDIR:-$HOME}\"\nexport ZDOTDIR=" + Quote(dir) + "\n"
			if name == ".zshrc" {
				content += functions
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
				return nil, err
			}
		}
		return append([]string{"env", "ZDOTDIR=" + dir, "STEALTHBOX_ORIGINAL_ZDOTDIR=" + original, shell, "-l"}, extra...), nil
	default:
		file := filepath.Join(dir, "shrc")
		content := "[ ! -f \"$HOME/.profile\" ] || . \"$HOME/.profile\"\n" + functions
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			return nil, err
		}
		return append([]string{"env", "ENV=" + file, shell, "-i"}, extra...), nil
	}
}

func expandLocalPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")), nil
	}
	return path, nil
}

func NormalizeLocalWorkspaceRoots(c *Config) error {
	var err error
	if c.Workspace.LocalRoot, err = expandLocalPath(c.Workspace.LocalRoot); err != nil {
		return err
	}
	if c.Workspace.RunnerRoot, err = expandLocalPath(c.Workspace.RunnerRoot); err != nil {
		return err
	}
	return EnsureWorkspaceIdentity(c)
}
