package stealthbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// NormalizeThemeLine handles differences in tmux's displayed option values.
// Empty array options have no value to replay. In older tmux, cursor-colour
// uses "default" for the newer "none" spelling.
func NormalizeThemeLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return line
	}
	fields := strings.Fields(line)
	if fields[0] != "set-option" && fields[0] != "set-window-option" && fields[0] != "set" && fields[0] != "setw" {
		return line
	}
	i := 1
	for i < len(fields) && strings.HasPrefix(fields[i], "-") {
		i++
	}
	if i+1 >= len(fields) {
		return ""
	}
	switch fields[i] {
	case "default-shell", "default-command", "destroy-unattached", "exit-unattached", "exit-empty":
		return ""
	}
	// Quiet options conceal compatibility failures from the validation server.
	line = strings.Replace(line, " -q ", " ", 1)
	if fields[i] == "cursor-colour" && fields[i+1] == "none" {
		line = strings.TrimSuffix(line, "none") + "default"
	}
	return line
}

// ApplyTheme validates against the actual remote tmux, on a disposable server.
// Only the successfully accepted commands reach the user's running server.
func ApplyTheme(ctx context.Context, c Config, source string, w io.Writer) error {
	if c.Workspace.RemoteDir == "" {
		return fmt.Errorf("theme needs a remote state directory")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	version, err := exec.CommandContext(ctx, "tmux", "-V").Output()
	if err != nil {
		return fmt.Errorf("check tmux version: %w", err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(append(append([]byte("theme-v2\n"), version...), data...)))
	target := filepath.Join(c.Workspace.RemoteDir, "tmux.conf")
	old, _ := os.ReadFile(target + ".sha256")
	if string(old) == digest {
		if _, err := os.Stat(target); err == nil {
			return nil
		}
	}
	dir, err := os.MkdirTemp("", "stealthbox-theme-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	socket := filepath.Base(dir)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	invoke := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", socket}, args...)...)
		return cmd.CombinedOutput()
	}
	if out, err := invoke("-f", "/dev/null", "new-session", "-d", "-s", "validate", "sleep 120"); err != nil {
		return fmt.Errorf("start theme validation: %w: %s", err, strings.TrimSpace(string(out)))
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "tmux", "-L", socket, "kill-server").Run()
	}()
	// Reset a previously imported, incompatible generated layout. Custom
	// status-format lines below can still override the VM's own default.
	accepted := []string{"set-option -gu status-format"}
	var skipped []string
	lineFile := filepath.Join(dir, "line.conf")
	for i, original := range strings.Split(string(data), "\n") {
		line := NormalizeThemeLine(original)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err = os.WriteFile(lineFile, []byte(line+"\n"), 0600); err != nil {
			return err
		}
		out, err := invoke("source-file", lineFile)
		if ctx.Err() != nil {
			return fmt.Errorf("validate tmux theme: %w", ctx.Err())
		}
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("line %d: %s", i+1, strings.TrimSpace(string(out))))
			continue
		}
		accepted = append(accepted, line)
	}
	if len(accepted) == 1 {
		return fmt.Errorf("theme has no compatible commands; see the source theme")
	}
	validated := []byte("# Validated by Stealth Box for " + strings.TrimSpace(string(version)) + "\n" + strings.Join(accepted, "\n") + "\n")
	if err = writePrivate(target, validated); err != nil {
		return err
	}
	if err = writePrivate(target+".report", []byte(strings.Join(skipped, "\n")+"\n")); err != nil {
		return err
	}
	// Apply changes to an existing workspace without terminating any pane.
	if exec.CommandContext(ctx, "tmux", "-L", "stealthbox", "has-session").Run() == nil {
		if out, err := exec.CommandContext(ctx, "tmux", "-L", "stealthbox", "source-file", target).CombinedOutput(); err != nil {
			return fmt.Errorf("apply validated theme: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if err = writePrivate(target+".sha256", []byte(digest)); err != nil {
		return err
	}
	if len(skipped) > 0 {
		fmt.Fprintf(w, "tmux style ready; %d unsupported bindings/options omitted. Details: %s.report\n", len(skipped), target)
	}
	return nil
}
