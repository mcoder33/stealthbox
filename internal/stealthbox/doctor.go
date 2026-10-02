package stealthbox

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"
)

func Doctor(ctx context.Context, c Config, w io.Writer) error {
	failed := false
	check := func(name string, fn func(context.Context) error) {
		testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := fn(testCtx); err != nil {
			fmt.Fprintf(w, "FAIL %s: %v\n", name, err)
			failed = true
		} else {
			fmt.Fprintf(w, " OK  %s\n", name)
		}
	}
	check("local SSH", func(context.Context) error { _, e := exec.LookPath("ssh"); return e })
	if c.Bridge.Enabled {
		check("local Docker", func(ctx context.Context) error { return exec.CommandContext(ctx, "docker", "info").Run() })
	}
	if WorkspaceRootEnabled(c) {
		check("workspace Projects root", func(ctx context.Context) error {
			if !filepath.IsAbs(c.Workspace.VMRoot) {
				return fmt.Errorf("run setup to expand VM ~")
			}
			_, e := Output(ctx, sshCommand(c.Workspace.Host, "test -d "+Quote(c.Workspace.VMRoot), false))
			return e
		})
		if c.Workspace.SyncTransport == "rsync" || c.Workspace.SyncTransport == "auto" || c.Workspace.SyncTransport == "" {
			check("local rsync", func(context.Context) error { _, e := exec.LookPath("rsync"); return e })
			check("VM rsync + Git", func(ctx context.Context) error {
				_, e := Output(ctx, sshCommand(c.Workspace.Host, "command -v rsync >/dev/null && command -v git >/dev/null", false))
				return e
			})
		}
		for _, name := range []string{"codex", "claude"} {
			args := c.Agents[name]
			if len(args) == 0 {
				continue
			}
			check("workspace agent "+name, func(ctx context.Context) error {
				_, e := Output(ctx, sshCommand(c.Workspace.Host, remoteToolPath+"; command -v "+Quote(args[0]), false))
				return e
			})
		}
	}
	if c.Workspace.Host != "" {
		check("SSH + remote tmux", func(ctx context.Context) error {
			_, e := Output(ctx, sshCommand(c.Workspace.Host, "command -v tmux", false))
			return e
		})
	}
	if c.Workspace.RemoteDir != "" {
		check("remote Stealth Box", func(ctx context.Context) error {
			_, e := Output(ctx, sshCommand(c.Workspace.Host, Quote(filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox"))+" --version", false))
			return e
		})
	}
	for _, name := range Names(c) {
		p := c.Projects[name]
		check("source "+name, func(ctx context.Context) error {
			_, e := Output(ctx, At(p.Source, "test -d "+Quote(p.Source.Path), false))
			return e
		})
		agent, _ := Defaults(p, "", "")
		args := c.Agents[agent]
		if len(args) == 0 {
			args = []string{agent}
		}
		check("agent "+agent+" for "+name, func(ctx context.Context) error {
			_, e := Output(ctx, At(p.Source, remoteToolPath+"; command -v "+Quote(args[0]), false))
			return e
		})
	}
	if c.Bridge.Enabled {
		check("Mac bridge (connect or bridge must be running)", func(ctx context.Context) error { return BridgeHealth(ctx, c) })
	}
	if failed {
		return fmt.Errorf("some checks failed; see results above")
	}
	return nil
}
