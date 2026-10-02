package stealthbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type ConnectOptions struct {
	Project, Agent, Runner, Slot, Binary, Session string
	Extra                                         []string
	Reconnect                                     bool
}

func Connect(ctx context.Context, c *Config, path string, o ConnectOptions, stdout, stderr io.Writer) error {
	if os.Getenv("TMUX") != "" {
		return fmt.Errorf("open Stealth Box in a terminal outside your local tmux to avoid nested prefixes; use a new terminal tab")
	}
	if len(c.Projects) == 0 && !WorkspaceRootEnabled(*c) {
		return fmt.Errorf("add a project in stealthbox tui first")
	}
	if o.Project == "" && !WorkspaceRootEnabled(*c) {
		o.Project = Names(*c)[0]
	}
	p, ok := c.Projects[o.Project]
	if !ok && !WorkspaceRootEnabled(*c) {
		return fmt.Errorf("unknown project")
	}
	o.Runner = runnerAlias(o.Runner)
	if ok {
		o.Agent, o.Runner = Defaults(p, o.Agent, o.Runner)
	} else {
		if o.Agent == "" {
			o.Agent = "shell"
		}
		if o.Runner == "" {
			o.Runner = "vm"
		}
		if o.Runner != "vm" && o.Runner != "mac" {
			return fmt.Errorf("root workspace runner must be local or vm")
		}
	}
	if o.Runner == "mac" && !c.Bridge.Enabled {
		return fmt.Errorf("enable the Mac bridge in TUI settings before selecting the Mac runner")
	}
	if _, configured := p.Runners[o.Runner]; ok && !configured {
		return fmt.Errorf("runner is not configured")
	}
	if c.Workspace.RemoteDir == "" {
		if err := Setup(ctx, c, path, o.Binary, stdout); err != nil {
			return err
		}
	} else {
		remoteBin := filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox")
		if _, err := Output(ctx, sshCommand(c.Workspace.Host, "test -x "+Quote(remoteBin), false)); err != nil {
			if err = Setup(ctx, c, path, o.Binary, stdout); err != nil {
				return err
			}
		} else if err = DeployConfig(ctx, *c); err != nil {
			return err
		}
	}
	if c.Bridge.Enabled {
		if err := StartBridgeService(ctx, *c, path, stderr); err != nil {
			return err
		}
		if err := WaitRemoteBridge(ctx, *c); err != nil {
			return err
		}
	}
	remoteBin := filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox")
	remoteCfg := filepath.Join(c.Workspace.RemoteDir, "config.json")
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		argv := []string{remoteBin, "workspace", "--config", remoteCfg, "--agent", o.Agent, "--runner", o.Runner}
		if o.Project != "" {
			if ok {
				argv = append(argv, "--project", o.Project)
			} else {
				argv = append(argv, "--path", o.Project)
			}
		}
		if o.Session != "" {
			argv = append(argv, "--session", o.Session)
		}
		if o.Slot != "" {
			argv = append(argv, "--slot", o.Slot)
		}
		if len(o.Extra) > 0 {
			argv = append(argv, "--")
			argv = append(argv, o.Extra...)
		}
		cmd := sshCommand(c.Workspace.Host, shellArgs(argv), true)
		options := []string{"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=yes"}
		for _, f := range c.Workspace.Forwards {
			options = append(options, "-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", f.Local, f.Remote))
		}
		cmd.Args = append(options, cmd.Args...)
		err := ExecuteContext(ctx, cmd, os.Stdin, stdout, stderr)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ex *exec.ExitError
		if !o.Reconnect || !errors.As(err, &ex) || ex.ExitCode() != 255 {
			return err
		}
		if err = pause(ctx, delay, stderr); err != nil {
			return err
		}
		delay = backoff(delay)
	}
}
func backoff(d time.Duration) time.Duration {
	if d < 15*time.Second {
		return d * 2
	}
	return 30 * time.Second
}
func pause(ctx context.Context, d time.Duration, w io.Writer) error {
	fmt.Fprintf(w, "SSH disconnected; reconnecting in %s. No commands are retried. Ctrl-C to stop.\n", d)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
