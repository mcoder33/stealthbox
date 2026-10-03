package stealthbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BuildVersion is set by the CLI. The handshake also detects older binaries
// which predate prepared workspaces, even when their release name is "dev".
var BuildVersion = "dev"

func Handshake() string { return "stealthbox-ready-v1 " + BuildVersion }

func managedPath() string {
	return managedBins() + string(os.PathListSeparator) + os.Getenv("PATH")
}

func managedBins() string {
	home, _ := os.UserHomeDir()
	return filepath.Dir(mustExecutable()) + string(os.PathListSeparator) +
		filepath.Join(home, ".local", "bin") + string(os.PathListSeparator) +
		filepath.Join(home, ".cargo", "bin")
}

const remoteToolPath = `export PATH="$HOME/.local/bin:$HOME/.cargo/bin:$PATH"`

// Both agents are part of the standard environment. Account authentication is
// owned by the native CLIs; no credentials are copied from the local machine.
const prepareToolsScript = `set -eu
export PATH="$HOME/.local/bin:$HOME/.cargo/bin:$PATH"
missing=""
for tool in tmux git rsync curl bash tic infocmp rg; do
    command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
    printf 'Preparing system tools:%s\n' "$missing"
    if [ "$(id -u)" = 0 ]; then
        elevate() { "$@"; }
    elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
        elevate() { sudo -n "$@"; }
    else
        printf 'System tools are missing:%s. Install them with your administrator, then run stealthbox again.\n' "$missing" >&2
        exit 1
    fi
    if command -v apt-get >/dev/null 2>&1; then
        elevate apt-get update -qq
        elevate env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq tmux git rsync curl bash ncurses-bin ca-certificates ripgrep bubblewrap
    elif command -v dnf >/dev/null 2>&1; then
        elevate dnf install -y tmux git rsync curl bash ncurses ca-certificates ripgrep bubblewrap
    elif command -v apk >/dev/null 2>&1; then
        elevate apk add tmux git rsync curl bash ncurses ca-certificates ripgrep libgcc libstdc++
    else
        printf 'Automatic preparation supports apt, dnf and apk. Missing:%s\n' "$missing" >&2
        exit 1
    fi
fi
task_tmp=$(mktemp -d)
trap 'rm -rf "$task_tmp"' EXIT HUP INT TERM
if ! command -v codex >/dev/null 2>&1; then
    printf 'Installing Codex from OpenAI…\n'
    curl --proto '=https' --tlsv1.2 -fsSL --retry 2 --connect-timeout 20 --max-time 180 https://chatgpt.com/codex/install.sh -o "$task_tmp/codex-install.sh"
    sh "$task_tmp/codex-install.sh"
fi
if ! command -v claude >/dev/null 2>&1; then
    printf 'Installing Claude Code from Anthropic…\n'
    curl --proto '=https' --tlsv1.2 -fsSL --retry 2 --connect-timeout 20 --max-time 180 https://claude.ai/install.sh -o "$task_tmp/claude-install.sh"
    bash "$task_tmp/claude-install.sh"
fi
for tool in tmux git rsync codex claude; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'Preparation did not provide %s in PATH\n' "$tool" >&2; exit 1; }
done
printf 'System tools, Codex and Claude are installed. Sign in when first starting each agent.\n'
`

func prepareRemoteTools(ctx context.Context, c Config, stdout io.Writer) error {
	if !PrepareEnabled(c) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := ExecuteContext(ctx, sshCommand(c.Workspace.Host, "sh -s", false), strings.NewReader(prepareToolsScript), stdout, stdout); err != nil {
		return fmt.Errorf("prepare VM tools (rerun after resolving the error above): %w", err)
	}
	return nil
}

func checkRemoteTools(ctx context.Context, c Config) error {
	if !PrepareEnabled(c) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	script := remoteToolPath + `; for tool in tmux git rsync curl bash tic infocmp rg codex claude; do command -v "$tool" >/dev/null 2>&1 || exit 1; done`
	_, err := Output(ctx, sshCommand(c.Workspace.Host, script, false))
	return err
}

// EnsureReady also upgrades old installations reached through settings/bridge
// actions, not only the main connect command.
func EnsureReady(ctx context.Context, c *Config, path, binary string, w io.Writer) error {
	if _, _, err := resolveRemote(ctx, c); err != nil {
		return err
	}
	if err := prepareRemoteWorkspace(ctx, *c); err != nil {
		return err
	}
	if c.Workspace.RemoteDir != "" {
		probe, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := Output(probe, sshCommand(c.Workspace.Host, Quote(filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox"))+" handshake", false))
		cancel()
		if err == nil && strings.TrimSpace(string(out)) == Handshake() && checkRemoteTools(ctx, *c) == nil {
			if err = prepareWorkspaceTheme(ctx, c, path, w); err != nil {
				return err
			}
			if err = DeployConfigWithOutput(ctx, *c, w); err != nil {
				return err
			}
			if err = prepareTerminfo(ctx, *c); err != nil {
				return err
			}
			return Save(path, *c)
		}
	}
	return Setup(ctx, c, path, binary, w)
}

func prepareRemoteWorkspace(ctx context.Context, c Config) error {
	if !WorkspaceRootEnabled(c) {
		return nil
	}
	return ExecuteNoninteractiveContext(ctx, sshCommand(c.Workspace.Host, "umask 077; mkdir -p "+Quote(c.Workspace.VMRoot), false), nil, io.Discard, io.Discard)
}

// Install the terminal description in the remote user's database. A terminal
// type must be known before attach, otherwise tmux fails after SSH succeeds.
func prepareTerminfo(ctx context.Context, c Config) error {
	terminal := os.Getenv("TERM")
	if terminal == "" || terminal == "dumb" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := Output(ctx, sshCommand(c.Workspace.Host, "infocmp "+Quote(terminal)+" >/dev/null 2>&1", false)); err == nil {
		return nil
	}
	description, err := exec.CommandContext(ctx, "infocmp", "-x", terminal).Output()
	if err != nil {
		return fmt.Errorf("terminal %s is unknown on the VM and its local terminfo could not be read: %w", terminal, err)
	}
	var details bytes.Buffer
	cmd := sshCommand(c.Workspace.Host, `umask 077; mkdir -p "$HOME/.terminfo" && tic -x -o "$HOME/.terminfo" -`, false)
	if err = ExecuteContext(ctx, cmd, bytes.NewReader(description), &details, &details); err != nil {
		return fmt.Errorf("install terminfo for %s: %w: %s", terminal, err, strings.TrimSpace(details.String()))
	}
	return nil
}

func prepareWorkspaceTheme(ctx context.Context, c *Config, configPath string, w io.Writer) error {
	if !PrepareEnabled(*c) {
		return nil
	}
	if c.Workspace.Theme != "" {
		old, err := os.ReadFile(c.Workspace.Theme)
		if err != nil || !bytes.HasPrefix(old, []byte("# Effective options exported by Stealth Box.")) {
			return err
		}
		// Upgrade only our own v0.3 export. Hand-written themes stay untouched.
		theme, err := ThemeSnapshot(ctx)
		if err != nil {
			return nil // no running local tmux: keep the existing style
		}
		if err = writePrivate(c.Workspace.Theme+".v0.3.bak", old); err != nil {
			return err
		}
		return writePrivate(c.Workspace.Theme, theme)
	}
	theme, err := ThemeSnapshot(ctx)
	if err != nil {
		theme = []byte(defaultTheme)
		fmt.Fprintln(w, "Local tmux is unavailable; using the built-in theme. Import your style later with stealthbox theme.")
	}
	path := filepath.Join(filepath.Dir(configPath), "tmux.conf")
	if err = writePrivate(path, theme); err != nil {
		return err
	}
	c.Workspace.Theme = path
	return nil
}

func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".stealthbox-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

const defaultTheme = `# Portable Stealth Box defaults. Your terminal keeps its font and palette.
set -g status-position top
set -g base-index 1
set -g renumber-windows on
set -g mouse on
set -g status-style 'bg=#1e1e2e,fg=#cdd6f4'
set -g status-left '#[bg=#b4befe,fg=#1e1e2e,bold] #S #[default] '
set -g status-left-length 30
set -g status-right ' #[fg=#a6adc8]#H · %H:%M '
set -g status-right-length 40
setw -g window-status-format ' #I:#W '
setw -g window-status-current-format '#[bg=#45475a,fg=#cdd6f4,bold] #I:#W #[default]'
set -g allow-rename off
`
