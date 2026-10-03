package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
			neighbor := exec.Command("sh", "-c", "while :; do printf x >> "+Quote(filepath.Join(root, "neighbor"))+"; sleep 0.02; done")
			neighbor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := neighbor.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = syscall.Kill(-neighbor.Process.Pid, syscall.SIGKILL); _ = neighbor.Wait() }()
			// Individual PID cleanup is safe on both the old implementation (shared
			// test process group) and the new isolated command process group.
			defer func() {
				for _, path := range []string{childPIDPath, parentPIDPath} {
					if pid := readProcessFixturePID(path); pid > 0 {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			}()
			started := time.Now()
			done := make(chan error, 1)
			go func() {
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
				data, _ := os.ReadFile(heartbeat)
				if len(data) >= 3 && readProcessFixturePID(childPIDPath) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if data, _ := os.ReadFile(heartbeat); len(data) < 3 {
				t.Fatal("descendant did not start before timeout")
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
			stopped, _ := os.ReadFile(heartbeat)
			neighborBefore, _ := os.ReadFile(filepath.Join(root, "neighbor"))
			time.Sleep(150 * time.Millisecond)
			after, _ := os.ReadFile(heartbeat)
			neighborAfter, _ := os.ReadFile(filepath.Join(root, "neighbor"))
			if !bytes.Equal(stopped, after) {
				t.Fatal("descendant heartbeat survived timeout")
			}
			if len(neighborAfter) <= len(neighborBefore) {
				t.Fatal("unrelated neighboring process was killed")
			}
		})
	}
}
