package stealthbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Go's asynchronous preemption and terminal resize signals may interrupt poll.
// They are not user cancellation or terminal errors.
func pollTerminal(ctx context.Context, fd int, timeout int) (int, error) {
	deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		remaining := time.Until(deadline).Milliseconds()
		if remaining < 0 {
			return 0, nil
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(remaining)+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return n, err
	}
}

func readTerminal(b []byte) (int, error) {
	for {
		n, err := os.Stdin.Read(b)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return n, err
	}
}

func choose(ctx context.Context, title string, items []string, selected int) (int, error) {
	if len(items) == 0 {
		return -1, fmt.Errorf("no choices available")
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return -1, fmt.Errorf("TUI requires an interactive terminal; use CLI commands for scripts")
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return -1, err
	}
	defer term.Restore(fd, old)
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	fmt.Print("\x1b[?1049h\x1b[?25l")
	draw := func() {
		width, _, _ := term.GetSize(fd)
		if width < 30 {
			width = 80
		}
		fmt.Print("\x1b[2J\x1b[H\x1b[38;5;183m  STEALTH BOX\x1b[0m\r\n\r\n")
		fmt.Print("  " + clip(title, width-4) + "\r\n\r\n")
		for i, s := range items {
			prefix := "    "
			if i == selected {
				prefix = "  › "
				fmt.Print("\x1b[38;5;151m")
			}
			fmt.Print(prefix + clip(s, width-6) + "\x1b[0m\r\n")
		}
		fmt.Print("\r\n\x1b[90m  ↑/↓ or j/k · Enter select · q/Esc back\x1b[0m\r\n")
	}
	draw()
	b := make([]byte, 1)
	for {
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		n, e := pollTerminal(ctx, fd, 200)
		if e != nil {
			return -1, e
		}
		if n == 0 {
			continue
		}
		if _, err = readTerminal(b); err != nil {
			return -1, err
		}
		switch b[0] {
		case 3, 'q', 'Q':
			return -1, nil
		case 27:
			seq := make([]byte, 2)
			n, e := pollTerminal(ctx, fd, 60)
			if e != nil {
				return -1, e
			}
			if n == 0 {
				return -1, nil
			}
			if _, err = readTerminal(seq[:1]); err != nil {
				return -1, err
			}
			if seq[0] != '[' {
				return -1, nil
			}
			if _, err = readTerminal(seq[1:]); err != nil {
				return -1, err
			}
			if seq[1] == 'A' {
				selected = (selected + len(items) - 1) % len(items)
			}
			if seq[1] == 'B' {
				selected = (selected + 1) % len(items)
			}
		case 'k':
			selected = (selected + len(items) - 1) % len(items)
		case 'j':
			selected = (selected + 1) % len(items)
		case 13, 10:
			return selected, nil
		}
		draw()
	}
}
func clip(s string, n int) string {
	r := []rune(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return string(r)
}
func prompt(ctx context.Context, label, current string) (string, error) {
	fmt.Printf("%s [%s]: ", label, current)
	s, err := readLine(ctx)
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		s = current
	}
	return s, nil
}
func pauseTUI(ctx context.Context) { fmt.Print("\nEnter to return…"); _, _ = readLine(ctx) }
func cloneConfig(c Config) Config {
	b, _ := json.Marshal(c)
	var n Config
	_ = json.Unmarshal(b, &n)
	return n
}
func TUI(ctx context.Context, path string) error {
	c, err := Load(path)
	if os.IsNotExist(err) {
		c, err = DefaultConfig()
		if err == nil {
			c.Workspace.VMRoot = "~/Projects"
			err = NormalizeLocalWorkspaceRoots(&c)
		}
		if err == nil {
			err = Save(path, c)
		}
	}
	if err != nil {
		return err
	}
	for {
		state := "off"
		if c.Bridge.Enabled {
			state = "on"
		}
		i, e := choose(ctx, "VM: "+c.Workspace.Host+" · Mac bridge: "+state, []string{"Continue in tmux / продолжить работу", "Projects / проекты", "VM and workspace / настройки ВМ", "Mac runner / доступ к Mac", "Agent environment / Codex и Claude", "Import current tmux style / оформление", "Prepare or update VM / подготовка среды", "Doctor / диагностика", "Source import/export / перенос исходников", "Exit"}, 0)
		if e != nil {
			return e
		}
		if i < 0 || i == 9 {
			return nil
		}
		switch i {
		case 0:
			if len(c.Projects) == 0 && !WorkspaceRootEnabled(c) {
				fmt.Println("Configure a Projects root or add a project first.")
				pauseTUI(ctx)
				continue
			}
			err = Connect(ctx, &c, path, ConnectOptions{Agent: "shell", Reconnect: true, Session: "tmux", Resume: true}, os.Stdout, os.Stderr)
			if err != nil {
				fmt.Println("Error:", err)
			}
			pauseTUI(ctx)
		case 1:
			err = editProject(ctx, &c, path)
			if err != nil {
				fmt.Println("Error:", err)
				pauseTUI(ctx)
			}
		case 2:
			if err = editWorkspace(ctx, &c, path); err != nil {
				fmt.Println(err)
				pauseTUI(ctx)
			}
		case 3:
			items := []string{fmt.Sprintf("Mac bridge enabled: %t — toggle", c.Bridge.Enabled), fmt.Sprintf("Direct Mac commands: %t — toggle", c.Bridge.AllowExec), "Change timeout", "Start / apply bridge settings", "Stop bridge", "Bridge status", "Back"}
			mi, e := choose(ctx, "Mac-user permissions; NOT a sandbox. Restart connection to apply.", items, 0)
			if e != nil {
				return e
			}
			if mi < 0 || mi == 6 {
				continue
			}
			if mi >= 3 {
				switch mi {
				case 3:
					err = EnsureReady(ctx, &c, path, "", os.Stdout)
					if err == nil {
						err = StartBridgeService(ctx, c, path, os.Stdout)
					}
					if err == nil {
						err = WaitRemoteBridge(ctx, c)
					}
				case 4:
					err = StopBridgeService(ctx, c)
				case 5:
					var state BridgeState
					state, err = BridgeStatus(ctx, c)
					if err == nil {
						fmt.Printf("Online; direct execution=%t\n", state.AllowExec)
					}
				}
				if err != nil {
					fmt.Println(err)
				}
				pauseTUI(ctx)
				continue
			}
			next := cloneConfig(c)
			switch mi {
			case 0:
				next.Bridge.Enabled = !next.Bridge.Enabled
				if !next.Bridge.Enabled && runnerAlias(next.Workspace.Runner) == "mac" {
					next.Workspace.Runner = "vm"
				}
			case 1:
				next.Bridge.AllowExec = !next.Bridge.AllowExec
			case 2:
				s, e := prompt(ctx, "Command timeout (seconds)", fmt.Sprint(next.Bridge.TimeoutSeconds))
				if e != nil {
					return e
				}
				if _, e = fmt.Sscan(s, &next.Bridge.TimeoutSeconds); e != nil {
					fmt.Println(e)
					pauseTUI(ctx)
					continue
				}
			}
			if err = Save(path, next); err == nil {
				c = next
			} else {
				fmt.Println(err)
				pauseTUI(ctx)
			}
		case 4:
			fmt.Println("Codex and Claude are part of the prepared environment. Start either command in any tmux window; the native CLI handles first-time sign-in.")
			err = ExecuteContext(ctx, sshCommand(c.Workspace.Host, remoteToolPath+`; for tool in codex claude; do command -v "$tool" || printf '%s: needs preparation\n' "$tool"; done`, false), nil, os.Stdout, os.Stderr)
			if err != nil {
				fmt.Println(err)
			}
			pauseTUI(ctx)
		case 5:
			var data []byte
			data, err = ThemeSnapshot(ctx)
			if err == nil {
				theme := filepath.Join(filepath.Dir(path), "tmux.conf")
				err = os.WriteFile(theme, data, 0600)
				if err == nil {
					next := cloneConfig(c)
					next.Workspace.Theme = theme
					err = Save(path, next)
					if err == nil {
						c = next
					}
				}
			}
			if err == nil {
				fmt.Println("Imported tmux options and bindings. run-shell plugin bindings are omitted.")
			} else {
				fmt.Println(err)
			}
			pauseTUI(ctx)
		case 6:
			err = Setup(ctx, &c, path, "", os.Stdout)
			if err != nil {
				fmt.Println(err)
			}
			pauseTUI(ctx)
		case 7:
			err = Doctor(ctx, c, os.Stdout)
			if err != nil {
				fmt.Println(err)
			}
			pauseTUI(ctx)
		case 8:
			err = sourceSyncTUI(ctx, c, path)
			if err != nil {
				fmt.Println("Error:", err)
				pauseTUI(ctx)
			}
		}
	}
}

func sourceSyncTUI(ctx context.Context, c Config, configPath string) error {
	if !WorkspaceRootEnabled(c) {
		return fmt.Errorf("configure a Whole Projects workspace first")
	}
	i, err := choose(ctx, "Source files: preview first, explicit apply; VM remains authoritative", []string{"Import local → VM", "Export VM → local", "Apply an existing reviewed plan", "Back"}, 0)
	if err != nil || i < 0 || i == 3 {
		return err
	}
	planFile, err := prompt(ctx, "Local plan file", filepath.Join(filepath.Dir(configPath), "source-plan.json"))
	if err != nil {
		return err
	}
	if i == 2 {
		return ApplySourceSync(ctx, c, planFile, os.Stdout)
	}
	checkout, err := prompt(ctx, "Checkout path relative to Projects (for example team/repo)", "")
	if err != nil {
		return err
	}
	deletion, err := choose(ctx, "Files only present at destination", []string{"Keep them", "Include deletion in the reviewed plan"}, 0)
	if err != nil || deletion < 0 {
		return err
	}
	direction := "import"
	if i == 1 {
		direction = "export"
	}
	if err = PreviewSourceSync(ctx, c, direction, checkout, deletion == 1, planFile, os.Stdout); err != nil {
		return err
	}
	pauseTUI(ctx)
	apply, err := choose(ctx, "Preview saved; both sides will be checked again before apply", []string{"Keep preview / back", "Apply this reviewed plan"}, 0)
	if err != nil || apply != 1 {
		return err
	}
	return ApplySourceSync(ctx, c, planFile, os.Stdout)
}
func editProject(ctx context.Context, c *Config, path string) error {
	next := cloneConfig(*c)
	names := Names(next)
	items := append([]string{"Add project"}, names...)
	i, err := choose(ctx, "Projects", items, 0)
	if err != nil || i < 0 {
		return err
	}
	name := ""
	if i > 0 {
		name = names[i-1]
		action, e := choose(ctx, name, []string{"Edit settings", "Remove from config (files stay)", "Back"}, 0)
		if e != nil {
			return e
		}
		if action < 0 || action == 2 {
			return nil
		}
		if action == 1 {
			delete(next.Projects, name)
			if err = Save(path, next); err == nil {
				*c = next
			}
			return err
		}
	}
	name, err = prompt(ctx, "Project name", name)
	if err != nil {
		return err
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("use letters, digits, underscore or hyphen")
	}
	p := next.Projects[name]
	if p.Runners == nil {
		p.Runners = map[string]Endpoint{}
	}
	p.Source.Host = next.Workspace.Host
	p.Source.Path, err = prompt(ctx, "Absolute project path on VM", p.Source.Path)
	if err != nil {
		return err
	}
	mac := p.Runners["mac"]
	if mac.Path == "" {
		home, _ := os.UserHomeDir()
		mac.Path = filepath.Join(home, ".local", "share", "stealthbox", "runners", name)
	}
	mac.Path, err = prompt(ctx, "Disposable Mac runner directory", mac.Path)
	if err != nil {
		return err
	}
	p.Runners["mac"] = mac
	p.Runners["vm"] = p.Source
	p.Agent, err = prompt(ctx, "Default agent", envDefaultValue(p.Agent, "codex"))
	if err != nil {
		return err
	}
	p.Runner, err = prompt(ctx, "Default runner", envDefaultValue(p.Runner, "vm"))
	if err != nil {
		return err
	}
	next.Projects[name] = p
	if err = Save(path, next); err == nil {
		*c = next
	}
	return err
}
func envDefaultValue(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func editWorkspace(ctx context.Context, c *Config, path string) error {
	next := cloneConfig(*c)
	var err error
	next.Workspace.Host, err = prompt(ctx, "SSH alias", c.Workspace.Host)
	if err != nil {
		return err
	}
	next.Workspace.VMRoot, err = prompt(ctx, "Projects on the VM", envDefaultValue(c.Workspace.VMRoot, "~/Projects"))
	if err != nil {
		return err
	}
	next.Workspace.LocalRoot, err = prompt(ctx, "Local projects (for explicit import/export)", envDefaultValue(c.Workspace.LocalRoot, "~/Projects"))
	if err != nil {
		return err
	}
	selected := 0
	if WorkspaceRunner(*c) == "mac" {
		selected = 1
	}
	i, err := choose(ctx, "Default place for Docker/tests", []string{"VM / на ВМ", "This computer / на этом компьютере"}, selected)
	if err != nil || i < 0 {
		return err
	}
	next.Workspace.Runner = []string{"vm", "local"}[i]
	if next.Workspace.Host != c.Workspace.Host {
		next.Workspace.ID, next.Workspace.RemoteDir, next.Bridge.RemoteSocket = "", "", ""
		for n, p := range next.Projects {
			if p.Source.Host == c.Workspace.Host {
				p.Source.Host = next.Workspace.Host
				vm := p.Runners["vm"]
				vm.Host = next.Workspace.Host
				p.Runners["vm"] = vm
				next.Projects[n] = p
			}
		}
	}
	if next.Workspace.VMRoot != c.Workspace.VMRoot {
		next.Workspace.ID = ""
	}
	if err = NormalizeLocalWorkspaceRoots(&next); err != nil {
		return err
	}
	if err = Save(path, next); err == nil {
		*c = next
	}
	return err
}

func readLine(ctx context.Context) (string, error) {
	var b strings.Builder
	one := make([]byte, 1)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		ready, err := pollTerminal(ctx, int(os.Stdin.Fd()), 200)
		if err != nil {
			return "", err
		}
		if ready == 0 {
			continue
		}
		n, e := readTerminal(one)
		if e != nil {
			return "", e
		}
		if n == 0 {
			return "", io.EOF
		}
		if one[0] == '\n' {
			return b.String(), nil
		}
		if b.Len() > 16384 {
			return "", fmt.Errorf("input exceeds limit")
		}
		b.WriteByte(one[0])
	}
}
