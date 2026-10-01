package stealthbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBridgeShutdownKillsCommandGroup(t *testing.T) {
	c := bridgeConfig(t)
	c.Bridge.AllowExec = true
	c.Bridge.Socket = filepath.Join(canonicalTemp(t), "stop.sock")
	if len(c.Bridge.Socket) > 100 {
		t.Skip("socket path too long")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- serveManagedBridge(ctx, c, ready, cancel) }()
	<-ready
	commandDone := make(chan error, 1)
	go func() {
		commandDone <- RunBridge(context.Background(), c, "test", []string{"sh", "-c", "echo $$ > pid; sleep 60"}, false, "", io.Discard, io.Discard)
	}()
	pidPath := filepath.Join(c.Projects["test"].Runners["mac"].Path, "pid")
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, e := os.ReadFile(pidPath)
		if e == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("command did not start")
	}
	if e := StopBridgeService(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("bridge did not finish")
	}
	if e := syscall.Kill(-pid, 0); e == nil {
		t.Fatal("command group survived shutdown")
	}
	select {
	case <-commandDone:
	case <-time.After(time.Second):
		t.Fatal("client remained blocked")
	}
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func bridgeConfig(t *testing.T) Config {
	t.Helper()
	c, e := DefaultConfig()
	if e != nil {
		t.Fatal(e)
	}
	c.Bridge.Enabled = true
	c.Projects = map[string]Project{"test": {Source: Endpoint{Path: canonicalTemp(t)}, Runners: map[string]Endpoint{"mac": {Path: filepath.Join(canonicalTemp(t), "runner")}, "vm": {Path: canonicalTemp(t)}}}}
	return c
}
func requestBridge(t *testing.T, h http.Handler, c Config, r RunRequest, body io.Reader, token string) *httptest.ResponseRecorder {
	t.Helper()
	meta, _ := json.Marshal(r)
	req := httptest.NewRequest("POST", "http://unix/run", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Stealthbox-Request", base64.RawURLEncoding.EncodeToString(meta))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
func snapshotFor(t *testing.T, path string) *bytes.Buffer {
	t.Helper()
	b := &bytes.Buffer{}
	if e := Snapshot(path, b, 1<<20); e != nil {
		t.Fatal(e)
	}
	return b
}
func TestBridgeSyncAndExitStatus(t *testing.T) {
	c := bridgeConfig(t)
	src := c.Projects["test"].Source.Path
	if e := os.WriteFile(src+"/code", []byte("uncommitted"), 0600); e != nil {
		t.Fatal(e)
	}
	h := BridgeHandler(c)
	w := requestBridge(t, h, c, RunRequest{Project: "test", Args: []string{"sh", "-c", "cat code; echo diagnostic >&2; exit 7"}, Sync: true}, snapshotFor(t, src), c.Bridge.Token)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var events []Event
	dec := json.NewDecoder(w.Body)
	for {
		var e Event
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	var stdout, stderr string
	code := -1
	for _, e := range events {
		switch e.Stream {
		case "stdout":
			stdout += e.Data
		case "stderr":
			stderr += e.Data
		case "exit":
			code = e.Code
		}
	}
	if stdout != "uncommitted" || !strings.Contains(stderr, "diagnostic") || code != 7 {
		t.Fatal(events)
	}
}
func TestBridgeAuthorizationAndExecPolicy(t *testing.T) {
	c := bridgeConfig(t)
	h := BridgeHandler(c)
	for _, tc := range []struct {
		token  string
		r      RunRequest
		status int
	}{{"wrong", RunRequest{Project: "test", Args: []string{"true"}}, 401}, {c.Bridge.Token, RunRequest{Project: "unknown", Args: []string{"true"}}, 403}, {c.Bridge.Token, RunRequest{Project: "test", Args: []string{"true"}}, 403}} {
		w := requestBridge(t, h, c, tc.r, nil, tc.token)
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestArchiveTraversalAndLinksRejected(t *testing.T) {
	for _, header := range []tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Size: 1}, {Name: "/absolute", Typeflag: tar.TypeReg, Size: 1}, {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, {Name: ".env", Typeflag: tar.TypeReg, Size: 1}} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		if e := tw.WriteHeader(&header); e != nil {
			t.Fatal(e)
		}
		if header.Size > 0 {
			_, _ = tw.Write([]byte("x"))
		}
		_ = tw.Close()
		if e := SyncSnapshot(filepath.Join(canonicalTemp(t), "runner"), &b, 1<<20); e == nil {
			t.Fatalf("unsafe header accepted: %+v", header)
		}
	}
}
func TestArchivePreservesSecretsAndDeletesStale(t *testing.T) {
	src := canonicalTemp(t)
	root := filepath.Join(canonicalTemp(t), "runner")
	_ = os.WriteFile(src+"/code", []byte("first"), 0600)
	_ = os.WriteFile(src+"/.env", []byte("source-secret"), 0600)
	if e := SyncSnapshot(root, snapshotFor(t, src), 1<<20); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(root + "/.env"); !os.IsNotExist(e) {
		t.Fatal("source .env copied")
	}
	_ = os.WriteFile(root+"/.env", []byte("mac-secret"), 0600)
	_ = os.Remove(src + "/code")
	if e := SyncSnapshot(root, snapshotFor(t, src), 1<<20); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(root + "/code"); !os.IsNotExist(e) {
		t.Fatal("stale file survived")
	}
	b, e := os.ReadFile(root + "/.env")
	if e != nil || string(b) != "mac-secret" {
		t.Fatal("secret removed", e)
	}
}
func TestRunnerPathSymlinkRejected(t *testing.T) {
	dir := canonicalTemp(t)
	outside := canonicalTemp(t)
	if e := os.Symlink(outside, dir+"/link"); e != nil {
		t.Fatal(e)
	}
	if e := SyncSnapshot(dir+"/link/runner", bytes.NewReader(nil), 1<<20); e == nil {
		t.Fatal("symlink parent accepted")
	}
}
func TestUnixBridgeClient(t *testing.T) {
	c := bridgeConfig(t)
	c.Bridge.Socket = filepath.Join(canonicalTemp(t), "m.sock")
	if len(c.Bridge.Socket) > 100 {
		t.Skip("temporary path is too long for Unix socket")
	}
	c.Bridge.AllowExec = true
	src := c.Projects["test"].Source.Path
	_ = os.WriteFile(src+"/code", []byte("hello"), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- ServeBridge(ctx, c, ready) }()
	select {
	case <-ready:
	case e := <-done:
		t.Fatal(e)
	case <-time.After(5 * time.Second):
		t.Fatal("server not ready")
	}
	var out, stderr bytes.Buffer
	if e := RunBridge(ctx, c, "test", []string{"cat", "code"}, true, "", &out, &stderr); e != nil {
		t.Fatal(e)
	}
	if out.String() != "hello" {
		t.Fatal(out.String())
	}
	out.Reset()
	if e := RunBridge(ctx, c, "test", []string{"sh", "-c", "exit 9"}, false, "", &out, &stderr); e == nil {
		t.Fatal("exit status lost")
	} else {
		var ex *ExitError
		if !errors.As(e, &ex) || ex.Code != 9 {
			t.Fatal(e)
		}
	}
	cancel()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func TestBridgeCWDTraversal(t *testing.T) {
	c := bridgeConfig(t)
	c.Bridge.AllowExec = true
	root := c.Projects["test"].Runners["mac"].Path
	_ = os.MkdirAll(root, 0700)
	_ = os.Symlink(canonicalTemp(t), root+"/outside")
	for _, cwd := range []string{"../escape", "outside"} {
		w := requestBridge(t, BridgeHandler(c), c, RunRequest{Project: "test", Args: []string{"true"}, CWD: cwd}, nil, c.Bridge.Token)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestArtifactReadStaysInsideRunner(t *testing.T) {
	c := bridgeConfig(t)
	root := c.Projects["test"].Runners["mac"].Path
	_ = os.MkdirAll(root, 0700)
	_ = os.WriteFile(root+"/report.txt", []byte("artifact"), 0600)
	outside := canonicalTemp(t)
	_ = os.WriteFile(outside+"/secret", []byte("secret"), 0600)
	_ = os.Symlink(outside+"/secret", root+"/link")
	h := BridgeHandler(c)
	for _, tc := range []struct {
		path   string
		status int
	}{{"report.txt", 200}, {"../secret", 400}, {"link", 404}} {
		r := httptest.NewRequest("GET", "http://unix/file?project=test&path="+tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body.String())
		}
		if tc.status == 200 && w.Body.String() != "artifact" {
			t.Fatal(w.Body.String())
		}
	}
}
