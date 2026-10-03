package stealthbox

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

func Doctor(ctx context.Context, c Config, w io.Writer) error {
	failed := false
	check := func(name string, fn func(context.Context) error) bool {
		testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := fn(testCtx); err != nil {
			fmt.Fprintf(w, "FAIL %s: %v\n", name, err)
			failed = true
			return false
		} else {
			fmt.Fprintf(w, " OK  %s\n", name)
			return true
		}
	}
	hosts := map[string]bool{}
	addHost := func(host string) {
		if host != "" {
			hosts[host] = false
		}
	}
	addHost(c.Workspace.Host)
	for _, project := range c.Projects {
		addHost(project.Source.Host)
		for _, runner := range project.Runners {
			addHost(runner.Host)
		}
	}
	check("local SSH", func(context.Context) error { _, e := exec.LookPath("ssh"); return e })
	if c.Bridge.Enabled {
		check("local Docker", func(ctx context.Context) error { return exec.CommandContext(ctx, "docker", "info").Run() })
	}
	var hostNames []string
	for host := range hosts {
		hostNames = append(hostNames, host)
	}
	sort.Strings(hostNames)
	for _, host := range hostNames {
		hosts[host] = check("SSH "+host, func(ctx context.Context) error {
			_, err := noninteractiveOutput(ctx, sshCommand(host, "true", false))
			return err
		})
	}
	remoteCheck := func(name, host string, fn func(context.Context) error) {
		if host != "" && !hosts[host] {
			fmt.Fprintf(w, "SKIP %s: initial SSH probe for %s failed\n", name, host)
			return
		}
		check(name, fn)
	}
	if WorkspaceRootEnabled(c) {
		remoteCheck("workspace Projects root", c.Workspace.Host, func(ctx context.Context) error {
			if !filepath.IsAbs(c.Workspace.VMRoot) {
				return fmt.Errorf("run setup to expand VM ~")
			}
			_, e := noninteractiveOutput(ctx, sshCommand(c.Workspace.Host, "test -d "+Quote(c.Workspace.VMRoot), false))
			return e
		})
		if c.Workspace.SyncTransport == "rsync" || c.Workspace.SyncTransport == "auto" || c.Workspace.SyncTransport == "" {
			check("local rsync", func(context.Context) error { _, e := exec.LookPath("rsync"); return e })
			remoteCheck("VM rsync + Git", c.Workspace.Host, func(ctx context.Context) error {
				_, e := noninteractiveOutput(ctx, sshCommand(c.Workspace.Host, "command -v rsync >/dev/null && command -v git >/dev/null", false))
				return e
			})
		}
		for _, name := range []string{"codex", "claude"} {
			args := c.Agents[name]
			if len(args) == 0 {
				continue
			}
			remoteCheck("workspace agent "+name, c.Workspace.Host, func(ctx context.Context) error {
				_, e := noninteractiveOutput(ctx, sshCommand(c.Workspace.Host, remoteToolPath+"; command -v "+Quote(args[0]), false))
				return e
			})
		}
	}
	if c.Workspace.Host != "" {
		remoteCheck("remote tmux", c.Workspace.Host, func(ctx context.Context) error {
			_, e := noninteractiveOutput(ctx, sshCommand(c.Workspace.Host, "command -v tmux", false))
			return e
		})
	}
	if c.Workspace.RemoteDir != "" {
		remoteCheck("remote Stealth Box", c.Workspace.Host, func(ctx context.Context) error {
			_, e := noninteractiveOutput(ctx, sshCommand(c.Workspace.Host, Quote(filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox"))+" --version", false))
			return e
		})
	}
	for _, name := range Names(c) {
		p := c.Projects[name]
		remoteCheck("source "+name, p.Source.Host, func(ctx context.Context) error {
			_, e := noninteractiveOutput(ctx, At(p.Source, "test -d "+Quote(p.Source.Path), false))
			return e
		})
		agent, _ := Defaults(p, "", "")
		args := c.Agents[agent]
		if len(args) == 0 {
			args = []string{agent}
		}
		remoteCheck("agent "+agent+" for "+name, p.Source.Host, func(ctx context.Context) error {
			_, e := noninteractiveOutput(ctx, At(p.Source, remoteToolPath+"; command -v "+Quote(args[0]), false))
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
