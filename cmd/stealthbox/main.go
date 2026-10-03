//go:build !windows

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
	stealthbox.BuildVersion = buildVersion()
	def, err := configDefault()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			args = []string{"connect"}
		} else {
			usage()
			return nil
		}
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		usage()
		return nil
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Println("stealthbox", stealthbox.BuildVersion)
		return nil
	}
	if args[0] == "handshake" {
		fmt.Println(stealthbox.Handshake())
		return nil
	}
	if strings.HasPrefix(args[0], "--") {
		args = append([]string{"connect"}, args...)
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	parseArgs := args[1:]
	positionalAgent, sourceAction := "", ""
	if args[0] == "agent" || args[0] == "source" {
		if len(parseArgs) == 0 || strings.HasPrefix(parseArgs[0], "-") {
			return fmt.Errorf("%s needs a name before its flags", args[0])
		}
		if args[0] == "agent" {
			positionalAgent = parseArgs[0]
		} else {
			sourceAction = parseArgs[0]
		}
		parseArgs = parseArgs[1:]
	}
	config := f.String("config", def, "configuration path")
	project := f.String("project", os.Getenv("STEALTHBOX_PROJECT"), "project name")
	runnerDefault := os.Getenv("STEALTHBOX_RUNNER")
	if os.Getenv("STEALTHBOX_RUNNER_SOURCE") == "default" {
		runnerDefault = ""
	}
	runner := f.String("runner", runnerDefault, "runner name")
	agent := f.String("agent", "", "agent name")
	session := f.String("session", "tmux", "tmux or shell")
	slot := f.String("slot", "main", "distinct agent instance name")
	dry := f.Bool("dry-run", false, "show plan without connecting or changing files")
	host := f.String("host", "", "SSH config alias")
	remoteDir := f.String("remote-dir", "", "remote state directory")
	binary := f.String("binary", "", "prebuilt remote binary for offline setup")
	sourcePath := f.String("path", "", "checkout path (VM absolute or workspace relative)")
	vmRoot := f.String("vm-root", "", "VM Projects root; ~ is expanded on the VM during setup")
	localRoot := f.String("local-root", "", "local Projects root for explicit source import/export")
	runnerRoot := f.String("runner-root", "", "separate disposable local runner root")
	syncTransport := f.String("sync-transport", "", "auto, rsync or archive")
	shellIntegration := f.Bool("shell-integration", false, "wrap codex/claude only in the managed workspace shell")
	plan := f.String("plan", "", "reviewed source sync plan file")
	expectedHash := f.String("expected-hash", "", "expected destination fingerprint (internal source prepare)")
	sourceExcludes := f.String("source-excludes", "", "JSON exclusion paths (internal source fingerprint/prepare)")
	sourceSafeLinks := f.Bool("source-safe-links", false, "preserve safe relative symlinks (internal source fingerprint/prepare)")
	macPath := f.String("mac-path", "", "dedicated disposable Mac runner path")
	remove := f.Bool("delete", false, "remove project from config; keep files")
	enableMac := f.Bool("enable-mac", false, "enable Mac runner bridge")
	allowExec := f.Bool("allow-mac-exec", false, "enable direct Mac command execution")
	on := f.String("on", "vm", "execution host: vm or mac")
	cwd := f.String("cwd", "", "command directory relative to the selected checkout")
	noAttach := f.Bool("no-attach", false, "create/select window without attaching (VM command)")
	resumeWorkspace := f.Bool("resume-workspace", false, "resume the current workspace window (internal)")
	reconnect := f.Bool("reconnect", true, "reconnect after SSH transport failure")
	artifact := f.String("file", "", "artifact path relative to Mac runner")
	output := f.String("output", "", "local output file (must not exist)")
	edit := f.Bool("edit", false, "edit config with EDITOR")
	if err = f.Parse(parseArgs); err != nil {
		return err
	}
	visited := map[string]bool{}
	f.Visit(func(value *flag.Flag) { visited[value.Name] = true })
	applyWorkspaceFlags := func(c *stealthbox.Config) error {
		oldHost, oldRoot := c.Workspace.Host, c.Workspace.VMRoot
		if visited["host"] {
			c.Workspace.Host = *host
		}
		if visited["remote-dir"] {
			c.Workspace.RemoteDir = *remoteDir
		}
		if visited["vm-root"] {
			c.Workspace.VMRoot = *vmRoot
		}
		if visited["local-root"] {
			c.Workspace.LocalRoot = *localRoot
		}
		if visited["runner-root"] {
			c.Workspace.RunnerRoot = *runnerRoot
		}
		if visited["sync-transport"] {
			c.Workspace.SyncTransport = *syncTransport
		}
		if visited["shell-integration"] {
			c.Workspace.ShellIntegration = *shellIntegration
		}
		if visited["runner"] && (args[0] == "init" || args[0] == "config") {
			c.Workspace.Runner = *runner
		}
		if oldHost != c.Workspace.Host || oldRoot != c.Workspace.VMRoot {
			c.Workspace.ID = ""
		}
		return stealthbox.NormalizeLocalWorkspaceRoots(c)
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
		if !visited["vm-root"] {
			c.Workspace.VMRoot = "~/Projects"
		}
		if e = applyWorkspaceFlags(&c); e != nil {
			return e
		}
		c.Bridge.Enabled = c.Bridge.Enabled || *enableMac
		c.Bridge.AllowExec = *allowExec
		if err = stealthbox.Save(*config, c); err == nil {
			fmt.Println("Created", *config, "(mode 0600). Run stealthbox to prepare and open your tmux workspace.")
		}
		return err
	case "tui", "settings":
		if *dry {
			return fmt.Errorf("--dry-run is not used with TUI")
		}
		return stealthbox.TUI(ctx, *config)
	}
	c, err := stealthbox.Load(*config)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("configuration not found: %s; create it once with stealthbox init --host YOUR_SSH_ALIAS", *config)
		}
		return err
	}
	if err = applyWorkspaceFlags(&c); err != nil {
		return err
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
		changed := false
		for _, name := range []string{"host", "remote-dir", "vm-root", "local-root", "runner-root", "sync-transport", "shell-integration", "runner"} {
			changed = changed || visited[name]
		}
		if changed {
			if *dry {
				return c.Validate()
			}
			if err = stealthbox.Save(*config, c); err != nil {
				return err
			}
		}
		c.Bridge.Token = "[redacted]"
		b, _ := json.MarshalIndent(c, "", "  ")
		fmt.Println(string(b))
		return nil
	case "list":
		if stealthbox.WorkspaceRootEnabled(c) {
			fmt.Printf("workspace\t%s\t%s\n", c.Workspace.Host, c.Workspace.VMRoot)
		}
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
			fmt.Printf("Would prepare %s: tmux, Git, rsync, Codex, Claude, terminal support and validated style.\n", c.Workspace.Host)
			return nil
		}
		return stealthbox.Setup(ctx, &c, *config, *binary, os.Stdout)
	case "theme-apply":
		if *dry {
			return nil
		}
		return stealthbox.ApplyTheme(ctx, c, *artifact, os.Stdout)
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
		if err = stealthbox.EnsureReady(ctx, &c, *config, *binary, os.Stdout); err != nil {
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
	case "source":
		if visited["source-excludes"] || visited["source-safe-links"] {
			if sourceAction != "fingerprint" && sourceAction != "prepare" {
				return fmt.Errorf("--source-excludes and --source-safe-links are only for internal source fingerprint/prepare")
			}
			if visited["source-excludes"] {
				if err = json.Unmarshal([]byte(*sourceExcludes), &c.Workspace.SourceExcludes); err != nil {
					return fmt.Errorf("invalid source exclusions: %w", err)
				}
			}
			if visited["source-safe-links"] {
				c.Workspace.SourceSafeLinks = *sourceSafeLinks
			}
			if err = c.Validate(); err != nil {
				return err
			}
		}
		switch sourceAction {
		case "import", "export":
			if *dry {
				fmt.Printf("Would preview %s %s without changing source files; plan=%s delete=%t\n", sourceAction, *sourcePath, *plan, *remove)
				return nil
			}
			return stealthbox.PreviewSourceSync(ctx, c, sourceAction, *sourcePath, *remove, *plan, os.Stdout)
		case "apply":
			if *dry {
				fmt.Println("Would validate and apply the reviewed source plan", *plan)
				return nil
			}
			return stealthbox.ApplySourceSync(ctx, c, *plan, os.Stdout)
		case "fingerprint":
			state, e := stealthbox.SourceFingerprint(ctx, c, *sourcePath)
			if e != nil {
				return e
			}
			return json.NewEncoder(os.Stdout).Encode(state)
		case "prepare":
			if *dry {
				return fmt.Errorf("--dry-run cannot prepare a source destination")
			}
			return stealthbox.PrepareSourceDestination(ctx, c, *sourcePath, *expectedHash)
		default:
			return fmt.Errorf("source action must be import, export or apply")
		}
	}
	if *sourcePath != "" {
		if visited["project"] && *project != "" && *project != *sourcePath {
			return fmt.Errorf("choose --project or --path")
		}
		*project = *sourcePath
		if stealthbox.WorkspaceRootEnabled(c) {
			if (args[0] == "connect" || args[0] == "open") && !filepath.IsAbs(c.Workspace.VMRoot) {
				if !filepath.IsAbs(*project) {
					*project = "./" + *project
				}
			} else {
				*project, err = stealthbox.WorkspaceCheckoutPath(c, *sourcePath)
				if err != nil {
					return err
				}
			}
		}
	}
	if *project == "" && !stealthbox.WorkspaceRootEnabled(c) {
		if len(c.Projects) == 1 {
			*project = stealthbox.Names(c)[0]
		} else {
			return fmt.Errorf("set --project or STEALTHBOX_PROJECT")
		}
	}
	requestedRunner := *runner
	p, configured := c.Projects[*project]
	if configured {
		*agent, *runner = stealthbox.Defaults(p, *agent, *runner)
	} else {
		if !stealthbox.WorkspaceRootEnabled(c) {
			return fmt.Errorf("unknown project %q", *project)
		}
		if *agent == "" {
			*agent = "shell"
		}
		if *runner == "" {
			*runner = stealthbox.WorkspaceRunner(c)
		}
	}
	switch args[0] {
	case "open", "connect":
		resume := !visited["agent"] && !visited["project"] && !visited["path"] && !visited["slot"] && len(f.Args()) == 0
		if resume {
			*agent = "shell"
		}
		if *session != "tmux" && *session != "shell" {
			return fmt.Errorf("session must be tmux or shell")
		}
		if *dry {
			fmt.Printf("Mac terminal → SSH %s → %s workspace; project=%s agent=%s runner=%s slot=%s; Mac bridge=%t\n", c.Workspace.Host, *session, *project, *agent, *runner, *slot, c.Bridge.Enabled)
			return nil
		}
		return stealthbox.Connect(ctx, &c, *config, stealthbox.ConnectOptions{Project: *project, Agent: *agent, Runner: requestedRunner, Slot: *slot, Binary: *binary, Extra: f.Args(), Reconnect: *reconnect, Session: *session, Resume: resume}, os.Stdout, os.Stderr)
	case "workspace":
		if *dry {
			fmt.Println(stealthbox.WindowName(*project, *agent, *runner, *slot, f.Args()))
			return nil
		}
		if *session == "shell" {
			return stealthbox.PlainRemote(ctx, c, *config, *project, *agent, requestedRunner, f.Args(), os.Stdout, os.Stderr)
		}
		if *resumeWorkspace {
			return stealthbox.ResumeWorkspaceRemote(ctx, c, *config, *project, requestedRunner, !*noAttach, os.Stdout, os.Stderr)
		}
		return stealthbox.WorkspaceRemote(ctx, c, *config, *project, *agent, requestedRunner, *slot, f.Args(), !*noAttach, os.Stdout, os.Stderr)
	case "agent":
		if positionalAgent != "" {
			*agent = positionalAgent
		}
		if *dry {
			fmt.Printf("Would launch %s at %s with runner=%s and a workspace MCP scope\n", *agent, *project, *runner)
			return nil
		}
		return stealthbox.LaunchAgent(ctx, c, *config, *project, *agent, *runner, f.Args(), os.Stdout, os.Stderr)
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
		if target == "local" {
			target = "mac"
		}
		if target == "remote" {
			target = "vm"
		}
		if *dry {
			if !configured {
				fmt.Printf("Workspace runner: path=%s cwd=%s runner=%s sync=%t command=%s\n", *project, *cwd, target, snapshot, stealthbox.JSONString(f.Args()))
				return nil
			}
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

  stealthbox                         Prepare and resume your remote tmux workspace
  stealthbox settings [--config PATH] Optional settings and diagnostics (alias: tui)
  stealthbox init --host dev-vm [--runner local|vm] [--allow-mac-exec]
  stealthbox config --vm-root '~/Projects' --local-root ~/Projects --runner-root ~/.local/share/stealthbox/runners --shell-integration
  stealthbox project --project NAME --path /home/user/project --mac-path /Users/user/runner
  stealthbox setup [--binary PATH]    Prepare VM tools, Codex, Claude, binary, terminal and style
  stealthbox theme                    Export current local tmux options/key bindings
  stealthbox connect --project NAME --agent codex --runner mac [--slot main]
  stealthbox connect                 Same as bare stealthbox: resume the active workspace window
  stealthbox agent codex|claude|shell [--path CHECKOUT] [--runner local|vm] -- ARGS
  stealthbox open ...                 Alias for connect; --session shell skips tmux
  stealthbox run [--path CHECKOUT] [--cwd RELATIVE] [--runner local|vm] -- COMMAND [ARGS...]
  stealthbox exec --on vm|local [--path CHECKOUT] [--cwd RELATIVE] -- COMMAND [ARGS...]
  stealthbox fetch --file REL --output PATH   Download a Mac artifact to the VM
  stealthbox doctor                   Check prerequisites and live connectivity
  stealthbox bridge                  Foreground Mac runner + reverse SSH tunnel
  stealthbox bridge-start|bridge-stop|bridge-status  Manage background Mac bridge
  stealthbox config [--edit]          Show redacted config or edit with EDITOR
  stealthbox list                     List projects
  stealthbox source import|export --path REL --plan FILE [--delete]
                                     Preview local → VM / VM → local transfer; source files stay unchanged
  stealthbox source apply --plan FILE Apply reviewed plan only if both sides are unchanged
  stealthbox mcp                      MCP stdio tools (used automatically by Codex/Claude)

All command flags precede -- and command arguments. --dry-run makes no changes.
Config: STEALTHBOX_CONFIG or ~/.config/stealthbox/config.json.
Project/runner defaults: STEALTHBOX_PROJECT / STEALTHBOX_RUNNER.
Workspace mode permits per-call checkout paths; local sources and disposable runners stay separate.
Connect from a terminal OUTSIDE your local tmux. On the VM, tmux uses its own
stealthbox socket and never modifies unrelated sessions or agent credentials.`)
}
