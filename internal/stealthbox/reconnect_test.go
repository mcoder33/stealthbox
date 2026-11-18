package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reconnectFixture(t *testing.T) Config {
	t.Helper()
	localDeploymentSSH(t)
	root, err := os.MkdirTemp("/tmp", "sb-reconnect-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("HOME", root)
	c, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	prepare := false
	c.Workspace.Prepare = &prepare
	c.Workspace.Host = "fixture-vm"
	c.Workspace.RemoteDir = filepath.Join(root, "state")
	c.Workspace.VMRoot = filepath.Join(root, "Projects")
	c.Workspace.LocalRoot = filepath.Join(root, "local")
	c.Workspace.RunnerRoot = filepath.Join(root, "runners")
	c.Workspace.ShellIntegration = true
	c.Workspace.Runner = "vm"
	c.Bridge.Enabled = true
	c.Bridge.Socket = filepath.Join(root, "mac.sock")
	c.Bridge.RemoteSocket = filepath.Join(root, "remote.sock")
	if err := os.MkdirAll(filepath.Join(c.Workspace.RemoteDir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.Workspace.VMRoot, 0700); err != nil {
		t.Fatal(err)
	}
	program := "#!/bin/sh\nif [ \"$1\" = handshake ]; then printf '%s\\n' " + Quote(Handshake()) + "; else printf '%s\\n' \"$@\" >> " + Quote(filepath.Join(root, "attached")) + "; fi\n"
	if err := os.WriteFile(filepath.Join(c.Workspace.RemoteDir, "bin/stealthbox"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReadyCreatesChangedRootWithoutRestartingPanes(t *testing.T) {
	for _, tilde := range []bool{false, true} {
		name := "absolute"
		if tilde {
			name = "home_relative"
		}
		t.Run(name, func(t *testing.T) {
			tmux := isolatedTmux(t)
			c := reconnectFixture(t)
			config := filepath.Join(c.Workspace.RemoteDir, "local.json")
			local := c
			local.Workspace.Host = ""
			if err := ResumeWorkspaceRemote(context.Background(), local, config, "", "", false, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			tmux("new-window", "-t", "stealthbox", "-n", "agent", "sleep 60")
			before := tmux("display-message", "-p", "-t", "stealthbox:agent", "#{window_id}|#{pane_pid}")
			want := filepath.Join(filepath.Dir(c.Workspace.VMRoot), "Work")
			c.Workspace.VMRoot = want
			if tilde {
				c.Workspace.VMRoot = "~/Work"
			}
			if err := EnsureReady(context.Background(), &c, config, "", io.Discard); err != nil {
				t.Fatal(err)
			}
			if c.Workspace.VMRoot != want {
				t.Fatalf("root was not normalized: %q", c.Workspace.VMRoot)
			}
			if info, err := os.Stat(want); err != nil || !info.IsDir() {
				t.Fatal("new root was not created", err)
			}
			local = c
			local.Workspace.Host = ""
			if err := ResumeWorkspaceRemote(context.Background(), local, config, "", "", false, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			if after := tmux("display-message", "-p", "-t", "stealthbox:agent", "#{window_id}|#{pane_pid}"); after != before {
				t.Fatal("retained agent was restarted", before, after)
			}
		})
	}
}

func TestConnectHonorsSSHHostOptions(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client unavailable")
	}
	c := reconnectFixture(t)
	c.Bridge.Enabled = false
	t.Setenv("TMUX", "")
	dir := canonicalTemp(t)
	sshConfig := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(sshConfig, []byte(`Host fixture-vm
	HostName 192.0.2.10
	ProxyJump fixture-jump
	ServerAliveInterval 20
	ServerAliveCountMax 4
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEALTHBOX_SSH_CONFIG", sshConfig)
	shim := filepath.Join(localDeploymentSSH(t), "ssh")
	script := "#!/bin/sh\n" + `for arg do
	case "$arg" in -t) exec ` + Quote(ssh) + ` -G "$@";; esac
done
while [ "$#" -gt 1 ]; do shift; done
exec /bin/sh -c "$1"
`
	if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output, stderr bytes.Buffer
	config := filepath.Join(dir, "local.json")
	if err := Connect(ctx, &c, config, ConnectOptions{}, &output, &stderr); err != nil {
		t.Fatalf("connect: %v\n%s", err, &stderr)
	}
	for _, option := range []string{"proxyjump fixture-jump", "serveraliveinterval 20", "serveralivecountmax 4", "exitonforwardfailure yes"} {
		if !strings.Contains("\n"+output.String()+"\n", "\n"+option+"\n") {
			t.Errorf("effective SSH configuration is missing %q", option)
		}
	}
}

func TestExplicitVMConnectWarnsOnBridgeFailure(t *testing.T) {
	c := reconnectFixture(t)
	t.Setenv("TMUX", "")
	config := filepath.Join(c.Workspace.RemoteDir, "local.json")
	if err := os.WriteFile(c.Bridge.Socket, []byte("unrelated-owner"), 0600); err != nil {
		t.Fatal(err)
	}
	var warning bytes.Buffer
	if err := Connect(context.Background(), &c, config, ConnectOptions{Runner: "vm"}, io.Discard, &warning); err != nil {
		t.Fatal("healthy explicitly selected VM was blocked", err)
	}
	if !strings.Contains(warning.String(), "Warning") || !strings.Contains(warning.String(), "bridge") {
		t.Fatal("missing bridge warning", warning.String())
	}
	if data, _ := os.ReadFile(c.Bridge.Socket); string(data) != "unrelated-owner" {
		t.Fatal("existing socket owner was modified")
	}
	if err := Connect(context.Background(), &c, config, ConnectOptions{Runner: "local"}, io.Discard, io.Discard); err == nil {
		t.Fatal("local connect silently fell back to VM")
	}
	if err := Connect(context.Background(), &c, config, ConnectOptions{}, io.Discard, io.Discard); err == nil {
		t.Fatal("nonexplicit VM silently ignored bridge failure")
	}
}

func TestExplicitVMConnectSurvivesRemoteBridgeHealthFailure(t *testing.T) {
	c := reconnectFixture(t)
	t.Setenv("TMUX", "")
	config := filepath.Join(c.Workspace.RemoteDir, "local.json")
	sshDir := localDeploymentSSH(t)
	program := "#!/bin/sh\nwhile [ \"$#\" -gt 1 ]; do shift; done\ncase \"$1\" in *bridge-health*) exit 1;; esac\nexec /bin/sh -c \"$1\"\n"
	if err := os.WriteFile(filepath.Join(sshDir, "ssh"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", c.Bridge.Socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(BridgeState{OK: true, ConfigHash: configHash(c)})
	})}
	go server.Serve(listener)
	defer server.Close()
	var warning bytes.Buffer
	started := time.Now()
	if err := Connect(context.Background(), &c, config, ConnectOptions{Runner: "vm"}, io.Discard, &warning); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 8*time.Second || !strings.Contains(warning.String(), "Warning") {
		t.Fatal("optional health failure blocked VM", warning.String())
	}
}

func TestReconnectOverrideKeepsOrdinaryShellDefaultsAndPinnedWindow(t *testing.T) {
	tmux := isolatedTmux(t)
	c := reconnectFixture(t)
	c.Workspace.Host = ""
	config := filepath.Join(c.Workspace.RemoteDir, "config.json")
	if err := Save(config, c); err != nil {
		t.Fatal(err)
	}
	if err := ResumeWorkspaceRemote(context.Background(), c, config, "", "", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	shellPID := tmux("display-message", "-p", "-t", "stealthbox:shell", "#{pane_pid}")
	log := filepath.Join(c.Workspace.RemoteDir, "pinned")
	var name strings.Builder
	if err := WorkspaceRemote(context.Background(), c, config, "", "shell", "mac", "pinned", []string{"-c", "printf '%s\\n' \"$STEALTHBOX_RUNNER|$STEALTHBOX_RUNNER_SOURCE\" > " + Quote(log) + "; exec sleep 60"}, false, &name, io.Discard); err != nil {
		t.Fatal(err)
	}
	waitForFixtureFile(t, log)
	pinnedWindow := strings.TrimSpace(name.String())
	pinnedPID := tmux("display-message", "-p", "-t", "stealthbox:"+pinnedWindow, "#{pane_pid}")
	for _, setting := range []string{"mac", "vm"} {
		c.Workspace.Runner = setting
		if err := Save(config, c); err != nil {
			t.Fatal(err)
		}
		override := "mac"
		if setting == "mac" {
			override = "vm"
		}
		if err := ResumeWorkspaceRemote(context.Background(), c, config, "", override, false, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		if got := tmux("show-environment", "-t", "stealthbox", "STEALTHBOX_RUNNER"); got != "STEALTHBOX_RUNNER="+setting {
			t.Fatal("reconnect override changed ordinary defaults", got)
		}
		if tmux("show-environment", "-t", "stealthbox", "STEALTHBOX_RUNNER_SOURCE") != "STEALTHBOX_RUNNER_SOURCE=default" {
			t.Fatal("ordinary shell default was pinned")
		}
		if tmux("display-message", "-p", "-t", "stealthbox:shell", "#{pane_pid}") != shellPID || tmux("display-message", "-p", "-t", "stealthbox:"+pinnedWindow, "#{pane_pid}") != pinnedPID {
			t.Fatal("reconnect restarted existing shell or pinned process")
		}
	}
	if data, _ := os.ReadFile(log); strings.TrimSpace(string(data)) != "mac|captured" {
		t.Fatal("pinned window lost runner", string(data))
	}
}

func TestBridgeTunnelDoesNotRemoveOccupiedRemoteSocket(t *testing.T) {
	c := reconnectFixture(t)
	sshDir := localDeploymentSSH(t)
	readyPath := filepath.Join(c.Workspace.RemoteDir, "tunnel-ready")
	program := "#!/bin/sh\ncase \" $* \" in *' -N '*) printf ready > " + Quote(readyPath) + "; exec sleep 60;; esac\nwhile [ \"$#\" -gt 1 ]; do shift; done\ncase \"$1\" in *bridge-health*) exit 1;; esac\nexec /bin/sh -c \"$1\"\n"
	if err := os.WriteFile(filepath.Join(sshDir, "ssh"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", c.Bridge.RemoteSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- BridgeService(ctx, c, io.Discard) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("tunnel fixture did not stop")
		}
	}()
	waitForFixtureFile(t, readyPath)
	if info, err := os.Lstat(c.Bridge.RemoteSocket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("bridge deleted another socket owner", err)
	}
}
