package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/mcoder33/stealthbox/internal/stealthbox"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "stealthbox:", err)
		var e *exec.ExitError
		if errors.As(err, &e) {
			os.Exit(e.ExitCode())
		}
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	if args[0] == "help" || args[0] == "--help" {
		usage()
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	def := os.Getenv("STEALTHBOX_CONFIG")
	if def == "" {
		def = filepath.Join(home, ".config", "stealthbox", "config.json")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	config := f.String("config", def, "configuration path")
	project := f.String("project", os.Getenv("STEALTHBOX_PROJECT"), "project name")
	runner := f.String("runner", envDefault("STEALTHBOX_RUNNER", "vm"), "runner name")
	session := f.String("session", "tmux", "tmux or shell")
	dry := f.Bool("dry-run", false, "print commands without executing")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] == "init" {
		if f.NArg() != 0 {
			return fmt.Errorf("unexpected arguments")
		}
		if err := os.MkdirAll(filepath.Dir(*config), 0700); err != nil {
			return err
		}
		file, err := os.OpenFile(*config, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = file.WriteString(example)
		if err == nil {
			fmt.Println("Created", *config, "— edit the example paths and SSH aliases before use")
		}
		return err
	}
	c, err := stealthbox.Load(*config)
	if err != nil {
		return err
	}
	if args[0] == "list" {
		for _, n := range stealthbox.Names(c) {
			fmt.Println(n)
		}
		return nil
	}
	if *project == "" {
		return fmt.Errorf("set --project or STEALTHBOX_PROJECT (see stealthbox list)")
	}
	p, ok := c.Projects[*project]
	if !ok {
		return fmt.Errorf("unknown project %q", *project)
	}
	var cmds []stealthbox.Command
	switch args[0] {
	case "open":
		cmd, err := stealthbox.Open(p, *project, *session, *runner, f.Args())
		if err != nil {
			return err
		}
		cmds = []stealthbox.Command{cmd}
	case "run":
		cmds, err = stealthbox.PlanRun(p, *runner, f.Args())
		if err != nil {
			return err
		}
	case "doctor":
		if f.NArg() != 0 {
			return fmt.Errorf("unexpected arguments")
		}
		dst, ok := p.Runners[*runner]
		if !ok {
			return fmt.Errorf("unknown runner %q", *runner)
		}
		script := "test -d " + stealthbox.Quote(p.Source.Path) + " && command -v sh && command -v rsync"
		if *session == "tmux" {
			script += " && command -v tmux"
		}
		cmds = append(cmds, stealthbox.At(p.Source, script, false))
		cmds = append(cmds, stealthbox.At(dst, "command -v rsync && docker info >/dev/null && docker compose version", false))
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	for _, cmd := range cmds {
		if *dry {
			fmt.Printf("%s", cmd.Program)
			for _, a := range cmd.Args {
				fmt.Printf(" %s", stealthbox.Quote(a))
			}
			fmt.Println()
			continue
		}
		if err := stealthbox.Execute(cmd, os.Stdin, os.Stdout, os.Stderr); err != nil {
			return err
		}
	}
	return nil
}
func envDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func usage() {
	fmt.Println(`stealthbox — terminal sessions and selectable execution hosts

  stealthbox init [--config PATH]
  stealthbox list [--config PATH]
  stealthbox open --project NAME [--session tmux|shell] [--runner vm|mac] [-- codex]
  stealthbox run --project NAME [--runner vm|mac] -- COMMAND [ARG...]
  stealthbox doctor --project NAME [--runner vm|mac]

All flags precede the command arguments. Add --dry-run to inspect commands.
Config: STEALTHBOX_CONFIG or ~/.config/stealthbox/config.json.
Project/runner defaults: STEALTHBOX_PROJECT and STEALTHBOX_RUNNER.`)
}

const example = `{
  "projects": {
    "example": {
      "source": {"host": "dev-vm", "path": "/home/developer/projects/example"},
      "runners": {
        "vm": {"host": "dev-vm", "path": "/home/developer/projects/example"},
        "mac": {"path": "/Users/developer/.local/share/stealthbox/runners/example"}
      }
    }
  }
}
`
