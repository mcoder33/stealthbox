package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

type heartbeatFixtureProcess struct {
	pid, pgid int
	heartbeat string
	done      chan struct{}
	waitErr   error
}

func (process *heartbeatFixtureProcess) exitState() (bool, error) {
	select {
	case <-process.done:
		return true, process.waitErr
	default:
		return false, nil
	}
}

func (process *heartbeatFixtureProcess) diagnostic() string {
	exited, err := process.exitState()
	return fmt.Sprintf("neighbor pid=%d pgid=%d exited=%t wait=%v", process.pid, process.pgid, exited, err)
}

func startHeartbeatFixtureProcess(t *testing.T, heartbeat string) *heartbeatFixtureProcess {
	t.Helper()
	cmd := exec.Command("sh", "-c", "while :; do printf x >> "+Quote(heartbeat)+"; sleep 0.02; done")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid, groupErr := syscall.Getpgid(cmd.Process.Pid)
	process := &heartbeatFixtureProcess{pid: cmd.Process.Pid, pgid: pgid, heartbeat: heartbeat, done: make(chan struct{})}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		if exited, _ := process.exitState(); !exited {
			if process.pgid == process.pid && process.pgid != syscall.Getpgrp() {
				_ = syscall.Kill(-process.pgid, syscall.SIGKILL)
			} else {
				_ = cmd.Process.Kill()
			}
		}
		select {
		case <-process.done:
		case <-time.After(3 * time.Second):
			t.Errorf("owned fixture did not exit after cleanup: %s", process.diagnostic())
		}
	})
	if groupErr != nil || pgid != process.pid || pgid == syscall.Getpgrp() {
		t.Fatalf("fixture lacks a private process group: %s error=%v", process.diagnostic(), groupErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	initial := -1
	for time.Now().Before(deadline) {
		if exited, _ := process.exitState(); exited {
			t.Fatal("fixture exited before readiness:", process.diagnostic())
		}
		data, err := os.ReadFile(heartbeat)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("fixture readiness read: %v; %s", err, process.diagnostic())
		}
		if err == nil {
			if initial >= 0 && len(data) > initial {
				return process
			}
			initial = len(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture heartbeat did not become ready: length=%d; %s", initial, process.diagnostic())
	return nil
}

func observeCancellationHeartbeats(heartbeat string, neighbor *heartbeatFixtureProcess) error {
	stopped, err := os.ReadFile(heartbeat)
	if err != nil {
		return fmt.Errorf("descendant heartbeat baseline read: %w; %s", err, neighbor.diagnostic())
	}
	before, err := os.ReadFile(neighbor.heartbeat)
	if err != nil {
		return fmt.Errorf("neighbor heartbeat baseline read: %w; %s", err, neighbor.diagnostic())
	}
	started := time.Now()
	for {
		after, err := os.ReadFile(heartbeat)
		if err != nil {
			return fmt.Errorf("descendant heartbeat read: %w; elapsed=%s; %s", err, time.Since(started), neighbor.diagnostic())
		}
		progress, err := os.ReadFile(neighbor.heartbeat)
		if err != nil {
			return fmt.Errorf("neighbor heartbeat read: %w; elapsed=%s; %s", err, time.Since(started), neighbor.diagnostic())
		}
		exited, waitErr := neighbor.exitState()
		diagnostic := fmt.Sprintf("elapsed=%s descendant=%d->%d neighbor=%d->%d; neighbor pid=%d pgid=%d exited=%t wait=%v", time.Since(started), len(stopped), len(after), len(before), len(progress), neighbor.pid, neighbor.pgid, exited, waitErr)
		if !bytes.Equal(stopped, after) {
			return fmt.Errorf("descendant heartbeat survived timeout: %s", diagnostic)
		}
		if exited {
			return fmt.Errorf("unrelated neighbor exited: %s", diagnostic)
		}
		if time.Since(started) >= 150*time.Millisecond && len(progress) > len(before) {
			return nil
		}
		if time.Since(started) >= 2*time.Second {
			return fmt.Errorf("neighbor heartbeat did not progress within observation bound: %s", diagnostic)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNoninteractiveVMTimeoutStopsDescendants(t *testing.T) {
	for _, test := range []struct {
		name      string
		viaMCP    bool
		earlyExit string
		waitDelay bool
	}{
		{name: "run"},
		{name: "mcp", viaMCP: true},
		{name: "run_leader_exits", earlyExit: "0"},
		{name: "mcp_leader_exits", viaMCP: true, earlyExit: "0"},
		{name: "run_leader_exits_nonzero", earlyExit: "7"},
		{name: "wait_delay_without_deadline", earlyExit: "0", waitDelay: true},
		{name: "wait_delay_nonzero_without_deadline", earlyExit: "7", waitDelay: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := bridgeConfig(t)
			c.Bridge.TimeoutSeconds = 1
			t.Setenv("STEALTHBOX_PROJECT", "test")
			t.Setenv("STEALTHBOX_SCOPE", "project")
			root := c.Projects["test"].Runners["vm"].Path
			heartbeat := filepath.Join(root, "heartbeat")
			childPIDPath := filepath.Join(root, "child.pid")
			parentPIDPath := filepath.Join(root, "parent.pid")
			childScript := "echo $$ > " + Quote(childPIDPath) + "; while :; do printf x >> " + Quote(heartbeat) + "; sleep 0.02; done"
			script := "echo $$ > " + Quote(parentPIDPath) + "; sh -c " + Quote(childScript) + " & wait"
			if test.earlyExit != "" {
				script = "echo $$ > " + Quote(parentPIDPath) + "; sh -c " + Quote(childScript) + " & exit " + test.earlyExit
			}
			neighbor := startHeartbeatFixtureProcess(t, filepath.Join(root, "neighbor"))
			commandGroup := 0
			commandChild := 0
			finished := make(chan struct{})
			defer func() {
				currentGroup, err := syscall.Getpgid(commandChild)
				if commandChild > 0 && err == nil && currentGroup == commandGroup && commandGroup > 1 && commandGroup != neighbor.pgid && commandGroup != syscall.Getpgrp() {
					_ = syscall.Kill(-commandGroup, syscall.SIGKILL)
				}
				select {
				case <-finished:
				case <-time.After(4 * time.Second):
					t.Error("owned command did not finish after fixture cleanup")
				}
			}()
			started := time.Now()
			done := make(chan error, 1)
			go func() {
				defer close(finished)
				if test.viaMCP {
					request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "vm_exec", "arguments": map[string]any{"argv": []string{"sh", "-c", script}}}})
					var response bytes.Buffer
					err := ServeMCP(context.Background(), c, bytes.NewReader(request), &response)
					if err == nil && !strings.Contains(response.String(), `"isError":true`) {
						err = errors.New("MCP did not report a timeout error")
					}
					done <- err
					return
				}
				if test.waitDelay {
					done <- ExecuteNoninteractiveContext(context.Background(), Command{"sh", []string{"-c", script}}, nil, io.Discard, io.Discard)
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				done <- Run(ctx, c, "test", "vm", []string{"sh", "-c", script}, false, "", io.Discard, io.Discard)
			}()
			deadline := time.Now().Add(900 * time.Millisecond)
			for time.Now().Before(deadline) {
				data, err := os.ReadFile(heartbeat)
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("descendant readiness read: %v; %s", err, neighbor.diagnostic())
				}
				if len(data) >= 3 && readProcessFixturePID(childPIDPath) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if data, err := os.ReadFile(heartbeat); err != nil || len(data) < 3 {
				t.Fatalf("descendant did not start before timeout: length=%d read=%v; %s", len(data), err, neighbor.diagnostic())
			}
			childPID := readProcessFixturePID(childPIDPath)
			if childPID <= 0 {
				t.Fatal("descendant PID fixture could not be read:", childPIDPath)
			}
			commandChild = childPID
			if group, err := syscall.Getpgid(childPID); err == nil {
				if group == neighbor.pgid || group == syscall.Getpgrp() {
					t.Fatalf("command and neighbor lack independent groups: child_pid=%d command_pgid=%d; %s", childPID, group, neighbor.diagnostic())
				}
				commandGroup = group
			} else if !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("command group inspection: child_pid=%d error=%v; %s", childPID, err, neighbor.diagnostic())
			}
			select {
			case err := <-done:
				if !test.viaMCP && err == nil || test.viaMCP && err != nil {
					t.Fatalf("unexpected timeout result: %v", err)
				}
				if test.waitDelay && test.earlyExit == "0" && !errors.Is(err, exec.ErrWaitDelay) {
					t.Fatalf("expected WaitDelay result, got %v", err)
				}
				if test.earlyExit == "7" {
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != 7 {
						t.Fatalf("leader's exit code was lost: %v", err)
					}
				}
			case <-time.After(time.Until(started.Add(4 * time.Second))):
				t.Fatal("timeout did not return within four seconds")
			}
			if elapsed := time.Since(started); elapsed > 4*time.Second {
				t.Fatal("timeout returned late:", elapsed)
			}
			if err := observeCancellationHeartbeats(heartbeat, neighbor); err != nil {
				t.Fatalf("%v; command_child_pid=%d command_pgid=%d", err, childPID, commandGroup)
			}
		})
	}
}

func TestCancellationHeartbeatOracle(t *testing.T) {
	for _, scenario := range []string{"paused_neighbor_resumes", "killed_neighbor", "live_descendant"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			heartbeat := filepath.Join(root, "descendant")
			if err := os.WriteFile(heartbeat, []byte("stopped"), 0600); err != nil {
				t.Fatal(err)
			}
			neighbor := startHeartbeatFixtureProcess(t, filepath.Join(root, "neighbor"))
			switch scenario {
			case "paused_neighbor_resumes":
				if err := syscall.Kill(-neighbor.pgid, syscall.SIGSTOP); err != nil {
					t.Fatal(err)
				}
				time.Sleep(50 * time.Millisecond)
				before, err := os.ReadFile(neighbor.heartbeat)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- observeCancellationHeartbeats(heartbeat, neighbor) }()
				time.Sleep(200 * time.Millisecond)
				after, err := os.ReadFile(neighbor.heartbeat)
				if exited, _ := neighbor.exitState(); err != nil || exited || !bytes.Equal(before, after) {
					t.Fatalf("controlled pause was not stable/alive: %d->%d read=%v; %s", len(before), len(after), err, neighbor.diagnostic())
				}
				if err := syscall.Kill(-neighbor.pgid, syscall.SIGCONT); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal("paused neighbor was mistaken for an exited process:", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("heartbeat oracle did not return within its bound")
				}
			case "killed_neighbor":
				if err := syscall.Kill(-neighbor.pgid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				select {
				case <-neighbor.done:
				case <-time.After(2 * time.Second):
					t.Fatal("killed neighbor was not reaped")
				}
				if err := observeCancellationHeartbeats(heartbeat, neighbor); err == nil || !strings.Contains(err.Error(), "neighbor exited") {
					t.Fatal("oracle accepted a killed neighbor:", err)
				}
			case "live_descendant":
				startHeartbeatFixtureProcess(t, heartbeat)
				if err := observeCancellationHeartbeats(heartbeat, neighbor); err == nil || !strings.Contains(err.Error(), "descendant heartbeat survived") {
					t.Fatal("oracle accepted a live descendant:", err)
				}
			}
		})
	}
}
