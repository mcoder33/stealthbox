package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mcoder33/stealthbox/internal/stealthbox"
	"golang.org/x/term"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "stealthbox:", err)
		var remote *stealthbox.ExitError
		if errors.As(err, &remote) {
			os.Exit(remote.Code)
		}
		var e *exec.ExitError
		if errors.As(err, &e) {
			code := e.ExitCode()
			if code < 1 {
				code = 1
			}
			os.Exit(code)
		}
		os.Exit(1)
	}
}
func configDefault() (string, error) {
	if s := os.Getenv("STEALTHBOX_CONFIG"); s != "" {
		return s, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".config", "stealthbox", "config.json"), err
}
func run(ctx context.Context, args []string) error {
	def, err := configDefault()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			return stealthbox.TUI(ctx, def)
		}
		usage()
		return nil
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		usage()
		return nil
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Println("stealthbox", version)
		return nil
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	config := f.String("config", def, "configuration path")
	project := f.String("project", os.Getenv("STEALTHBOX_PROJECT"), "project name")
	runner := f.String("runner", os.Getenv("STEALTHBOX_RUNNER"), "runner name")
	agent := f.String("agent", "", "agent name")
	session := f.String("session", "tmux", "tmux or shell")
	slot := f.String("slot", "main", "distinct agent instance name")
	dry := f.Bool("dry-run", false, "show plan without connecting or changing files")
	host := f.String("host", "", "SSH config alias")
	remoteDir := f.String("remote-dir", "", "remote state directory")
	binary := f.String("binary", "", "prebuilt remote binary for offline setup")
	sourcePath := f.String("path", "", "absolute project source path on VM")
	macPath := f.String("mac-path", "", "dedicated disposable Mac runner path")
	remove := f.Bool("delete", false, "remove project from config; keep files")
	enableMac := f.Bool("enable-mac", false, "enable Mac runner bridge")
	allowExec := f.Bool("allow-mac-exec", false, "enable direct Mac command execution")
	on := f.String("on", "vm", "execution host: vm or mac")
	cwd := f.String("cwd", "", "Mac directory relative to runner root")
	noAttach := f.Bool("no-attach", false, "create/select window without attaching (VM command)")
	reconnect := f.Bool("reconnect", true, "reconnect after SSH transport failure")
	artifact := f.String("file", "", "artifact path relative to Mac runner")
	output := f.String("output", "", "local output file (must not exist)")
	edit := f.Bool("edit", false, "edit config with EDITOR")
	if err = f.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "init":
		if *dry {
			fmt.Println("Would create", *config)
			return nil
		}
		if _, e := os.Stat(*config); !os.IsNotExist(e) {
			return fmt.Errorf("config already exists or is inaccessible")
		}
		c, e := stealthbox.DefaultConfig()
		if e != nil {
			return e
		}
		if *host != "" {
			c.Workspace.Host = *host
		}
		if *remoteDir != "" {
			c.Workspace.RemoteDir = *remoteDir
		}
		c.Bridge.Enabled = *enableMac
		c.Bridge.AllowExec = *allowExec
		if err = stealthbox.Save(*config, c); err == nil {
			fmt.Println("Created", *config, "(mode 0600). Use stealthbox tui to add projects.")
		}
		return err
	case "tui":
		if *dry {
			return fmt.Errorf("--dry-run is not used with TUI")
		}
		return stealthbox.TUI(ctx, *config)
	}
	c, err := stealthbox.Load(*config)
	if err != nil {
		return err
	}
	if *host != "" {
		c.Workspace.Host = *host
	}
	if *remoteDir != "" {
		c.Workspace.RemoteDir = *remoteDir
	}
	switch args[0] {
	case "config":
		if *edit {
			if *dry {
				return fmt.Errorf("--dry-run cannot edit config")
			}
			editor := strings.Fields(os.Getenv("EDITOR"))
			if len(editor) == 0 {
				editor = []string{"vi"}
			}
			cmd := exec.CommandContext(ctx, editor[0], append(editor[1:], *config)...)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err = cmd.Run(); err != nil {
				return err
			}
			_, err = stealthbox.Load(*config)
			return err
		}
		c.Bridge.Token = "[redacted]"
		b, _ := json.MarshalIndent(c, "", "  ")
		fmt.Println(string(b))
		return nil
	case "list":
		for _, n := range stealthbox.Names(c) {
			p := c.Projects[n]
			a, r := stealthbox.Defaults(p, "", "")
			fmt.Printf("%s\t%s\t%s\t%s\n", n, a, r, p.Source.Path)
		}
		return nil
	case "project":
		if *project == "" {
			return fmt.Errorf("provide --project")
		}
		if *remove {
			delete(c.Projects, *project)
		} else {
			p := c.Projects[*project]
			if *sourcePath != "" {
				p.Source = stealthbox.Endpoint{Host: c.Workspace.Host, Path: *sourcePath}
			}
			if p.Runners == nil {
				p.Runners = map[string]stealthbox.Endpoint{}
			}
			p.Runners["vm"] = p.Source
			if *macPath != "" {
				p.Runners["mac"] = stealthbox.Endpoint{Path: *macPath}
			}
			if *agent != "" {
				p.Agent = *agent
			}
			if *runner != "" {
				p.Runner = *runner
			}
			c.Projects[*project] = p
		}
		if *dry {
			fmt.Println("Would update project", *project)
			return c.Validate()
		}
		return stealthbox.Save(*config, c)
	case "setup":
		if *dry {
			fmt.Printf("Would probe %s, install binary/config/theme, and save detected paths.\n", c.Workspace.Host)
			return nil
		}
		return stealthbox.Setup(ctx, &c, *config, *binary, os.Stdout)
	case "theme":
		if *dry {
			fmt.Println("Would export effective local tmux options and keys")
			return nil
		}
		b, e := stealthbox.ThemeSnapshot(ctx)
		if e != nil {
			return e
		}
		p := filepath.Join(filepath.Dir(*config), "tmux.conf")
		if e = os.WriteFile(p, b, 0600); e != nil {
			return e
		}
		c.Workspace.Theme = p
		return stealthbox.Save(*config, c)
	case "doctor":
		if *dry {
			fmt.Println("Would check SSH, tmux, agents, directories, Docker and bridge")
			return nil
		}
		return stealthbox.Doctor(ctx, c, os.Stdout)
	case "bridge-health":
		if *dry {
			fmt.Println("Would check bridge health")
			return nil
		}
		return stealthbox.BridgeHealth(ctx, c)
	case "bridge", "bridge-service":
		if *dry {
			fmt.Println("Would serve Mac runner and maintain a reverse SSH tunnel")
			return nil
		}
		return stealthbox.BridgeService(ctx, c, os.Stderr)
	case "bridge-start":
		if *dry {
			fmt.Println("Would start background Mac bridge")
			return nil
		}
		if c.Workspace.RemoteDir == "" {
			if err = stealthbox.Setup(ctx, &c, *config, *binary, os.Stdout); err != nil {
				return err
			}
		}
		if err = stealthbox.DeployConfig(ctx, c); err != nil {
			return err
		}
		return stealthbox.StartBridgeService(ctx, c, *config, os.Stdout)
	case "bridge-stop":
		if *dry {
			fmt.Println("Would stop Mac bridge")
			return nil
		}
		return stealthbox.StopBridgeService(ctx, c)
	case "bridge-status":
		if *dry {
			fmt.Println("Would check bridge status")
			return nil
		}
		s, e := stealthbox.BridgeStatus(ctx, c)
		if e != nil {
			return e
		}
		fmt.Printf("Mac bridge: online; direct execution=%t\n", s.AllowExec)
		return nil
	case "mcp":
		if *dry {
			fmt.Println("Would start MCP stdio server")
			return nil
		}
		return stealthbox.ServeMCP(ctx, c, os.Stdin, os.Stdout)
	}
	if *project == "" {
		if len(c.Projects) == 1 {
			*project = stealthbox.Names(c)[0]
		} else {
			return fmt.Errorf("set --project or STEALTHBOX_PROJECT")
		}
	}
	p, ok := c.Projects[*project]
	if !ok {
		return fmt.Errorf("unknown project %q", *project)
	}
	*agent, *runner = stealthbox.Defaults(p, *agent, *runner)
	switch args[0] {
	case "open", "connect":
		if *session != "tmux" && *session != "shell" {
			return fmt.Errorf("session must be tmux or shell")
		}
		if *dry {
			fmt.Printf("Mac terminal → SSH %s → %s workspace; project=%s agent=%s runner=%s slot=%s; Mac bridge=%t\n", c.Workspace.Host, *session, *project, *agent, *runner, *slot, c.Bridge.Enabled)
			return nil
		}
		return stealthbox.Connect(ctx, &c, *config, stealthbox.ConnectOptions{Project: *project, Agent: *agent, Runner: *runner, Slot: *slot, Binary: *binary, Extra: f.Args(), Reconnect: *reconnect, Session: *session}, os.Stdout, os.Stderr)
	case "workspace":
		if *dry {
			fmt.Println(stealthbox.WindowName(*project, *agent, *runner, *slot, f.Args()))
			return nil
		}
		if *session == "shell" {
			return stealthbox.PlainRemote(ctx, c, *config, *project, *agent, *runner, f.Args(), os.Stdout, os.Stderr)
		}
		return stealthbox.WorkspaceRemote(ctx, c, *config, *project, *agent, *runner, *slot, f.Args(), !*noAttach, os.Stdout, os.Stderr)
	case "fetch":
		if *artifact == "" || *output == "" {
			return fmt.Errorf("fetch needs --file and --output")
		}
		if *dry {
			fmt.Println("Would fetch Mac artifact", *artifact, "to", *output)
			return nil
		}
		return stealthbox.Fetch(ctx, c, *project, *artifact, *output)
	case "run", "exec":
		snapshot := args[0] == "run"
		target := *runner
		if !snapshot {
			target = *on
		}
		if *dry {
			dst, ok := p.Runners[target]
			if !ok {
				return fmt.Errorf("unknown runner")
			}
			if dst.Bridge {
				fmt.Printf("Mac bridge: sync=%t; command=%s\n", snapshot, stealthbox.JSONString(f.Args()))
				return nil
			}
			if !snapshot {
				fmt.Printf("Execute on %s: %s\n", target, stealthbox.JSONString(f.Args()))
				return nil
			}
			commands, e := stealthbox.PlanRun(p, target, f.Args())
			if e != nil {
				return e
			}
			for _, cmd := range commands {
				fmt.Printf("%s %s\n", cmd.Program, stealthbox.JSONString(cmd.Args))
			}
			return nil
		}
		return stealthbox.Run(ctx, c, *project, target, f.Args(), snapshot, *cwd, os.Stdout, os.Stderr)
	default:
		return fmt.Errorf("unknown command %q; use stealthbox help", args[0])
	}
}
func usage() {
	fmt.Println(`Stealth Box — your terminal, remote tmux, agents and a selectable Mac/VM runner

  stealthbox                         Interactive TUI (when stdin/stdout are a terminal)
  stealthbox tui [--config PATH]      Projects, agents, runners, theme, deployment, doctor
  stealthbox init --host dev-vm [--enable-mac] [--allow-mac-exec]
  stealthbox project --project NAME --path /home/user/project --mac-path /Users/user/runner
  stealthbox setup [--binary PATH]    Deploy binary/config to the VM; detect HOME and platform
  stealthbox theme                    Export current local tmux options/key bindings
  stealthbox connect --project NAME --agent codex --runner mac [--slot main]
  stealthbox open ...                 Alias for connect; --session shell skips tmux
  stealthbox run [--runner vm|mac] -- COMMAND [ARGS...]
  stealthbox exec --on vm|mac [--cwd RELATIVE] -- COMMAND [ARGS...]
  stealthbox fetch --file REL --output PATH   Download a Mac artifact to the VM
  stealthbox doctor                   Check prerequisites and live connectivity
  stealthbox bridge                  Foreground Mac runner + reverse SSH tunnel
  stealthbox bridge-start|bridge-stop|bridge-status  Manage background Mac bridge
  stealthbox config [--edit]          Show redacted config or edit with EDITOR
  stealthbox list                     List projects
  stealthbox mcp                      MCP stdio tools (used automatically by Codex/Claude)

All command flags precede -- and command arguments. --dry-run makes no changes.
Config: STEALTHBOX_CONFIG or ~/.config/stealthbox/config.json.
Project/runner defaults: STEALTHBOX_PROJECT / STEALTHBOX_RUNNER.
Connect from a terminal OUTSIDE your local tmux. On the VM, tmux uses its own
stealthbox socket and never modifies unrelated sessions or agent credentials.`)
}
