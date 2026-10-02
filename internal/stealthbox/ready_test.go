package stealthbox

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMinimalWorkspaceConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"workspace":{"host":"dev-vm","vm_root":"~/Projects","runner":"local"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Bridge.Enabled || !c.Workspace.ShellIntegration || !PrepareEnabled(c) || WorkspaceRunner(c) != "mac" || !filepath.IsAbs(c.Workspace.LocalRoot) || !filepath.IsAbs(c.Workspace.RunnerRoot) {
		t.Fatal("minimal config did not enable the prepared workspace")
	}
	if c.Workspace.VMRoot != "~/Projects" || c.Workspace.ID == "" || len(c.Agents) != 3 {
		t.Fatal("VM home must stay unresolved until SSH setup")
	}
	if err = Save(path, c); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte(`"agents"`)) {
		t.Fatal("normal configuration must not require an agent selection")
	}
	loaded, err := Load(path)
	if err != nil || loaded.Bridge.Token != c.Bridge.Token || loaded.Workspace.ID != c.Workspace.ID {
		t.Fatal("generated bridge token and identity must survive reload", err)
	}
}

func TestNativeAgentUtilityCommands(t *testing.T) {
	c, _ := DefaultConfig()
	for _, name := range []string{"codex", "claude"} {
		for _, extra := range [][]string{{"--version"}, {"login", "--device-auth"}, {"auth", "login"}, {"mcp", "list"}, {"--help"}} {
			argv, err := AgentCommand(c, "/private/config.json", "", name, "vm", extra)
			want := append([]string{name}, extra...)
			if err != nil || !slices.Equal(argv, want) {
				t.Fatalf("utility %s: %q, %v", name, argv, err)
			}
		}
		argv, err := AgentCommand(c, "/private/config.json", "", name, "vm", []string{"resume"})
		if err != nil || !strings.Contains(strings.Join(argv, " "), "mcp") {
			t.Fatal("interactive resume lost runner tools", err)
		}
	}
}

func TestPreparationInstallsBothAgentsOnce(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tools")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// Keep the test offline and unprivileged: tools are present, downloads are
	// fixture installers. The real vendor installers are checked on the test VM.
	for _, tool := range []string{"tmux", "git", "rsync", "tic", "infocmp", "rg"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	curl := `#!/bin/sh
set -eu
agent=
output=
while [ "$#" -gt 0 ]; do
  case "$1" in
    https://chatgpt.com/codex/install.sh) agent=codex ;;
    https://claude.ai/install.sh) agent=claude ;;
    -o) shift; output=$1 ;;
  esac
  shift
done
test -n "$agent" && test -n "$output"
printf 'mkdir -p "$HOME/.local/bin"\nprintf "#!/bin/sh\\nexit 0\\n" > "$HOME/.local/bin/%s"\nchmod 700 "$HOME/.local/bin/%s"\nprintf "%s\\n" >> "$HOME/installed"\n' "$agent" "$agent" "$agent" > "$output"
`
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "sh", "-c", prepareToolsScript)
		cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+":/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("prepare #%d: %v: %s", i, err, out)
		}
	}
	installed, err := os.ReadFile(filepath.Join(dir, "installed"))
	if err != nil || string(installed) != "codex\nclaude\n" {
		t.Fatal("preparation must install both agents once, then retain existing installations", err)
	}
}

// A private socket directory keeps these tests away from any user tmux server.
func isolatedTmux(t *testing.T) func(...string) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; also covered by the SSH integration fixture")
	}
	dir, err := os.MkdirTemp("/tmp", "sb-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX", "")
	t.Setenv("SHELL", "/bin/bash")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "tmux", "-L", "stealthbox", "kill-server").Run()
		_ = exec.CommandContext(ctx, "tmux", "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
	return func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", "stealthbox"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %q: %s, %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
}

func TestResumeKeepsActiveWindowAndProcess(t *testing.T) {
	tmux := isolatedTmux(t)
	c := rootConfig(t)
	c.Workspace.RemoteDir = canonicalTemp(t)
	c.Workspace.ShellIntegration = true
	config := filepath.Join(c.Workspace.RemoteDir, "config.json")
	if err := Save(config, c); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ResumeWorkspaceRemote(ctx, c, config, "", "vm", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	tmux("new-window", "-t", "stealthbox", "-n", "my-agent", "sleep 60")
	before := tmux("display-message", "-p", "-t", "stealthbox", "#{window_id}|#{pane_pid}|#{session_windows}")
	if err := ResumeWorkspaceRemote(ctx, c, config, "", "mac", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	after := tmux("display-message", "-p", "-t", "stealthbox", "#{window_id}|#{pane_pid}|#{session_windows}")
	if after != before {
		t.Fatalf("resume changed the active process/window: %q -> %q", before, after)
	}
	if !strings.Contains(tmux("show-options", "-t", "stealthbox", "default-command"), "--rcfile") || tmux("show-environment", "-t", "stealthbox", "STEALTHBOX_SCOPE") != "STEALTHBOX_SCOPE=workspace" {
		t.Fatal("ordinary new windows must inherit the managed shell")
	}
}

func TestThemeCompatibilityAndLiveSession(t *testing.T) {
	tmux := isolatedTmux(t)
	c := rootConfig(t)
	c.Workspace.RemoteDir = canonicalTemp(t)
	tmux("-f", "/dev/null", "new-session", "-d", "-s", "stealthbox", "sleep 90")
	tmux("set-option", "-g", "status-format[0]", "broken imported layout")
	before := tmux("display-message", "-p", "-t", "stealthbox", "#{pane_pid}")
	source := filepath.Join(c.Workspace.RemoteDir, "source.conf")
	data := "set -g @thm_test '#cba6f7'\nset -g status-position top\nset -g status-right '#{E:@thm_test}'\nset-window-option -q -g cursor-colour none\nset-window-option -q -g pane-colours\nbind-key DefinitelyNotAKey display-message unsupported\n"
	if err := os.WriteFile(source, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var report bytes.Buffer
	if err := ApplyTheme(context.Background(), c, source, &report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.String(), "unsupported") || before != tmux("display-message", "-p", "-t", "stealthbox", "#{pane_pid}") {
		t.Fatal("theme validation must report incompatible keys without restarting a pane")
	}
	if tmux("show-options", "-gv", "status-position") != "top" || tmux("display-message", "-p", "-t", "stealthbox", "#{E:status-right}") != "#cba6f7" || tmux("show-options", "-gv", "status-format[0]") == "broken imported layout" {
		t.Fatal("theme lost colors, placement, or native layout")
	}
	if err := ApplyTheme(context.Background(), c, source, io.Discard); err != nil {
		t.Fatal("second application is not idempotent", err)
	}
}

func TestThemeSnapshotPreservesPaletteAndOmitsGeneratedLayout(t *testing.T) {
	isolatedTmux(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "tmux", "-f", "/dev/null", "new-session", "-d", "-s", "style", "sleep 60").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(ctx, "tmux", "set-option", "-g", "@palette", "#cba6f7").Run(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ThemeSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(snapshot, []byte("@palette")) || bytes.Contains(snapshot, []byte("status-format[")) {
		t.Fatal("snapshot must keep user palette and avoid exporting tmux version-specific defaults")
	}
	if err := exec.CommandContext(ctx, "tmux", "set-option", "-g", "status-format[0]", "custom #S #W").Run(); err != nil {
		t.Fatal(err)
	}
	snapshot, err = ThemeSnapshot(ctx)
	if err != nil || !bytes.Contains(snapshot, []byte("custom #S #W")) {
		t.Fatal("explicit custom status layout was lost", err)
	}
}
