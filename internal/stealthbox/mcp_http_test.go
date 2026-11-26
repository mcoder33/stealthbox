package stealthbox

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type mcpHTTPFixture struct {
	t    *testing.T
	url  string
	key  []byte
	http *http.Client
}

func startMCPHTTPFixture(t *testing.T, c Config) mcpHTTPFixture {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return startMCPHTTPOnListener(t, c, ln)
}

func startMCPHTTPOnListener(t *testing.T, c Config, ln net.Listener) mcpHTTPFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	key := bytes.Repeat([]byte{0x83}, 32)
	done := make(chan error, 1)
	go func() { done <- ServeMCPHTTP(ctx, c, ln, key) }()
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("MCP HTTP service did not stop")
		}
	})
	return mcpHTTPFixture{t: t, url: "http://" + ln.Addr().String(), key: key, http: client}
}

func (f mcpHTTPFixture) bearer(scope MCPScope) string {
	f.t.Helper()
	token, err := MCPHTTPBearer(f.key, scope)
	if err != nil {
		f.t.Fatal(err)
	}
	return token
}

func (f mcpHTTPFixture) request(ctx context.Context, method, path, token, session, body string) *http.Request {
	f.t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, f.url+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("MCP-Session-Id", session)
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	}
	return req
}

func (f mcpHTTPFixture) exchange(req *http.Request, status int) (http.Header, string) {
	f.t.Helper()
	res, err := f.http.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != status {
		f.t.Fatalf("%s %s: status %d, want %d; body %q; read %v", req.Method, req.URL.Path, res.StatusCode, status, body, err)
	}
	return res.Header, string(body)
}

func (f mcpHTTPFixture) post(token, session, body string, status int) string {
	f.t.Helper()
	_, reply := f.exchange(f.request(context.Background(), http.MethodPost, "/mcp", token, session, body), status)
	return reply
}

const mcpHTTPInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`

func (f mcpHTTPFixture) initialize(token string) string {
	f.t.Helper()
	headers, reply := f.exchange(f.request(context.Background(), http.MethodPost, "/mcp", token, "", mcpHTTPInitialize), http.StatusOK)
	session := headers.Get("MCP-Session-Id")
	if session == "" || !strings.Contains(reply, `"protocolVersion":"2025-11-25"`) {
		f.t.Fatalf("missing session or wrong handshake: %q %q", session, reply)
	}
	if body := f.post(token, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, http.StatusAccepted); body != "" {
		f.t.Fatal("notification produced a JSON-RPC response")
	}
	return session
}

func TestMCPHTTPClientScopes(t *testing.T) {
	c := rootConfig(t)
	a := createCheckout(t, c, "a", "checkout-a")
	b := createCheckout(t, c, "b", "checkout-b")
	c.Projects["first"] = Project{Source: Endpoint{Path: a}, Runners: map[string]Endpoint{"vm": {Path: a}}}
	c.Projects["second"] = Project{Source: Endpoint{Path: b}, Runners: map[string]Endpoint{"vm": {Path: b}}}
	t.Setenv("STEALTHBOX_SCOPE", "workspace")
	t.Setenv("STEALTHBOX_PROJECT", "first")
	t.Setenv("STEALTHBOX_RUNNER", "mac")
	f := startMCPHTTPFixture(t, c)

	firstToken := f.bearer(MCPScope{Project: "first", Runner: "vm"})
	secondToken := f.bearer(MCPScope{Project: "second", Runner: "vm"})
	workspaceToken := f.bearer(MCPScope{Workspace: true, Runner: "vm"})
	first := f.initialize(firstToken)
	second := f.initialize(secondToken)
	workspace := f.initialize(workspaceToken)
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"runner_run","arguments":{"argv":["cat","name"]}}}`
	if body := f.post(firstToken, first, call, http.StatusOK); !strings.Contains(body, "checkout-a") || strings.Contains(body, "checkout-b") {
		t.Fatal(body)
	}
	if body := f.post(secondToken, second, call, http.StatusOK); !strings.Contains(body, "checkout-b") || strings.Contains(body, "checkout-a") {
		t.Fatal(body)
	}
	f.post(secondToken, first, call, http.StatusNotFound)
	f.post(f.bearer(MCPScope{Project: "first", Runner: "vm"}), first, call, http.StatusNotFound)
	cross := strings.Replace(call, `"argv"`, `"project":"second","argv"`, 1)
	if body := f.post(firstToken, first, cross, http.StatusOK); !strings.Contains(body, "bound to project first") {
		t.Fatal(body)
	}
	if body := f.post(workspaceToken, workspace, call, http.StatusOK); !strings.Contains(body, "require a checkout path") {
		t.Fatal(body)
	}
	workspaceCall := strings.Replace(call, `"argv"`, `"path":"b","argv"`, 1)
	if body := f.post(workspaceToken, workspace, workspaceCall, http.StatusOK); !strings.Contains(body, "checkout-b") {
		t.Fatal(body)
	}
}

func TestMCPHTTPTransportBoundaries(t *testing.T) {
	c := bridgeConfig(t)
	f := startMCPHTTPFixture(t, c)
	token := f.bearer(MCPScope{Project: "test", Runner: "vm"})
	const challenge = "MCP fixture random challenge"
	health := f.request(context.Background(), http.MethodGet, "/health", "", "", "")
	health.Header.Del("Authorization")
	health.Header.Set(mcpHTTPChallengeHeader, challenge)
	header, _ := f.exchange(health, http.StatusNoContent)
	if got := header.Get(mcpHTTPProofHeader); got != mcpHTTPHealthProof(f.key, challenge) {
		t.Fatalf("health proof does not match challenge: %q", got)
	}
	f.exchange(f.request(context.Background(), http.MethodGet, "/health", "", "", ""), http.StatusBadRequest)

	for _, test := range []struct {
		name, host, origin, token string
		status                    int
	}{
		{name: "valid credential", token: token, status: http.StatusOK},
		{name: "missing credential", status: http.StatusUnauthorized},
		{name: "tampered credential", token: token + "x", status: http.StatusUnauthorized},
		{name: "unconfigured project", token: f.bearer(MCPScope{Project: "missing", Runner: "vm"}), status: http.StatusUnauthorized},
		{name: "unconfigured runner", token: f.bearer(MCPScope{Project: "test", Runner: "missing"}), status: http.StatusUnauthorized},
		{name: "unexpected host", host: "outside.example", token: token, status: http.StatusForbidden},
		{name: "unexpected origin", origin: "https://outside.example", token: token, status: http.StatusForbidden},
		{name: "matching origin", origin: f.url, token: token, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := f.request(context.Background(), http.MethodPost, "/mcp", test.token, "", mcpHTTPInitialize)
			if test.host != "" {
				req.Host = test.host
			}
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			f.exchange(req, test.status)
		})
	}
	f.exchange(f.request(context.Background(), http.MethodGet, "/mcp", token, "", ""), http.StatusMethodNotAllowed)
	f.exchange(f.request(context.Background(), http.MethodDelete, "/mcp", token, "", ""), http.StatusMethodNotAllowed)
	f.exchange(f.request(context.Background(), http.MethodPost, "/run", token, "", "{}"), http.StatusNotFound)
	f.post(token, "", strings.Repeat(" ", (2<<20)+1), http.StatusRequestEntityTooLarge)
	chunked := f.request(context.Background(), http.MethodPost, "/mcp", token, "", strings.Repeat(" ", (2<<20)+1))
	chunked.ContentLength = -1
	f.exchange(chunked, http.StatusRequestEntityTooLarge)
	f.post(token, "", mcpHTTPInitialize+"\n{}", http.StatusBadRequest)
	f.post(token, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, http.StatusBadRequest)
	session := f.initialize(token)
	unsupported := f.request(context.Background(), http.MethodPost, "/mcp", token, session, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	unsupported.Header.Set("MCP-Protocol-Version", "invalid")
	f.exchange(unsupported, http.StatusBadRequest)
	for _, version := range []string{"2025-03-26", "2025-06-18"} {
		req := f.request(context.Background(), http.MethodPost, "/mcp", token, "", strings.Replace(mcpHTTPInitialize, "2025-11-25", version, 1))
		headers, body := f.exchange(req, http.StatusOK)
		if !strings.Contains(body, `"protocolVersion":"`+version+`"`) {
			t.Fatal(body)
		}
		req = f.request(context.Background(), http.MethodPost, "/mcp", token, headers.Get("MCP-Session-Id"), `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
		req.Header.Set("MCP-Protocol-Version", version)
		f.exchange(req, http.StatusOK)
	}
}

func TestMCPHTTPSessionHeadersAndMessages(t *testing.T) {
	f := startMCPHTTPFixture(t, bridgeConfig(t))
	token := f.bearer(MCPScope{Project: "test", Runner: "vm"})
	session := f.initialize(token)
	const ping = `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	for _, test := range []struct {
		name   string
		header string
		values []string
		status int
	}{
		{name: "no protocol header defaults to session", header: mcpProtocolHeader, status: http.StatusOK},
		{name: "unsupported protocol on discovery", header: mcpProtocolHeader, values: []string{"2026-07-28"}, status: http.StatusBadRequest},
		{name: "protocol differs from session", header: mcpProtocolHeader, values: []string{"2025-06-18"}, status: http.StatusBadRequest},
		{name: "duplicate session header", header: mcpSessionHeader, values: []string{session, session}, status: http.StatusBadRequest},
		{name: "duplicate authorization", header: "Authorization", values: []string{"Bearer " + token, "Bearer " + token}, status: http.StatusUnauthorized},
		{name: "duplicate origin", header: "Origin", values: []string{f.url, f.url}, status: http.StatusForbidden},
		{name: "application json accepts utf8 parameter", header: "Content-Type", values: []string{"application/json; charset=utf-8"}, status: http.StatusOK},
		{name: "plain text body rejected", header: "Content-Type", values: []string{"text/plain"}, status: http.StatusUnsupportedMediaType},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := f.request(context.Background(), http.MethodPost, mcpHTTPPath, token, session, ping)
			req.Header.Del(test.header)
			for _, value := range test.values {
				req.Header.Add(test.header, value)
			}
			f.exchange(req, test.status)
		})
	}
	for _, body := range []string{
		mcpHTTPInitialize,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"1.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2}`,
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"\xff\"}",
	} {
		f.post(token, session, body, http.StatusBadRequest)
	}
}

func TestMCPHTTPSessionsDoNotConsumeIdleCapacity(t *testing.T) {
	c := bridgeConfig(t)
	f := startMCPHTTPFixture(t, c)
	token := f.bearer(MCPScope{Project: "test", Runner: "vm"})
	first := f.initialize(token)
	for range 1024 {
		request := f.request(context.Background(), http.MethodPost, "/mcp", token, "", mcpHTTPInitialize)
		f.exchange(request, http.StatusOK)
	}
	f.post(token, first, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, http.StatusOK)
}

func TestMCPHTTPCancelsOnlyMatchingSession(t *testing.T) {
	c := bridgeConfig(t)
	f := startMCPHTTPFixture(t, c)
	token := f.bearer(MCPScope{Project: "test", Runner: "vm"})
	first := f.initialize(token)
	second := f.initialize(token)
	root := c.Projects["test"].Runners["vm"].Path
	marker := filepath.Join(root, "http-active")
	req := f.request(context.Background(), http.MethodPost, "/mcp", token, first, mcpHTTPWaitCommand)
	done := mcpHTTPStatusAsync(f.http, req)
	waitForMCPHTTPProcess(t, marker)
	f.post(token, first, mcpHTTPWaitCommand, http.StatusBadRequest)
	notify := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"same"}}`
	f.post(token, second, notify, http.StatusAccepted)
	select {
	case status := <-done:
		t.Fatalf("other session cancelled active request: %d", status)
	case <-time.After(100 * time.Millisecond):
	}
	f.post(token, first, notify, http.StatusAccepted)
	select {
	case status := <-done:
		if status != http.StatusAccepted {
			t.Fatalf("cancelled call: HTTP %d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("matching cancellation did not stop command")
	}
	f.post(token, first, `{"jsonrpc":"2.0","id":"different","method":"ping"}`, http.StatusOK)
}

const mcpHTTPWaitCommand = `{"jsonrpc":"2.0","id":"same","method":"tools/call","params":{"name":"vm_exec","arguments":{"argv":["sh","-c","echo $$ > http-active; exec sleep 30"]}}}`

func mcpHTTPStatusAsync(client *http.Client, req *http.Request) <-chan int {
	done := make(chan int, 1)
	go func() {
		res, err := client.Do(req)
		if err != nil {
			done <- 0
			return
		}
		res.Body.Close()
		done <- res.StatusCode
	}()
	return done
}

func waitForMCPHTTPProcess(t *testing.T, marker string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pid := readProcessFixturePID(marker); pid > 0 {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("MCP HTTP command did not start")
	return 0
}

func TestMCPHTTPDisconnectCancelsCommand(t *testing.T) {
	c := bridgeConfig(t)
	f := startMCPHTTPFixture(t, c)
	token := f.bearer(MCPScope{Project: "test", Runner: "vm"})
	session := f.initialize(token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := f.request(ctx, http.MethodPost, "/mcp", token, session, mcpHTTPWaitCommand)
	done := mcpHTTPStatusAsync(f.http, req)
	pid := waitForMCPHTTPProcess(t, filepath.Join(c.Projects["test"].Runners["vm"].Path, "http-active"))
	cancel()
	select {
	case status := <-done:
		if status != 0 {
			t.Fatalf("client received reply after disconnect: %d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the disconnected request did not return")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owned command %d was still running after client disconnected", pid)
}

func TestMCPHTTPCommandTimeout(t *testing.T) {
	c := bridgeConfig(t)
	c.Bridge.TimeoutSeconds = 1
	f := startMCPHTTPFixture(t, c)
	token := f.bearer(MCPScope{Project: "test", Runner: "vm"})
	session := f.initialize(token)
	if body := f.post(token, session, mcpHTTPWaitCommand, http.StatusOK); !strings.Contains(body, `"isError":true`) {
		t.Fatal(body)
	}
	if _, err := os.Stat(filepath.Join(c.Projects["test"].Runners["vm"].Path, "http-active")); err != nil {
		t.Fatal("deadline test did not reach execution", err)
	}
}

func TestMCPHTTPReadTimeoutDoesNotCancelSilentCall(t *testing.T) {
	t.Parallel()
	c := bridgeConfig(t)
	c.Bridge.TimeoutSeconds = 20
	f := startMCPHTTPFixture(t, c)
	f.http.Timeout = 25 * time.Second
	token := f.bearer(MCPScope{Project: "test", Runner: "vm"})
	session := f.initialize(token)
	const call = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"vm_exec","arguments":{"argv":["sh","-c","sleep 12; printf finished"]}}}`
	body := f.post(token, session, call, http.StatusOK)
	if !strings.Contains(body, "finished") || !strings.Contains(body, `"isError":false`) {
		t.Fatal(body)
	}
}

func TestMCPHTTPAllowsBridgeQueueAndExecutionBudgets(t *testing.T) {
	c := bridgeConfig(t)
	c.Bridge.TimeoutSeconds = 1
	root, err := os.MkdirTemp("/tmp", "sb-mcp-http-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	c.Bridge.Socket = filepath.Join(root, "bridge.sock")
	c.Projects["test"].Runners["mac"] = Endpoint{Bridge: true, Path: c.Projects["test"].Runners["mac"].Path}
	listener, err := net.Listen("unix", c.Bridge.Socket)
	if err != nil {
		t.Fatal(err)
	}
	bridge := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/run":
			timer := time.NewTimer(1200 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
			case <-timer.C:
				_, _ = io.WriteString(w, "{\"stream\":\"stdout\",\"data\":\"bridge-finished\"}\n{\"stream\":\"exit\",\"code\":0}\n")
			}
		default:
			http.NotFound(w, r)
		}
	})}
	go bridge.Serve(listener)
	t.Cleanup(func() { _ = bridge.Close() })
	f := startMCPHTTPFixture(t, c)
	token := f.bearer(MCPScope{Project: "test", Runner: "local"})
	session := f.initialize(token)
	response := f.post(token, session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"runner_run","arguments":{"argv":["true"]}}}`, http.StatusOK)
	if !strings.Contains(response, "bridge-finished") || !strings.Contains(response, `"isError":false`) {
		t.Fatal(response)
	}
}

func TestMCPHTTPSlowReaderCannotHoldResponse(t *testing.T) {
	c := bridgeConfig(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan string, 4)
	f := startMCPHTTPOnListener(t, c, &mcpHTTPCloseObserver{Listener: listener, closed: closed})
	token := f.bearer(MCPScope{Project: "test", Runner: "vm"})
	session := f.initialize(token)
	const output = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"vm_exec","arguments":{"argv":["head","-c","1048576","/dev/zero"]}}}`
	req := f.request(context.Background(), http.MethodPost, "/mcp", token, session, output)
	conn, err := net.Dial("tcp4", req.URL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(mcpHTTPWriteTimeout + 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("slow request response headers: %#v", res)
	}
	// Start a separate request while the original client holds the response unread.
	f.post(token, session, `{"jsonrpc":"2.0","id":3,"method":"ping"}`, http.StatusOK)
	// Wait for the server to close the keep-alive connection after the stalled
	// response. TCP may still have unread bytes on the client after this point.
	deadline := time.NewTimer(mcpHTTPWriteTimeout + 2*time.Second)
	defer deadline.Stop()
	for {
		select {
		case peer := <-closed:
			if peer == conn.LocalAddr().String() {
				return
			}
		case <-deadline.C:
			t.Fatal("server did not release the slow response at its write deadline")
		}
	}
}

type mcpHTTPCloseObserver struct {
	net.Listener
	closed chan<- string
}

func (l *mcpHTTPCloseObserver) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &mcpHTTPObservedConn{Conn: conn, closed: l.closed}, nil
}

type mcpHTTPObservedConn struct {
	net.Conn
	closed chan<- string
}

func (c *mcpHTTPObservedConn) Close() error {
	peer := c.RemoteAddr().String()
	err := c.Conn.Close()
	select {
	case c.closed <- peer:
	default:
	}
	return err
}

func TestMCPCappedBufferCapsBridgeStringWrites(t *testing.T) {
	buf := &cappedBuffer{limit: 4}
	n, err := io.WriteString(buf, "0123456789")
	if err != nil || n != 10 || !strings.HasPrefix(buf.Text(), "0123\n[output truncated;") {
		t.Fatalf("unexpected capture and write result: %q, %d, %v", buf.Text(), n, err)
	}
}
