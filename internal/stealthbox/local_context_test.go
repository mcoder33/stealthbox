package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func contextTestConfig(t *testing.T) Config {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "sbctx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	c := sourceTestConfig(t)
	c.Bridge.Enabled = true
	c.Bridge.Socket = filepath.Join(base, "bridge.sock")
	c.Bridge.RemoteSocket = c.Bridge.Socket
	c.Workspace.RemoteDir = filepath.Join(base, "remote")
	return c
}

func startContextBridge(t *testing.T, c Config, handler http.Handler) {
	t.Helper()
	l, err := net.Listen("unix", c.Bridge.Socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: handler}
	go s.Serve(l)
	t.Cleanup(func() { s.Close(); l.Close() })
}

func envValue(environment []string, key string) string {
	for _, entry := range environment {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value
		}
	}
	return ""
}

func TestLocalContextTerminalEnvironmentAndRefresh(t *testing.T) {
	c := contextTestConfig(t)
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("STEALTHBOX_TEST_TOKEN", "terminal token\nwith spaces = literal")
	t.Setenv("STEALTHBOX_TEST_OLD", "old value") // These internal names must not travel.
	t.Setenv("TEST_SERVICE_TOKEN", "terminal token\nwith spaces = literal")
	t.Setenv("TEST_REMOVED_TOKEN", "was on Mac")
	handler := BridgeHandler(c)
	startContextBridge(t, c, handler)
	t.Setenv("TEST_SERVICE_TOKEN", "VM stale value")
	t.Setenv("TEST_REMOVED_TOKEN", "VM stale value")
	env, err := contextEnvironment(context.Background(), c, "/vm/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if envValue(env, "TEST_SERVICE_TOKEN") != "terminal token\nwith spaces = literal" {
		t.Fatal("terminal value corrupted")
	}
	if envValue(env, "HOME") != os.Getenv("HOME") || envValue(env, "PATH") != os.Getenv("PATH") {
		t.Fatal("VM machine environment overwritten")
	}
	if !strings.Contains(envValue(env, "GIT_CONFIG_VALUE_1"), "git-credential") || envValue(env, "GIT_CONFIG_VALUE_2") != "true" {
		t.Fatal("Git helper not configured")
	}
	if !strings.Contains(envValue(env, "STEALTHBOX_LOCAL_ENV_KEYS"), "TEST_REMOVED_TOKEN") {
		t.Fatal("forwarded keys missing")
	}
	first := configHash(c)
	t.Setenv("TEST_SERVICE_TOKEN", "new token")
	if configHash(c) == first {
		t.Fatal("changed terminal env did not invalidate bridge snapshot")
	}
	// A newly launched process must remove old exports absent from the next
	// Mac snapshot, even if launched from a panel that inherited them.
	var snapshot localContextSnapshot
	snapshot.Environment = map[string]string{"TEST_SERVICE_TOKEN": "fresh"}
	newHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/local-context" {
			json.NewEncoder(w).Encode(snapshot)
		} else {
			handler.ServeHTTP(w, r)
		}
	})
	// Reuse the HTTP server through a switchable test handler on another socket.
	c.Bridge.Socket += "2"
	c.Bridge.RemoteSocket = c.Bridge.Socket
	startContextBridge(t, c, newHandler)
	t.Setenv("STEALTHBOX_LOCAL_ENV_KEYS", "TEST_REMOVED_TOKEN,TEST_SERVICE_TOKEN")
	env, err = contextEnvironment(context.Background(), c, "/vm/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if envValue(env, "TEST_REMOVED_TOKEN") != "" || envValue(env, "TEST_SERVICE_TOKEN") != "fresh" {
		t.Fatal("old terminal exports survived refresh")
	}
}

func TestLocalContextAuthorizationAndOptOut(t *testing.T) {
	c := contextTestConfig(t)
	for _, path := range []string{"/local-context", "/git-credential", "/ssh-agent", "/git-ssh"} {
		req, _ := http.NewRequest("GET", "http://unix"+path, nil)
		recorder := httptest.NewRecorder()
		BridgeHandler(c).ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatal(path, recorder.Code)
		}
	}
	falseValue := false
	c.Bridge.LocalContext = &falseValue
	handler := BridgeHandler(c)
	for _, path := range []string{"/local-context", "/git-credential", "/ssh-agent", "/git-ssh"} {
		req, _ := http.NewRequest("GET", "http://unix"+path, nil)
		req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusForbidden {
			t.Fatal(path, recorder.Code)
		}
	}
	if transferableEnvironment("SSH_AUTH_SOCK") || transferableEnvironment("GIT_CONFIG_VALUE_0") || transferableEnvironment("PATH") || transferableEnvironment("HOME") {
		t.Fatal("machine variables forwarded")
	}
	if !transferableEnvironment("GITLAB_TOKEN") || !transferableEnvironment("DATABASE_URL") {
		t.Fatal("required exported variables filtered")
	}
	t.Setenv("STEALTHBOX_LOCAL_ENV_KEYS", "TEST_SERVICE_TOKEN")
	t.Setenv("TEST_SERVICE_TOKEN", "old exported secret")
	t.Setenv("GIT_CONFIG_COUNT", "3")
	t.Setenv("GIT_SSH_COMMAND", "old bridge helper")
	t.Setenv("SSH_AUTH_SOCK", credentialAgentSocket(c))
	env, err := contextEnvironment(context.Background(), c, "config.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TEST_SERVICE_TOKEN", "GIT_CONFIG_COUNT", "GIT_SSH_COMMAND", "SSH_AUTH_SOCK", "STEALTHBOX_LOCAL_ENV_KEYS"} {
		if envValue(env, key) != "" {
			t.Fatal("opt-out retained inherited local context", key)
		}
	}
}

func TestGitCredentialUsesMacRepositoryHelper(t *testing.T) {
	c := contextTestConfig(t)
	directory := initTestGit(t, c.Workspace.LocalRoot, "team/repo")
	testGit(t, directory, "config", "credential.helper", "!f() { printf 'username=mac-user\\npassword=mac-secret\\n'; }; f")
	testGit(t, directory, "config", "credential.useHttpPath", "true")
	startContextBridge(t, c, BridgeHandler(c))
	oldCWD, _ := os.Getwd()
	os.MkdirAll(filepath.Join(c.Workspace.VMRoot, "team/repo"), 0700)
	os.Chdir(filepath.Join(c.Workspace.VMRoot, "team/repo"))
	t.Cleanup(func() { os.Chdir(oldCWD) })
	var output bytes.Buffer
	if err := GitCredential(context.Background(), c, "get", strings.NewReader("protocol=https\nhost=example.test\npath=team/repo.git\n\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "password=mac-secret") || !strings.Contains(output.String(), "username=mac-user") {
		t.Fatal("Mac credential not returned")
	}
	output.Reset()
	if err := GitCredential(context.Background(), c, "store", strings.NewReader("password=replace\n"), &output); err != nil || output.Len() != 0 {
		t.Fatal("store modified credential", err)
	}
	for _, directory := range []string{"../outside", "team/../../outside"} {
		_, err := macGitCredential(context.Background(), c, gitCredentialRequest{Protocol: "https", Host: "example.test", Directory: directory})
		if err == nil {
			t.Fatal("credential directory traversal accepted")
		}
	}
	if _, err := (gitCredentialRequest{Protocol: "https", Host: "example.test\npassword=injected"}).input(); err == nil {
		t.Fatal("credential protocol injection accepted")
	}
}

func TestGitCredentialUsesEmbeddedMacURLAndHostSpecificGLabStore(t *testing.T) {
	c := contextTestConfig(t)
	dir := initTestGit(t, c.Workspace.LocalRoot, "repo")
	testGit(t, dir, "remote", "add", "origin", "https://oauth2:embedded-secret@gitlab.fixture/team/repo.git")
	request := gitCredentialRequest{Protocol: "https", Host: "gitlab.fixture", Path: "team/repo.git", Directory: "repo"}
	credential, err := macGitCredential(context.Background(), c, request)
	if err != nil || !bytes.Contains(credential, []byte("password=embedded-secret")) {
		t.Fatal("embedded URL credential unavailable", err)
	}
	testGit(t, dir, "remote", "remove", "origin")
	testGit(t, dir, "config", "credential.helper", "")
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0700)
	os.WriteFile(filepath.Join(bin, "glab"), []byte("#!/bin/sh\n[ -z \"$GITLAB_TOKEN\" ] || exit 1\n[ \"$*\" = 'config get token --host gitlab.fixture --global' ] || exit 1\nprintf 'host-specific-secret\\n'\n"), 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITLAB_TOKEN", "must-not-leak-to-another-host")
	credential, err = macGitCredential(context.Background(), c, request)
	if err != nil || !bytes.Contains(credential, []byte("password=host-specific-secret")) {
		t.Fatal("glab host credential unavailable", err)
	}
	request.Host = "unrelated.fixture"
	if _, err = macGitCredential(context.Background(), c, request); err == nil {
		t.Fatal("credential sent to unrelated host")
	}
}

func TestCredentialAgentRelaysRealKeyAndKeepsSocketAcrossBridgeRestart(t *testing.T) {
	for _, name := range []string{"ssh-agent", "ssh-add", "ssh-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " unavailable")
		}
	}
	c := contextTestConfig(t)
	localSocket := filepath.Join(filepath.Dir(c.Bridge.Socket), "mac-agent.sock")
	agent := exec.Command("ssh-agent", "-D", "-a", localSocket)
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Process.Kill(); agent.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(localSocket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("local agent not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Setenv("SSH_AUTH_SOCK", localSocket)
	key := filepath.Join(filepath.Dir(c.Bridge.Socket), "key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if out, err := exec.Command("ssh-add", key).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	configPath := filepath.Join(filepath.Dir(c.Bridge.Socket), "config.json")
	if err := Save(configPath, c); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", c.Bridge.Socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: BridgeHandler(c)}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ServeCredentialAgent(ctx, c, configPath) }()
	proxy := credentialAgentSocket(c)
	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(proxy); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	check := func() {
		t.Helper()
		cmd := exec.Command("ssh-add", "-l")
		cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+proxy)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "ED25519") {
			t.Fatal("agent relay failed", err, string(out))
		}
	}
	check()
	// A real signature exercises requests beyond listing identities.
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-U", "-f", key+".pub", "-n", "stealthbox-test")
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+proxy)
	cmd.Stdin = strings.NewReader("sign me")
	if out, err := cmd.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("BEGIN SSH SIGNATURE")) {
		t.Fatal("agent signing failed", err, string(out))
	}
	server.Close()
	listener.Close()
	listener, err = net.Listen("unix", c.Bridge.Socket)
	if err != nil {
		t.Fatal(err)
	}
	server = &http.Server{Handler: BridgeHandler(c)}
	go server.Serve(listener)
	check()
	if info, err := os.Stat(proxy); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("relay socket lost or public", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop")
	}
}

// Preserve one agent connection across messages, as required by OpenSSH's
// session-bind extension and destination-constrained keys.
func TestCredentialAgentPreservesConnectionState(t *testing.T) {
	c := contextTestConfig(t)
	socket := filepath.Join(filepath.Dir(c.Bridge.Socket), "state-agent.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t.Setenv("SSH_AUTH_SOCK", socket)
	go func() {
		conn, e := l.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		for _, wanted := range []byte{1, 2} {
			b := make([]byte, 1)
			if _, e = io.ReadFull(conn, b); e != nil || b[0] != wanted {
				return
			}
			conn.Write([]byte{wanted + 10})
		}
	}()
	startContextBridge(t, c, BridgeHandler(c))
	client, proxy := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go proxyAgentConnection(ctx, c, proxy)
	client.SetDeadline(time.Now().Add(3 * time.Second))
	for _, n := range []byte{1, 2} {
		if _, err = client.Write([]byte{n}); err != nil {
			t.Fatal(err)
		}
		answer := make([]byte, 1)
		if _, err = io.ReadFull(client, answer); err != nil || answer[0] != n+10 {
			t.Fatal("agent connection state lost", err)
		}
	}
}
