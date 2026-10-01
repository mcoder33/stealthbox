package stealthbox

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Endpoint struct {
	Host string `json:"host,omitempty"`
	Path string `json:"path"`
}
type Project struct {
	Source  Endpoint            `json:"source"`
	Runners map[string]Endpoint `json:"runners"`
	Session string              `json:"session,omitempty"`
}
type Config struct {
	Projects map[string]Project `json:"projects"`
}
type Command struct {
	Program string
	Args    []string
}

func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func shellArgs(args []string) string {
	a := make([]string, len(args))
	for i, s := range args {
		a[i] = Quote(s)
	}
	return strings.Join(a, " ")
}

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
var hostRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.@-]*$`)

func ValidateEndpoint(e Endpoint) error {
	if !filepath.IsAbs(e.Path) || filepath.Clean(e.Path) == "/" || strings.ContainsAny(e.Path, "\n\r\x00") {
		return fmt.Errorf("path must be absolute and must not be root: %q", e.Path)
	}
	if e.Host != "" && !hostRE.MatchString(e.Host) {
		return fmt.Errorf("use a safe SSH config alias for host: %q", e.Host)
	}
	return nil
}
func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	for name, p := range c.Projects {
		if !nameRE.MatchString(name) {
			return c, fmt.Errorf("invalid project name %q", name)
		}
		if err = ValidateEndpoint(p.Source); err != nil {
			return c, err
		}
		for n, e := range p.Runners {
			if !nameRE.MatchString(n) {
				return c, fmt.Errorf("invalid runner name %q", n)
			}
			if err = ValidateEndpoint(e); err != nil {
				return c, err
			}
		}
	}
	return c, nil
}
func At(e Endpoint, script string, tty bool) Command {
	if e.Host == "" {
		return Command{"sh", []string{"-c", script}}
	}
	args := []string{"-o", "BatchMode=yes"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, e.Host, script)
	return Command{"ssh", args}
}
func Open(p Project, name, session, runner string, command []string) (Command, error) {
	if _, ok := p.Runners[runner]; !ok {
		return Command{}, fmt.Errorf("runner %q is not configured", runner)
	}
	if session != "tmux" && session != "shell" {
		return Command{}, fmt.Errorf("session must be tmux or shell")
	}
	script := ""
	if session == "shell" {
		if len(command) > 0 {
			script = "cd " + Quote(p.Source.Path) + " && env STEALTHBOX_PROJECT=" + Quote(name) + " STEALTHBOX_RUNNER=" + Quote(runner) + " " + shellArgs(command)
		} else {
			script = "cd " + Quote(p.Source.Path) + " && export STEALTHBOX_PROJECT=" + Quote(name) + " STEALTHBOX_RUNNER=" + Quote(runner) + " && exec \"${SHELL:-/bin/sh}\" -l"
		}
	} else {
		s := p.Session
		if s == "" {
			s = "stealthbox-" + name + "-" + runner
		}
		if !nameRE.MatchString(s) {
			return Command{}, fmt.Errorf("invalid tmux session name")
		}
		inner := "env STEALTHBOX_PROJECT=" + Quote(name) + " STEALTHBOX_RUNNER=" + Quote(runner) + " "
		if len(command) > 0 {
			inner += shellArgs(command)
		} else {
			inner += "\"${SHELL:-/bin/sh}\" -l"
		}
		script = "cd " + Quote(p.Source.Path) + " && exec tmux new-session -A -s " + Quote(s) + " -c " + Quote(p.Source.Path) + " " + Quote(inner)
	}
	return At(p.Source, script, true), nil
}
func location(e Endpoint) string {
	if e.Host == "" {
		return e.Path + "/"
	}
	return e.Host + ":" + e.Path + "/"
}
func PlanRun(p Project, runner string, args []string) ([]Command, error) {
	dst, ok := p.Runners[runner]
	if !ok {
		return nil, fmt.Errorf("runner %q is not configured", runner)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("provide a command after --")
	}
	same := p.Source.Host == dst.Host && filepath.Clean(p.Source.Path) == filepath.Clean(dst.Path)
	cmds := []Command{}
	if !same {
		if p.Source.Host != "" && dst.Host != "" {
			return nil, fmt.Errorf("remote-to-remote sync is unsupported; run stealthbox on the source host or runner host")
		}
		// Guard disposable runner directories before rsync can delete stale files.
		if p.Source.Host == dst.Host && (strings.HasPrefix(filepath.Clean(dst.Path)+"/", filepath.Clean(p.Source.Path)+"/") || strings.HasPrefix(filepath.Clean(p.Source.Path)+"/", filepath.Clean(dst.Path)+"/")) {
			return nil, fmt.Errorf("source and runner directories must not overlap")
		}
		cmds = append(cmds, At(dst, "mkdir -p "+Quote(dst.Path)+" && cd "+Quote(dst.Path)+" && { test -f .stealthbox-runner || test -z \"$(ls -A)\"; } && touch .stealthbox-runner", false))
		cmds = append(cmds, Command{"rsync", []string{"-az", "--delete-delay", "--protect-args", "--exclude=.stealthbox-runner", "--exclude=.git", "--exclude=.env", "--exclude=.env.*", "--exclude=node_modules/", "--exclude=vendor/", "--exclude=.serena/", "-e", "ssh -o BatchMode=yes", "--", location(p.Source), location(dst)}})
	}
	cmds = append(cmds, At(dst, "cd "+Quote(dst.Path)+" && "+shellArgs(args), false))
	return cmds, nil
}
func Execute(c Command, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command(c.Program, c.Args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
func Names(c Config) []string {
	a := []string{}
	for n := range c.Projects {
		a = append(a, n)
	}
	sort.Strings(a)
	return a
}
