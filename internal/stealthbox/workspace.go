package stealthbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func JSONString(v any) string { b, _ := json.Marshal(v); return string(b) }
func WindowName(project, agent, runner, slot string, extra []string) string {
	base := project + "-" + agent + "-" + runner + "-" + slot
	if len(extra) > 0 {
		h := sha256.Sum256([]byte(shellArgs(extra)))
		base += fmt.Sprintf("-%x", h[:4])
	}
	return base
}
func WorkspaceRemote(ctx context.Context, c Config, configPath, project, agent, runner, slot string, extra []string, attach bool, stdout, stderr io.Writer) error {
	launch, err := resolveWorkspaceLaunch(ctx, c, project, agent, runner)
	if err != nil {
		return err
	}
	agent, runner = launch.agent, launch.runner
	if slot == "" {
		slot = "main"
	}
	if !nameRE.MatchString(slot) {
		return fmt.Errorf("invalid session slot")
	}
	if !nameRE.MatchString(agent) {
		return fmt.Errorf("invalid agent")
	}
	name := c.Workspace.Name
	if name == "" {
		name = "stealthbox"
	}
	window := WindowName(launch.label, agent, runner, slot, extra)
	environment, err := workspaceLaunchScript(c, configPath, launch, extra)
	if err != nil {
		return err
	}
	tmuxArgs := []string{"-L", "stealthbox"}
	theme := filepath.Join(c.Workspace.RemoteDir, "tmux.conf")
	if _, e := os.Stat(theme); e == nil {
		tmuxArgs = append(tmuxArgs, "-f", theme)
	}
	invoke := func(args ...string) ([]byte, error) {
		return Output(ctx, Command{"tmux", append(append([]string{}, tmuxArgs...), args...)})
	}
	if _, err = invoke("has-session", "-t", name); err != nil {
		if _, err = invoke("new-session", "-d", "-s", name, "-n", window, "-c", launch.directory, environment); err != nil {
			return fmt.Errorf("create tmux workspace: %w", err)
		}
	} else {
		b, e := invoke("list-windows", "-t", name, "-F", "#{window_name}")
		if e != nil {
			return e
		}
		found := false
		for _, n := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if n == window {
				found = true
			}
		}
		if !found {
			if _, err = invoke("new-window", "-t", name, "-n", window, "-c", launch.directory, environment); err != nil {
				return err
			}
		}
	}
	if launch.scope == "workspace" {
		// Session-scoped defaults also reach ordinary new windows and split panes.
		defaults := launch
		defaults.agent, defaults.binding = "shell", ""
		defaultScript, e := workspaceLaunchScript(c, configPath, defaults, nil)
		if e != nil {
			return e
		}
		for key, value := range map[string]string{"STEALTHBOX_CONFIG": configPath, "STEALTHBOX_SCOPE": "workspace", "STEALTHBOX_PROJECT": "", "STEALTHBOX_RUNNER": runner, "STEALTHBOX_VM_ROOT": c.Workspace.VMRoot, "PATH": filepath.Dir(mustExecutable()) + ":" + os.Getenv("PATH")} {
			if _, e = invoke("set-environment", "-t", name, key, value); e != nil {
				return e
			}
		}
		if _, e = invoke("set-option", "-t", name, "default-command", defaultScript); e != nil {
			return e
		}
	}
	if _, err = invoke("select-window", "-t", name+":"+window); err != nil {
		return err
	}
	if !attach {
		_, err = fmt.Fprintln(stdout, window)
		return err
	}
	if os.Getenv("TMUX") != "" {
		return fmt.Errorf("already inside tmux; select the workspace window or use --no-attach")
	}
	return ExecuteContext(ctx, Command{"tmux", append(tmuxArgs, "attach-session", "-t", name)}, os.Stdin, stdout, stderr)
}
func mustExecutable() string { p, _ := os.Executable(); return p }
func ThemeSnapshot(ctx context.Context) ([]byte, error) {
	// Export effective options/key bindings instead of running local plugin installers remotely.
	var lines []string
	for _, args := range [][]string{{"show-options", "-g"}, {"show-window-options", "-g"}, {"list-keys"}} {
		b, e := exec.CommandContext(ctx, "tmux", args...).Output()
		if e != nil {
			return nil, fmt.Errorf("start your usual local tmux before exporting its style: %w", e)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			if args[0] == "list-keys" {
				if strings.Contains(line, "run-shell") {
					continue
				}
				lines = append(lines, line)
				continue
			}
			if strings.HasPrefix(line, "default-shell ") || strings.HasPrefix(line, "default-command ") || strings.HasPrefix(line, "@") {
				continue
			}
			prefix := "set-option -q -g "
			if args[0] == "show-window-options" {
				prefix = "set-window-option -q -g "
			}
			lines = append(lines, prefix+line)
		}
	}
	return []byte("# Effective options exported by Stealth Box. Fonts stay in your local terminal.\n" + strings.Join(lines, "\n") + "\nset-option -g allow-rename off\n"), nil
}

func PlainRemote(ctx context.Context, c Config, configPath, project, agent, runner string, extra []string, stdout, stderr io.Writer) error {
	launch, err := resolveWorkspaceLaunch(ctx, c, project, agent, runner)
	if err != nil {
		return err
	}
	script, err := workspaceLaunchScript(c, configPath, launch, extra)
	if err != nil {
		return err
	}
	return ExecuteContext(ctx, At(Endpoint{}, "cd "+Quote(launch.directory)+" && "+script, false), os.Stdin, stdout, stderr)
}
