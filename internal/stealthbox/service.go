package stealthbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type BridgeState struct {
	OK         bool   `json:"ok"`
	AllowExec  bool   `json:"allow_exec"`
	ConfigHash string `json:"config_hash"`
}

func BridgeStatus(ctx context.Context, c Config) (BridgeState, error) {
	var s BridgeState
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://unix/health", nil)
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	res, err := bridgeHTTP(c.Bridge.Socket).Do(req)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return s, fmt.Errorf("local Mac bridge is not running; use bridge-start: %w", err)
		case errors.Is(err, syscall.ECONNREFUSED):
			return s, fmt.Errorf("local Mac bridge socket refused connection; the daemon may have stopped or the socket may be stale; inspect its owner before restarting: %w", err)
		case errors.Is(err, os.ErrPermission):
			return s, fmt.Errorf("local Mac bridge socket permission denied; check its owner and permissions: %w", err)
		default:
			return s, fmt.Errorf("local Mac bridge status could not be checked: %w", err)
		}
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		if res.StatusCode == http.StatusUnauthorized {
			return s, fmt.Errorf("local Mac bridge rejected credentials (HTTP 401); use the configuration matching the running daemon")
		}
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return s, fmt.Errorf("local Mac bridge status: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(detail)))
	}
	err = json.NewDecoder(res.Body).Decode(&s)
	if err != nil {
		return s, fmt.Errorf("local Mac bridge returned an invalid health response: %w", err)
	}
	if !s.OK {
		return s, fmt.Errorf("local Mac bridge reports unhealthy")
	}
	return s, err
}
func StopBridgeService(ctx context.Context, c Config) error {
	return stopBridgeService(ctx, c, false)
}

func stopBridgeService(ctx context.Context, c Config, idleOnly bool) error {
	endpoint := "/shutdown"
	if idleOnly {
		endpoint = "/shutdown-idle"
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://unix"+endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	res, err := bridgeHTTP(c.Bridge.Socket).Do(req)
	if err != nil {
		return fmt.Errorf("bridge is not running or inaccessible: %w", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		if idleOnly {
			if res.StatusCode == http.StatusConflict {
				return fmt.Errorf("Mac bridge is busy; wait for local runs to finish and retry, or explicitly use bridge-stop to cancel them")
			}
			return fmt.Errorf("cannot safely refresh bridge: HTTP %d; use bridge-stop with its original configuration or stop its owner, then retry", res.StatusCode)
		}
		return fmt.Errorf("cannot stop bridge: HTTP %d (foreground unmanaged bridges need Ctrl-C)", res.StatusCode)
	}
	for i := 0; i < 40; i++ {
		if _, e := os.Lstat(c.Bridge.Socket); os.IsNotExist(e) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("bridge did not stop")
}
func StartBridgeService(ctx context.Context, c Config, path string, log io.Writer) error {
	if !c.Bridge.Enabled {
		return fmt.Errorf("enable the Mac bridge in settings first")
	}
	if c.Workspace.RemoteDir == "" {
		return fmt.Errorf("run setup before starting the bridge")
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	s, e := BridgeStatus(probe, c)
	cancel()
	if e == nil {
		if s.ConfigHash == configHash(c) {
			return nil
		}
		if e = stopBridgeService(ctx, c, true); e != nil {
			return e
		}
	} else {
		if _, socketErr := os.Lstat(c.Bridge.Socket); socketErr == nil {
			return fmt.Errorf("existing bridge cannot be checked safely: %w; use bridge-stop with its original configuration or stop its owner, then retry; stale sockets require manual cleanup", e)
		} else if !os.IsNotExist(socketErr) {
			return fmt.Errorf("cannot inspect bridge socket: %w", socketErr)
		}
	}
	if err := os.MkdirAll(filepath.Dir(c.Bridge.Socket), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(c.Bridge.Socket+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "bridge-service", "--config", path)
	cmd.Stdin = nil
	cmd.Stdout = file
	cmd.Stderr = file
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	for i := 0; i < 100; i++ {
		select {
		case e := <-wait:
			return fmt.Errorf("bridge service exited: %v; see %s.log", e, c.Bridge.Socket)
		default:
		}
		probe, done := context.WithTimeout(ctx, time.Second)
		_, e := BridgeStatus(probe, c)
		done()
		if e == nil {
			fmt.Fprintf(log, "Mac bridge started in background. Log: %s.log\n", c.Bridge.Socket)
			return nil
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	return fmt.Errorf("bridge startup timed out; see %s.log", c.Bridge.Socket)
}
func BridgeService(parent context.Context, c Config, log io.Writer) error {
	if c.Workspace.Host == "" || c.Workspace.RemoteDir == "" || c.Bridge.RemoteSocket == "" {
		return fmt.Errorf("run setup first")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- serveManagedBridge(ctx, c, ready, cancel); cancel() }()
	select {
	case <-ready:
	case e := <-done:
		return e
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { cancel(); <-done }()
	remoteBin := filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox")
	remoteCfg := filepath.Join(c.Workspace.RemoteDir, "config.json")
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		probe, end := context.WithTimeout(ctx, 5*time.Second)
		_, err := Output(probe, sshCommand(c.Workspace.Host, shellArgs([]string{remoteBin, "bridge-health", "--config", remoteCfg}), false))
		end()
		if err == nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
				continue
			}
		}
		prepare, finish := context.WithTimeout(ctx, 10*time.Second)
		err = ExecuteNoninteractiveContext(prepare, sshCommand(c.Workspace.Host, shellArgs([]string{remoteBin, "bridge-prepare-socket", "--config", remoteCfg}), false), nil, io.Discard, log)
		finish()
		if err != nil {
			fmt.Fprintln(log, "Reverse bridge socket was not changed; inspect its owner before retrying.")
			if err = pause(ctx, delay, log); err != nil {
				return nil
			}
			delay = backoff(delay)
			continue
		}
		cmd := sshCommand(c.Workspace.Host, "", false)
		cmd.Args = cmd.Args[:len(cmd.Args)-1]
		cmd.Args = append([]string{"-N", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=yes", "-R", c.Bridge.RemoteSocket + ":" + c.Bridge.Socket}, cmd.Args...)
		fmt.Fprintln(log, "Connecting Mac bridge to", c.Workspace.Host)
		err = ExecuteContext(ctx, cmd, nil, io.Discard, log)
		if ctx.Err() != nil {
			return nil
		}
		fmt.Fprintln(log, "Check the reverse SSH error above; an occupied remote socket needs its owner stopped or a stale socket removed manually.")
		if err = pause(ctx, delay, log); err != nil {
			return nil
		}
		delay = backoff(delay)
	}
}
func WaitRemoteBridge(ctx context.Context, c Config) error {
	bin := filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox")
	cfg := filepath.Join(c.Workspace.RemoteDir, "config.json")
	waitCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	var lastErr error
	for i := 0; i < 60 && waitCtx.Err() == nil; i++ {
		// Include a fresh SSH handshake, not just the socket health request.
		probe, done := context.WithTimeout(waitCtx, 10*time.Second)
		_, lastErr = Output(probe, sshCommand(c.Workspace.Host, shellArgs([]string{bin, "bridge-health", "--config", cfg}), false))
		done()
		if lastErr == nil {
			return nil
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("reverse SSH bridge did not become ready: %w; see %s.log", lastErr, c.Bridge.Socket)
}
