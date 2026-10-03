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
	"sync/atomic"
	"testing"
	"time"
)

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
	runDone := make(chan error, 1)
	go func() {
		runDone <- RunBridge(context.Background(), c, "test", []string{"sh", "-c", "while [ ! -f " + Quote(finish) + " ]; do printf x >> " + Quote(heartbeat) + "; sleep 0.02; done"}, false, "", io.Discard, io.Discard)
	}()
	waitForFixtureFile(t, heartbeat)
	changed := c
	changed.Bridge.TimeoutSeconds++
	if err := StartBridgeService(ctx, changed, filepath.Join(root, "config.json"), io.Discard); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatal("busy bridge was automatically restarted")
	}
	before, _ := os.ReadFile(heartbeat)
	time.Sleep(150 * time.Millisecond)
	after, _ := os.ReadFile(heartbeat)
	if len(after) <= len(before) {
		t.Fatal("automatic config refresh killed ongoing local run")
	}
	if state, err := BridgeStatus(context.Background(), c); err != nil || state.ConfigHash != configHash(c) {
		t.Fatal("busy bridge configuration changed", err)
	}
	if err := os.WriteFile(finish, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
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
