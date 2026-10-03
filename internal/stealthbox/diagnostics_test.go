//go:build !windows

package stealthbox

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func doctorSSHFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "ssh-calls")
	program := "#!/bin/sh\nwhile [ \"$#\" -gt 2 ]; do shift; done\nprintf '%s|%s\\n' \"$1\" \"$2\" >> " + Quote(log) + `
case "$1" in
 dead|dead-alias) printf 'fixture transport failure\n' >&2; exit 255;;
 blocked) sleep 5 & wait; exit 0;;
esac
case "$2" in *'command -v tmux'*) printf 'tmux missing\n' >&2; exit 127;; esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestDoctorProbesExactHostsOnceAndSkipsDependents(t *testing.T) {
	log := doctorSSHFixture(t)
	c, _ := DefaultConfig()
	c.Bridge.Enabled = false
	c.Workspace.Host = "dead"
	c.Workspace.VMRoot = "/fixture/Projects"
	c.Workspace.RemoteDir = "/fixture/state"
	c.Projects = map[string]Project{
		"same-host":  {Source: Endpoint{Host: "dead", Path: "/fixture/a"}, Agent: "shell", Runners: map[string]Endpoint{"other": {Host: "dead-alias", Path: "/fixture/runner"}}},
		"other-host": {Source: Endpoint{Host: "healthy", Path: "/fixture/b"}, Agent: "shell"},
		"local":      {Source: Endpoint{Path: t.TempDir()}, Agent: "shell"},
	}
	var output bytes.Buffer
	if err := Doctor(context.Background(), c, &output); err == nil {
		t.Fatal("unreachable SSH destination was accepted")
	}
	calls, _ := os.ReadFile(log)
	for _, host := range []string{"dead", "dead-alias"} {
		if count := strings.Count(string(calls), host+"|"); count != 1 {
			t.Fatalf("%s got %d remote calls instead of one initial probe: %s", host, count, calls)
		}
	}
	if !strings.Contains(string(calls), "healthy|true") || !strings.Contains(output.String(), " OK  source other-host") || !strings.Contains(output.String(), " OK  source local") {
		t.Fatal("independent checks did not continue", output.String(), string(calls))
	}
	for _, name := range []string{"workspace Projects root", "VM rsync + Git", "workspace agent codex", "remote tmux", "remote Stealth Box", "source same-host"} {
		if !strings.Contains(output.String(), "SKIP "+name) {
			t.Fatal("missing dependent skip", name, output.String())
		}
	}
}

func TestDoctorReachableMissingTmuxIsSeparateFailure(t *testing.T) {
	log := doctorSSHFixture(t)
	c, _ := DefaultConfig()
	c.Bridge.Enabled = false
	c.Workspace.Host = "healthy"
	c.Workspace.VMRoot = "/fixture/Projects"
	var output bytes.Buffer
	if err := Doctor(context.Background(), c, &output); err == nil {
		t.Fatal("missing remote tmux was accepted")
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "healthy|true\n") != 1 || !strings.Contains(output.String(), " OK  SSH healthy") || !strings.Contains(output.String(), "FAIL remote tmux") || strings.Contains(output.String(), "SKIP remote tmux") {
		t.Fatal("reachability and tool availability were conflated", output.String(), string(calls))
	}
}

func TestDoctorSSHProbeCancellationIsBounded(t *testing.T) {
	log := doctorSSHFixture(t)
	c, _ := DefaultConfig()
	c.Bridge.Enabled = false
	c.Workspace.Host = "blocked"
	c.Workspace.VMRoot = "/fixture/Projects"
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	var output bytes.Buffer
	_ = Doctor(ctx, c, &output)
	if time.Since(started) > 3*time.Second {
		t.Fatal("probe waited for inherited descendant pipes", time.Since(started))
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "blocked|") != 1 || !strings.Contains(output.String(), "SKIP workspace Projects root") {
		t.Fatal("failed probe was retried", string(calls), output.String())
	}
}

func TestRemoteDiscoveryCancellationIsBounded(t *testing.T) {
	log := doctorSSHFixture(t)
	c, _ := DefaultConfig()
	c.Workspace.Host = "blocked"
	c.Workspace.VMRoot = "/fixture/Projects"
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	if paths, err := WorkspaceProjects(ctx, c); err == nil || len(paths) != 0 {
		t.Fatal("canceled discovery succeeded", paths, err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("discovery waited for descendant pipes", time.Since(started))
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "blocked|") != 1 {
		t.Fatal("discovery was retried", string(calls))
	}
}

func TestBridgeStatusDistinguishesReadOnlyFixtureStates(t *testing.T) {
	for _, state := range []string{"missing", "refused", "permission", "unauthorized", "healthy", "unknown"} {
		t.Run(state, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "sb-status-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			c, _ := DefaultConfig()
			c.Workspace.Host = "must-not-be-probed"
			c.Bridge.Socket = filepath.Join(root, "bridge.sock")
			want := ""
			var underlying error
			switch state {
			case "missing":
				want, underlying = "not running; use bridge-start", syscall.ENOENT
			case "refused":
				listener, err := net.Listen("unix", c.Bridge.Socket)
				if err != nil {
					t.Fatal(err)
				}
				listener.(*net.UnixListener).SetUnlinkOnClose(false)
				listener.Close()
				want, underlying = "refused connection", syscall.ECONNREFUSED
			case "permission":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses fixture directory permissions")
				}
				if err := os.Chmod(root, 0000); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(root, 0700)
				want, underlying = "permission denied", syscall.EACCES
			default:
				listener, err := net.Listen("unix", c.Bridge.Socket)
				if err != nil {
					t.Fatal(err)
				}
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" || r.URL.Path != "/health" {
						t.Errorf("status mutated fixture: %s %s", r.Method, r.URL.Path)
					}
					switch state {
					case "unauthorized":
						w.WriteHeader(401)
					case "unknown":
						w.WriteHeader(503)
						_, _ = w.Write([]byte("fixture upstream detail"))
					default:
						_, _ = w.Write([]byte(`{"ok":true,"allow_exec":true,"config_hash":"fixture"}`))
					}
				})}
				go server.Serve(listener)
				defer server.Close()
				if state == "unauthorized" {
					want = "HTTP 401"
				} else if state == "unknown" {
					want = "HTTP 503: fixture upstream detail"
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := BridgeStatus(ctx, c)
			if state == "healthy" {
				if err != nil || !result.OK || !result.AllowExec {
					t.Fatal("healthy local daemon unavailable", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), want) || (underlying != nil && !errors.Is(err, underlying)) {
				t.Fatalf("state %s not identified or original error lost: %v", state, err)
			}
			if state == "refused" {
				if _, err := os.Lstat(c.Bridge.Socket); err != nil {
					t.Fatal("status removed stale socket", err)
				}
			}
		})
	}
}
