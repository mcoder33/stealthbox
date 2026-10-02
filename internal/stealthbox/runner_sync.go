package stealthbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func runnerSyncTransport(c Config, p Project) string {
	switch c.Workspace.SyncTransport {
	case "rsync", "archive":
		return c.Workspace.SyncTransport
	}
	if p.Source.Host != "" {
		return "rsync"
	}
	return "archive"
}

func runnerRsyncCommand(source, destination Endpoint) Command {
	args := []string{"-az", "--checksum", "--safe-links", "--no-links", "--delete-delay"}
	for _, pattern := range []string{".stealthbox-runner", ".git", ".env*", "node_modules", "vendor", ".serena", ".claude", ".codex", ".agents", ".opencode", ".kimi-code"} {
		args = append(args, "--exclude="+pattern)
	}
	ssh := []string{"ssh", "-o", "BatchMode=yes"}
	if config := os.Getenv("STEALTHBOX_SSH_CONFIG"); config != "" {
		ssh = append(ssh, "-F", config)
	}
	args = append(args, "-e", shellArgs(ssh), "--", rsyncLocation(source), rsyncLocation(destination))
	return Command{"rsync", args}
}

func rsyncLocation(endpoint Endpoint) string {
	if endpoint.Host == "" {
		return endpoint.Path + "/"
	}
	path := endpoint.Path + "/"
	if !remotePathRE.MatchString(path) {
		path = Quote(path)
	}
	return endpoint.Host + ":" + path
}

var rsyncTotalSizeRE = regexp.MustCompile(`(?m)^Total file size:\s*([0-9,]+)(?: bytes| B)?\s*$`)

// PullRunner uses delta rsync over the Mac's outbound SSH connection. The target
// is disposable and marked. Source files are never pushed into local checkouts.
func PullRunner(ctx context.Context, c Config, source Endpoint, destination string, stderr io.Writer) error {
	if source.Host == "" {
		return fmt.Errorf("rsync bridge transport needs the configured source SSH alias")
	}
	if !hostRE.MatchString(source.Host) || !filepath.IsAbs(source.Path) || strings.ContainsAny(source.Path, "\n\r\x00") {
		return fmt.Errorf("invalid rsync source")
	}
	if err := validateRunnerRoot(destination); err != nil {
		return err
	}
	cmd := runnerRsyncCommand(source, Endpoint{Path: destination})
	// Check total selected source size before transfer, not vendor/secret caches.
	dryArgs := append([]string{}, cmd.Args[:len(cmd.Args)-3]...)
	dryArgs = append(dryArgs, "--dry-run", "--stats")
	dryArgs = append(dryArgs, cmd.Args[len(cmd.Args)-3:]...)
	var statsOutput strings.Builder
	err := executeRunnerSync(ctx, Command{"env", append([]string{"LC_ALL=C", "rsync"}, dryArgs...)}, &statsOutput, stderr)
	stats := []byte(statsOutput.String())
	if err != nil {
		return fmt.Errorf("rsync source preflight: %w", err)
	}
	match := rsyncTotalSizeRE.FindStringSubmatch(string(stats))
	if len(match) != 2 {
		return fmt.Errorf("cannot verify rsync source size")
	}
	size, err := strconv.ParseInt(strings.ReplaceAll(match[1], ",", ""), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid rsync source size")
	}
	max, _ := limits(c)
	if size > max {
		return fmt.Errorf("rsync selected source exceeds %d bytes", max)
	}
	if err = executeRunnerSync(ctx, cmd, io.Discard, stderr); err != nil {
		return fmt.Errorf("rsync source pull: %w", err)
	}
	// Source may change during rsync. Refuse command execution if the resulting
	// checkout exceeds the limit; dependency caches and secret files are excluded.
	var total int64
	return filepath.WalkDir(destination, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, e := filepath.Rel(destination, path)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		if excluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		if total > max {
			return fmt.Errorf("rsync result exceeds %d bytes; command was not started", max)
		}
		return nil
	})
}

func executeRunnerSync(ctx context.Context, command Command, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, command.Program, command.Args...)
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
	return cmd.Run()
}
