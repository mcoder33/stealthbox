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
	p, ok := c.Projects[project]
	if !ok {
		return fmt.Errorf("unknown project")
	}
	if p.Source.Host != "" {
		return fmt.Errorf("workspace command runs on the VM; use connect on the Mac")
	}
	agent, runner = Defaults(p, agent, runner)
	if _, ok = p.Runners[runner]; !ok {
		return fmt.Errorf("unknown runner")
	}
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
	window := WindowName(project, agent, runner, slot, extra)
	cmd, err := AgentCommand(c, configPath, project, agent, runner, extra)
	if err != nil {
		return err
	}
	// Use the same login shell PATH as a normal interactive session, without copying agent credentials.
	environment := "export PATH=" + Quote(filepath.Dir(mustExecutable())) + ":\"$PATH\" STEALTHBOX_CONFIG=" + Quote(configPath) + " STEALTHBOX_PROJECT=" + Quote(project) + " STEALTHBOX_RUNNER=" + Quote(runner) + " STEALTHBOX_AGENT=" + Quote(agent) + "; exec " + shellArgs(cmd)
	tmuxArgs := []string{"-L", "stealthbox"}
	theme := filepath.Join(c.Workspace.RemoteDir, "tmux.conf")
	if _, e := os.Stat(theme); e == nil {
		tmuxArgs = append(tmuxArgs, "-f", theme)
	}
	invoke := func(args ...string) ([]byte, error) {
		return Output(ctx, Command{"tmux", append(append([]string{}, tmuxArgs...), args...)})
	}
	if _, err = invoke("has-session", "-t", name); err != nil {
		if _, err = invoke("new-session", "-d", "-s", name, "-n", window, "-c", p.Source.Path, environment); err != nil {
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
			if _, err = invoke("new-window", "-t", name, "-n", window, "-c", p.Source.Path, environment); err != nil {
				return err
			}
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
	p, ok := c.Projects[project]
	if !ok || p.Source.Host != "" {
		return fmt.Errorf("plain session must run on source VM")
	}
	cmd, err := AgentCommand(c, configPath, project, agent, runner, extra)
	if err != nil {
		return err
	}
	script := "cd " + Quote(p.Source.Path) + " && export PATH=" + Quote(filepath.Dir(mustExecutable())) + ":\"$PATH\" STEALTHBOX_CONFIG=" + Quote(configPath) + " STEALTHBOX_PROJECT=" + Quote(project) + " STEALTHBOX_RUNNER=" + Quote(runner) + " && exec " + shellArgs(cmd)
	return ExecuteContext(ctx, At(p.Source, script, false), os.Stdin, stdout, stderr)
}
