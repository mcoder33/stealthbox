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
	"time"
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
	return workspaceRemote(ctx, c, configPath, project, agent, runner, slot, extra, attach, false, stdout, stderr)
}

func ResumeWorkspaceRemote(ctx context.Context, c Config, configPath, project, runner string, attach bool, stdout, stderr io.Writer) error {
	return workspaceRemote(ctx, c, configPath, project, "shell", runner, "main", nil, attach, true, stdout, stderr)
}

func workspaceRemote(ctx context.Context, c Config, configPath, project, agent, runner, slot string, extra []string, attach, resume bool, stdout, stderr io.Writer) error {
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
	if resume {
		window = "shell"
	}
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
	} else if !resume {
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
		for key, value := range map[string]string{"STEALTHBOX_CONFIG": configPath, "STEALTHBOX_SCOPE": "workspace", "STEALTHBOX_PROJECT": "", "STEALTHBOX_RUNNER": runner, "STEALTHBOX_VM_ROOT": c.Workspace.VMRoot, "PATH": managedPath()} {
			if _, e = invoke("set-environment", "-t", name, key, value); e != nil {
				return e
			}
		}
		if _, e = invoke("set-option", "-t", name, "default-command", defaultScript); e != nil {
			return e
		}
	}
	if !resume {
		if _, err = invoke("select-window", "-t", name+":"+window); err != nil {
			return err
		}
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
	defaults := defaultStatusFormats(ctx)
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
			if strings.HasPrefix(line, "default-shell ") || strings.HasPrefix(line, "default-command ") {
				continue
			}
			// tmux's own generated status layout changes between releases. Let the
			// VM use its matching default, while retaining custom user layouts.
			if strings.HasPrefix(line, "status-format[") && (defaults == nil || defaults[line]) {
				continue
			}
			prefix := "set-option -q -g "
			if args[0] == "show-window-options" {
				prefix = "set-window-option -q -g "
			}
			if normalized := NormalizeThemeLine(prefix + line); normalized != "" {
				lines = append(lines, normalized)
			}
		}
	}
	return []byte("# Stealth Box theme v2. Effective options; fonts stay in your local terminal.\n" + strings.Join(lines, "\n") + "\nset-option -g allow-rename off\n"), nil
}

func defaultStatusFormats(ctx context.Context) map[string]bool {
	dir, err := os.MkdirTemp("", "sb-style-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(dir)
	socket := filepath.Base(dir)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", "defaults", "sleep 30").Run() != nil {
		return nil
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "tmux", "-L", socket, "kill-server").Run()
	}()
	data, err := exec.CommandContext(ctx, "tmux", "-L", socket, "show-options", "-g", "status-format").Output()
	if err != nil {
		return nil
	}
	result := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		result[line] = true
	}
	return result
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
