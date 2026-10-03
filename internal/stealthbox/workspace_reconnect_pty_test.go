package stealthbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func privateWorkspaceCLI(t *testing.T) string {
	t.Helper()
	cli := filepath.Join(t.TempDir(), "stealthbox")
	build := exec.Command("go", "build", "-o", cli, "./cmd/stealthbox")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	return cli
}

func waitWorkspaceCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out: " + description)
}

// Every terminal and process group belongs to this fixture. A real CLI avoids
// reexecuting the Go test binary through service or SSH entry points.
func workspacePTY(t *testing.T, cli string, args ...string) (io.Writer, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for isolated terminal fixture")
	}
	transcript := filepath.Join(t.TempDir(), "terminal.log")
	script := `import os, pty, select, signal, sys, time, fcntl, termios, struct
pid, master = pty.fork()
if pid == 0:
    os.environ['TERM'] = 'xterm-256color'
    os.execv(sys.argv[2], sys.argv[2:])
fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 100, 0, 0))
stopping = False
def stop(signum, frame):
    global stopping
    stopping = True
signal.signal(signal.SIGTERM, stop)
try:
    with open(sys.argv[1], 'wb', buffering=0) as output:
        while not stopping:
            readable, _, _ = select.select([master, 0], [], [], .1)
            if master in readable:
                try:
                    chunk = os.read(master, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                output.write(chunk)
            if 0 in readable:
                chunk = os.read(0, 65536)
                if not chunk:
                    break
                os.write(master, chunk)
finally:
    done, _ = os.waitpid(pid, os.WNOHANG)
    if not done:
        try:
            os.killpg(pid, signal.SIGKILL)
        except PermissionError:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        # Close the master before reaping: macOS terminal revocation during
        # child exit can wait for this descriptor to close.
        os.close(master)
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            done, _ = os.waitpid(pid, os.WNOHANG)
            if done:
                break
            time.sleep(.02)
        else:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
    else:
        os.close(master)
`
	argv := append([]string{"-c", script, transcript, cli}, args...)
	cmd := exec.Command(python, argv...)
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = input.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("terminal fixture cleanup: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("terminal fixture did not reap its owned process group")
		}
	})
	return input, transcript
}

func TestWorkspaceAttachUpdatesRootPreservesExplicitDirectories(t *testing.T) {
	tmux := isolatedTmux(t)
	c := reconnectFixture(t)
	c.Workspace.Host = ""
	config := filepath.Join(c.Workspace.RemoteDir, "local.json")
	if err := Save(config, c); err != nil {
		t.Fatal(err)
	}
	if err := ResumeWorkspaceRemote(context.Background(), c, config, "", "", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	before := tmux("display-message", "-p", "-t", "stealthbox:shell", "#{pane_pid}")
	oldRoot := c.Workspace.VMRoot
	c.Workspace.VMRoot = filepath.Join(filepath.Dir(oldRoot), "Work")
	checkout := filepath.Join(c.Workspace.VMRoot, "b", "api")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Save(config, c); err != nil {
		t.Fatal(err)
	}
	input, _ := workspacePTY(t, privateWorkspaceCLI(t), "workspace", "--config", config, "--resume-workspace")
	waitWorkspaceCondition(t, "actual tmux attach", func() bool {
		return tmux("list-clients", "-t", "stealthbox", "-F", "#{client_pid}") != ""
	})
	if after := tmux("display-message", "-p", "-t", "stealthbox:shell", "#{pane_pid}"); after != before {
		t.Fatal("attach replaced existing shell", before, after)
	}
	// An attached client's ordinary new-window key uses the session cwd.
	// A command client launched from outside tmux uses its own cwd instead.
	_, _ = io.WriteString(input, "\x02c")
	waitWorkspaceCondition(t, "ordinary attached-client new window", func() bool {
		return len(strings.Split(tmux("list-windows", "-t", "stealthbox", "-F", "#{window_id}"), "\n")) == 2
	})
	ordinary := tmux("display-message", "-p", "-t", "stealthbox", "#{pane_id}")
	tmux("send-keys", "-t", ordinary, "printf ready > "+Quote(filepath.Join(c.Workspace.VMRoot, "ready-default")), "Enter")
	waitWorkspaceCondition(t, "ordinary shell startup", func() bool {
		_, err := os.Stat(filepath.Join(c.Workspace.VMRoot, "ready-default"))
		return err == nil
	})
	if actual := tmux("display-message", "-p", "-t", ordinary, "#{pane_current_path}"); actual != c.Workspace.VMRoot {
		t.Fatalf("new-window from attached client: got %q want %q", actual, c.Workspace.VMRoot)
	}
	for index, directory := range []string{checkout, oldRoot} {
		window := fmt.Sprintf("cwd-%d", index)
		args := []string{"new-window", "-t", "stealthbox", "-n", window}
		args = append(args, "-c", directory)
		target := tmux(append(args, "-P", "-F", "#{pane_id}")...)
		// Wait until managed shell startup has finished, rather than observing
		// the initial tmux cwd before default-command has had time to change it.
		tmux("send-keys", "-t", target, "printf ready > "+Quote(filepath.Join(directory, "ready-window")), "Enter")
		waitWorkspaceCondition(t, "window startup", func() bool {
			_, err := os.Stat(filepath.Join(directory, "ready-window"))
			return err == nil
		})
		if actual := tmux("display-message", "-p", "-t", target, "#{pane_current_path}"); actual != directory {
			t.Fatalf("window -c was overwritten: got %q want %q", actual, directory)
		}
		pane := tmux("split-window", "-t", target, "-c", directory, "-P", "-F", "#{pane_id}")
		tmux("send-keys", "-t", pane, "printf ready > "+Quote(filepath.Join(directory, "ready-pane")), "Enter")
		waitWorkspaceCondition(t, "pane startup", func() bool {
			_, err := os.Stat(filepath.Join(directory, "ready-pane"))
			return err == nil
		})
		if actual := tmux("display-message", "-p", "-t", pane, "#{pane_current_path}"); actual != directory {
			t.Fatalf("pane -c was overwritten: got %q want %q", actual, directory)
		}
	}
}

func TestTUIContinueUsesCurrentDefaultInRetainedShell(t *testing.T) {
	tmux := isolatedTmux(t)
	c := reconnectFixture(t)
	// This fixture exercises runner selection without a Mac bridge server.
	localContext := false
	c.Bridge.LocalContext = &localContext
	c.Bridge.Enabled = false
	cli := privateWorkspaceCLI(t)
	data, err := os.ReadFile(cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox"), data, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(c.Workspace.RemoteDir)
	started := filepath.Join(root, "shell-started")
	agentLog := filepath.Join(root, "agents")
	if err := os.WriteFile(filepath.Join(root, ".bashrc"), []byte("printf '%s|%s\\n' \"$STEALTHBOX_RUNNER_SOURCE\" \"$STEALTHBOX_RUNNER\" > "+Quote(started)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "claude"} {
		program := filepath.Join(root, name)
		if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s|%s\\n' \"$STEALTHBOX_RUNNER\" \"$STEALTHBOX_RUNNER_SOURCE\" >> "+Quote(agentLog)+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		c.Agents[name] = []string{program}
	}
	if err := os.MkdirAll(filepath.Join(c.Workspace.VMRoot, "repo", ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "local.json")
	if err := Save(config, c); err != nil {
		t.Fatal(err)
	}
	input, transcript := workspacePTY(t, cli, "settings", "--config", config)
	waitWorkspaceCondition(t, "TUI menu", func() bool {
		text, _ := os.ReadFile(transcript)
		return strings.Contains(string(text), "Continue in tmux")
	})
	_, _ = io.WriteString(input, "\r")
	waitWorkspaceCondition(t, "TUI shell startup", func() bool { _, err := os.Stat(started); return err == nil })
	if source, _ := os.ReadFile(started); string(source) != "default|vm\n" {
		t.Fatalf("ordinary TUI shell was pinned: %q", source)
	}
	before := tmux("display-message", "-p", "-t", "stealthbox:shell", "#{pane_pid}")
	for index, runner := range []string{"mac", "vm"} {
		c.Workspace.Runner = runner
		c.Bridge.Enabled = true
		remote, err := remoteConfig(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := Save(filepath.Join(c.Workspace.RemoteDir, "config.json"), remote); err != nil {
			t.Fatal(err)
		}
		plan := filepath.Join(root, fmt.Sprintf("plan-%d", index))
		_, _ = io.WriteString(input, "codex; claude; stealthbox run --dry-run --path repo -- true > "+Quote(plan)+"\r")
		waitWorkspaceCondition(t, "runner selection from retained TUI shell", func() bool {
			text, _ := os.ReadFile(plan)
			return strings.Contains(string(text), runner)
		})
		log, _ := os.ReadFile(agentLog)
		want := "mac|captured\nmac|captured\n"
		if index == 1 {
			want += "vm|captured\nvm|captured\n"
		}
		if string(log) != want {
			t.Fatalf("new agents ignored current default: %q want %q", log, want)
		}
		if after := tmux("display-message", "-p", "-t", "stealthbox:shell", "#{pane_pid}"); after != before {
			t.Fatal("default switch restarted the shell", before, after)
		}
	}
}
