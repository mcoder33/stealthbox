package stealthbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func credentialAgentSocket(c Config) string {
	return filepath.Join(c.Workspace.RemoteDir, "run", "git-agent.sock")
}

func relayAgent(ctx context.Context, a net.Conn, reader io.Reader, b net.Conn) {
	closeBoth := func() { _ = a.Close(); _ = b.Close() }
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	defer closeBoth()
	var copies sync.WaitGroup
	copyTo := func(destination net.Conn, source io.Reader) {
		defer copies.Done()
		if _, err := io.Copy(destination, source); err != nil {
			closeBoth()
			return
		}
		if half, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			_ = destination.Close()
		}
	}
	copies.Add(2)
	go copyTo(b, reader)
	go copyTo(a, b)
	copies.Wait()
}

func serveLocalAgent(w http.ResponseWriter, r *http.Request, c Config, socket string) {
	if socket == "" {
		http.Error(w, "the local SSH-agent is unavailable", http.StatusServiceUnavailable)
		return
	}
	_, timeout := limits(c)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	agent, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		http.Error(w, "the local SSH-agent is unavailable", http.StatusServiceUnavailable)
		return
	}
	defer agent.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "SSH-agent streaming is unavailable", http.StatusInternalServerError)
		return
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer connection.Close()
	if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = buffer.Flush(); err != nil {
		return
	}
	// Preserve the complete agent stream, including OpenSSH session bindings.
	relayAgent(ctx, connection, buffer.Reader, agent)
}

func proxyAgentConnection(ctx context.Context, c Config, client net.Conn) {
	defer client.Close()
	bridge, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Bridge.Socket)
	if err != nil {
		return
	}
	defer bridge.Close()
	_ = bridge.SetDeadline(time.Now().Add(10 * time.Second))
	req, _ := http.NewRequest("CONNECT", "http://unix/ssh-agent", nil)
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	if err = req.Write(bridge); err != nil {
		return
	}
	reader := bufio.NewReader(bridge)
	response, err := http.ReadResponse(reader, req)
	if err != nil || response.StatusCode != http.StatusOK {
		return
	}
	_ = bridge.SetDeadline(time.Time{})
	relayAgent(ctx, bridge, reader, client)
}

type agentProxyOwner struct {
	PID int `json:"pid"`
}

func ensureAgentProxy(ctx context.Context, c Config, configPath string) error {
	socket := credentialAgentSocket(c)
	if !filepath.IsAbs(socket) || len(socket) > 100 || c.Workspace.RemoteDir == "" {
		return fmt.Errorf("SSH-agent relay needs a short absolute remote state directory")
	}
	dial := func() bool {
		connection, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
		if err == nil {
			_ = connection.Close()
		}
		return err == nil
	}
	if dial() {
		return nil
	}
	if info, err := os.Lstat(socket); err == nil {
		var owner agentProxyOwner
		data, readErr := os.ReadFile(socket + ".owner")
		if readErr != nil || json.Unmarshal(data, &owner) != nil || owner.PID <= 0 || syscall.Kill(owner.PID, 0) != syscall.ESRCH {
			return fmt.Errorf("SSH-agent relay socket has an unverified owner; inspect it before restarting")
		}
		identity, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(identity.Uid) != os.Geteuid() || info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("SSH-agent relay socket has an unverified owner")
		}
		if current, e := os.Lstat(socket); e != nil || !os.SameFile(info, current) {
			return fmt.Errorf("SSH-agent relay socket changed while inspecting it")
		}
		if err = os.Remove(socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	path, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(socket+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	command := exec.Command(bin, "credential-agent", "--config", path)
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	for i := 0; i < 50; i++ {
		if dial() {
			return nil
		}
		select {
		case err := <-done:
			if dial() { // Another launcher may have won the socket race.
				return nil
			}
			return fmt.Errorf("SSH-agent relay stopped: %w", err)
		case <-ctx.Done():
			_ = command.Process.Signal(syscall.SIGTERM)
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = command.Process.Signal(syscall.SIGTERM)
	return fmt.Errorf("SSH-agent relay startup timed out")
}

func ServeCredentialAgent(ctx context.Context, c Config, configPath string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !remoteLocalContext(c) {
		return fmt.Errorf("SSH-agent relay must run in the VM workspace")
	}
	socket := credentialAgentSocket(c)
	if len(socket) > 100 {
		return fmt.Errorf("SSH-agent relay socket path is too long")
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	owner, _ := json.Marshal(agentProxyOwner{PID: os.Getpid()})
	if err = os.WriteFile(socket+".owner", owner, 0600); err != nil {
		return err
	}
	defer os.Remove(socket + ".owner")
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var connections sync.WaitGroup
	defer connections.Wait()
	defer cancel()
	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			current, err := Load(configPath)
			if err != nil || !remoteLocalContext(current) {
				_ = client.Close()
				return
			}
			proxyAgentConnection(ctx, current, client)
		}()
	}
}
