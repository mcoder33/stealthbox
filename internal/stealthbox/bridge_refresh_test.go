package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func observeBridgeHeartbeat(heartbeat string, completion func() (bool, error), pid, pgid int) error {
	before, err := os.ReadFile(heartbeat)
	if err != nil {
		return fmt.Errorf("run heartbeat baseline read: %w; pid=%d pgid=%d", err, pid, pgid)
	}
	started := time.Now()
	for {
		after, err := os.ReadFile(heartbeat)
		if err != nil {
			return fmt.Errorf("run heartbeat read: %w; elapsed=%s pid=%d pgid=%d", err, time.Since(started), pid, pgid)
		}
		completed, runErr := completion()
		diagnostic := fmt.Sprintf("elapsed=%s heartbeat=%d->%d pid=%d pgid=%d completed=%t result=%v", time.Since(started), len(before), len(after), pid, pgid, completed, runErr)
		if completed {
			return fmt.Errorf("run completed unexpectedly: %s", diagnostic)
		}
		if time.Since(started) >= 150*time.Millisecond && len(after) > len(before) {
			return nil
		}
		if time.Since(started) >= 2*time.Second {
			return fmt.Errorf("pending run made no heartbeat progress within observation bound: %s", diagnostic)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type bridgeFixtureCompletion struct {
	done chan struct{}
	err  error
}

func (run *bridgeFixtureCompletion) state() (bool, error) {
	select {
	case <-run.done:
		return true, run.err
	default:
		return false, nil
	}
}

func TestBridgeHeartbeatOracle(t *testing.T) {
	for _, scenario := range []string{"paused_run_resumes", "killed_run", "stalled_live_run"} {
		t.Run(scenario, func(t *testing.T) {
			process := startHeartbeatFixtureProcess(t, filepath.Join(t.TempDir(), "heartbeat"))
			if scenario == "killed_run" {
				if err := syscall.Kill(-process.pgid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				select {
				case <-process.done:
				case <-time.After(2 * time.Second):
					t.Fatal("killed fixture was not reaped")
				}
				if err := observeBridgeHeartbeat(process.heartbeat, process.exitState, process.pid, process.pgid); err == nil || !strings.Contains(err.Error(), "run completed unexpectedly") {
					t.Fatal("oracle did not detect a completed run:", err)
				}
				return
			}
			if err := syscall.Kill(-process.pgid, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			before, err := os.ReadFile(process.heartbeat)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "stalled_live_run" {
				started := time.Now()
				err := observeBridgeHeartbeat(process.heartbeat, process.exitState, process.pid, process.pgid)
				if err == nil || !strings.Contains(err.Error(), "pending run made no heartbeat progress") {
					t.Fatal("oracle accepted or misclassified a stalled live run:", err)
				}
				if exited, _ := process.exitState(); exited || time.Since(started) > 3*time.Second {
					t.Fatalf("stall check was not bounded with a live fixture: %s", process.diagnostic())
				}
				return
			}
			done := make(chan error, 1)
			go func() {
				done <- observeBridgeHeartbeat(process.heartbeat, process.exitState, process.pid, process.pgid)
			}()
			time.Sleep(200 * time.Millisecond)
			after, err := os.ReadFile(process.heartbeat)
			if exited, _ := process.exitState(); err != nil || exited || !bytes.Equal(before, after) {
				t.Fatalf("controlled pause was not stable/alive: %d->%d read=%v; %s", len(before), len(after), err, process.diagnostic())
			}
			if err := syscall.Kill(-process.pgid, syscall.SIGCONT); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("oracle mistook a paused live run for a killed run:", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("heartbeat oracle exceeded its observation bound")
			}
		})
	}
}

func TestBridgeRefreshPreservesBusyRunThenAppliesConfig(t *testing.T) {
	localDeploymentSSH(t)
	root, err := os.MkdirTemp("/tmp", "sb-refresh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	c := bridgeConfig(t)
	c.Workspace.Host = "fixture-vm"
	c.Workspace.RemoteDir = filepath.Join(root, "state")
	c.Bridge.Socket = filepath.Join(root, "mac.sock")
	c.Bridge.RemoteSocket = filepath.Join(root, "remote.sock")
	c.Bridge.AllowExec = true
	if err := os.MkdirAll(filepath.Join(c.Workspace.RemoteDir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	remoteBin := filepath.Join(c.Workspace.RemoteDir, "bin/stealthbox")
	if err := os.WriteFile(remoteBin, []byte("#!/bin/sh\nif [ \"$1\" = handshake ]; then printf '%s\\n' "+Quote(Handshake())+"; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready, stopped := make(chan struct{}), make(chan error, 1)
	go func() { stopped <- serveManagedBridge(ctx, c, ready, cancel) }()
	<-ready
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(6 * time.Second):
			t.Error("fixture bridge did not stop")
		}
	})
	heartbeat := filepath.Join(root, "heartbeat")
	finish := filepath.Join(root, "finish")
	pidPath := filepath.Join(root, "command.pid")
	runCtx, cancelRun := context.WithCancel(context.Background())
	run := &bridgeFixtureCompletion{done: make(chan struct{})}
	go func() {
		run.err = RunBridge(runCtx, c, "test", []string{"sh", "-c", "echo $$ > " + Quote(pidPath) + "; while [ ! -f " + Quote(finish) + " ]; do printf x >> " + Quote(heartbeat) + "; sleep 0.02; done"}, false, "", io.Discard, io.Discard)
		close(run.done)
	}()
	pid, pgid := 0, 0
	t.Cleanup(func() {
		if completed, _ := run.state(); !completed {
			if group, err := syscall.Getpgid(pid); pid > 0 && err == nil && group == pgid && pgid == pid && pgid != syscall.Getpgrp() {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
		cancelRun()
		select {
		case <-run.done:
		case <-time.After(3 * time.Second):
			t.Error("owned bridge request did not finish after cleanup")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if completed, err := run.state(); completed {
			t.Fatal("bridge run completed before readiness:", err)
		}
		data, err := os.ReadFile(heartbeat)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal("run heartbeat readiness read:", err)
		}
		if err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge run heartbeat did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid = readProcessFixturePID(pidPath)
	pgid, err = syscall.Getpgid(pid)
	if pid <= 0 || err != nil || pgid != pid || pgid == syscall.Getpgrp() {
		t.Fatalf("bridge fixture lacks a private process group: pid=%d pgid=%d error=%v", pid, pgid, err)
	}
	if err := observeBridgeHeartbeat(heartbeat, run.state, pid, pgid); err != nil {
		t.Fatal("bridge run did not establish progress before refresh:", err)
	}
	changed := c
	changed.Bridge.TimeoutSeconds++
	if err := StartBridgeService(ctx, changed, filepath.Join(root, "config.json"), io.Discard); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatal("busy bridge was automatically restarted")
	}
	if err := observeBridgeHeartbeat(heartbeat, run.state, pid, pgid); err != nil {
		t.Fatal("busy bridge run after automatic refresh:", err)
	}
	if state, err := BridgeStatus(context.Background(), c); err != nil || state.ConfigHash != configHash(c) {
		t.Fatal("busy bridge configuration changed", err)
	}
	if err := os.WriteFile(finish, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.done:
		if run.err != nil {
			t.Fatal(run.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local run did not finish")
	}
	config := filepath.Join(root, "config.json")
	if err := Save(config, changed); err != nil {
		t.Fatal(err)
	}
	// The real CLI starts its own background executable; a test executable is
	// not a bridge-service binary. All sockets, config and logs stay in root.
	cli := filepath.Join(root, "stealthbox")
	build := exec.Command("go", "build", "-o", cli, "./cmd/stealthbox")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	refresh := exec.Command(cli, "bridge-start", "--config", config)
	if out, err := refresh.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = StopBridgeService(cleanup, changed)
	})
	if state, err := BridgeStatus(context.Background(), changed); err != nil || state.ConfigHash != configHash(changed) {
		t.Fatal("idle bridge did not accept new config", err)
	}
}

func TestIdleBridgeShutdownRefusesNewWorkAtomically(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "sb-drain-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	c := bridgeConfig(t)
	c.Bridge.Socket = filepath.Join(root, "socket")
	c.Bridge.AllowExec = true
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}), make(chan error, 1)
	requested := make(chan struct{})
	// Hold shutdown after the atomic idle/draining transition, making the race
	// window deterministic while allowing an HTTP request to reach the server.
	go func() { done <- serveManagedBridge(ctx, c, ready, func() { close(requested) }) }()
	<-ready
	defer func() { cancel(); <-done }()
	req, _ := http.NewRequest("POST", "http://unix/shutdown-idle", nil)
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	res, err := bridgeHTTP(c.Bridge.Socket).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("idle shutdown refused", res.StatusCode)
	}
	<-requested
	marker := filepath.Join(root, "started")
	if err := RunBridge(context.Background(), c, "test", []string{"sh", "-c", "touch " + Quote(marker)}, false, "", io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatal("draining server accepted new work", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command ran after idle shutdown", err)
	}
}

func TestAutomaticBridgeRefreshNeverForceStopsLegacyOrUnauthorizedServer(t *testing.T) {
	for _, unauthorized := range []bool{false, true} {
		name := "legacy"
		if unauthorized {
			name = "changed_token"
		}
		t.Run(name, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "sb-legacy-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			c := bridgeConfig(t)
			c.Workspace.RemoteDir = root
			c.Bridge.Socket = filepath.Join(root, "socket")
			var forced atomic.Int32
			listener, err := net.Listen("unix", c.Bridge.Socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if unauthorized {
					w.WriteHeader(401)
					return
				}
				if r.URL.Path == "/health" {
					_ = json.NewEncoder(w).Encode(BridgeState{OK: true, ConfigHash: "legacy-config"})
					return
				}
				if r.URL.Path == "/shutdown" {
					forced.Add(1)
					w.WriteHeader(200)
					return
				}
				w.WriteHeader(404)
			})}
			go server.Serve(listener)
			defer server.Close()
			err = StartBridgeService(context.Background(), c, filepath.Join(root, "config.json"), io.Discard)
			if err == nil || !strings.Contains(err.Error(), "bridge-stop") {
				t.Fatal("missing actionable safe refresh refusal", err)
			}
			if forced.Load() != 0 {
				t.Fatal("auto-refresh attempted force shutdown")
			}
		})
	}
}

func waitForFixtureFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(data)) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fixture did not become ready", path)
}
