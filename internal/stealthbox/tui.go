package stealthbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

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
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, e := unix.Poll(fds, 200)
		if e != nil {
			return -1, e
		}
		if n == 0 {
			continue
		}
		if _, err = os.Stdin.Read(b); err != nil {
			return -1, err
		}
		switch b[0] {
		case 3, 'q', 'Q':
			return -1, nil
		case 27:
			seq := make([]byte, 2)
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, e := unix.Poll(poll, 60)
			if e != nil {
				return -1, e
			}
			if n == 0 {
				return -1, nil
			}
			if _, err = os.Stdin.Read(seq[:1]); err != nil {
				return -1, err
			}
			if seq[0] != '[' {
				return -1, nil
			}
			if _, err = os.Stdin.Read(seq[1:]); err != nil {
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
		i, e := choose(ctx, "VM: "+c.Workspace.Host+" · Mac bridge: "+state, []string{"Connect / подключиться", "Projects / проекты", "VM and workspace / настройки ВМ", "Mac runner / доступ к Mac", "Agents / команды агентов", "Import current tmux style / оформление", "Deploy or update on VM / установка", "Doctor / диагностика", "Exit"}, 0)
		if e != nil {
			return e
		}
		if i < 0 || i == 8 {
			return nil
		}
		switch i {
		case 0:
			if len(c.Projects) == 0 {
				fmt.Println("Add a project first.")
				pauseTUI(ctx)
				continue
			}
			names := Names(c)
			idx, e := choose(ctx, "Choose a project", names, 0)
			if e != nil {
				return e
			}
			if idx < 0 {
				continue
			}
			name := names[idx]
			p := c.Projects[name]
			agents := []string{}
			for n := range c.Agents {
				agents = append(agents, n)
			}
			if len(agents) == 0 {
				agents = []string{"codex", "claude", "shell"}
			}
			sort.Strings(agents)
			ai, e := choose(ctx, "Choose an agent", agents, 0)
			if e != nil {
				return e
			}
			if ai < 0 {
				continue
			}
			runners := []string{}
			for n := range p.Runners {
				runners = append(runners, n)
			}
			sort.Strings(runners)
			ri, e := choose(ctx, "Docker/test runner", runners, 0)
			if e != nil {
				return e
			}
			if ri < 0 {
				continue
			}
			slot, e := prompt(ctx, "Session name (another name = another agent instance)", "main")
			if e != nil {
				return e
			}
			err = Connect(ctx, &c, path, ConnectOptions{Project: name, Agent: agents[ai], Runner: runners[ri], Slot: slot, Reconnect: true}, os.Stdout, os.Stderr)
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
			next := cloneConfig(c)
			oldHost := next.Workspace.Host
			next.Workspace.Host, err = prompt(ctx, "SSH alias", c.Workspace.Host)
			if err == nil {
				next.Workspace.Name, err = prompt(ctx, "Workspace name", c.Workspace.Name)
			}
			if err == nil {
				next.Workspace.RemoteDir, err = prompt(ctx, "Remote state directory (empty = detect HOME)", c.Workspace.RemoteDir)
			}
			if err == nil {
				if oldHost != next.Workspace.Host {
					next.Workspace.RemoteDir = ""
					next.Bridge.RemoteSocket = ""
					for n, p := range next.Projects {
						if p.Source.Host == oldHost {
							p.Source.Host = next.Workspace.Host
							vm := p.Runners["vm"]
							vm.Host = next.Workspace.Host
							p.Runners["vm"] = vm
							next.Projects[n] = p
						}
					}
				}
				err = Save(path, next)
			}
			if err == nil {
				c = next
			} else {
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
					if c.Workspace.RemoteDir == "" {
						err = Setup(ctx, &c, path, "", os.Stdout)
					} else {
						err = DeployConfig(ctx, c)
					}
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
			next := cloneConfig(c)
			name, e := prompt(ctx, "Agent name", "codex")
			if e != nil {
				return e
			}
			current := next.Agents[name]
			if len(current) == 0 {
				current = []string{name}
			}
			raw, e := prompt(ctx, "Command as JSON argv", JSONString(current))
			if e != nil {
				return e
			}
			var argv []string
			if e = json.Unmarshal([]byte(raw), &argv); e != nil {
				fmt.Println(e)
				pauseTUI(ctx)
				continue
			}
			if next.Agents == nil {
				next.Agents = map[string][]string{}
			}
			next.Agents[name] = argv
			if err = Save(path, next); err == nil {
				c = next
			} else {
				fmt.Println(err)
				pauseTUI(ctx)
			}
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
		}
	}
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

func readLine(ctx context.Context) (string, error) {
	var b strings.Builder
	one := make([]byte, 1)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
		ready, err := unix.Poll(fds, 200)
		if err != nil {
			return "", err
		}
		if ready == 0 {
			continue
		}
		n, e := os.Stdin.Read(one)
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
