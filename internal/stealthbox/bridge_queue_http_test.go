package stealthbox

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func queueRunRequest(t *testing.T, ctx context.Context, c Config, url string, spec RunRequest) *http.Request {
	t.Helper()
	meta, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/run", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	r.Header.Set("X-Stealthbox-Request", base64.RawURLEncoding.EncodeToString(meta))
	return r
}

func queueTestBridge(t *testing.T, timeout int) (Config, string, *runnerQueue, *httptest.Server) {
	t.Helper()
	c := bridgeConfig(t)
	c.Bridge.AllowExec = true
	c.Bridge.TimeoutSeconds = timeout
	runner := c.Projects["test"].Runners["mac"].Path
	if err := os.MkdirAll(runner, 0700); err != nil {
		t.Fatal(err)
	}
	queue := &runnerQueue{waiting: make(map[string][]chan struct{})}
	server := httptest.NewServer(bridgeHandler(c, queue))
	t.Cleanup(server.Close)
	return c, runner, queue, server
}

func waitForQueueTest(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func queueDepth(queue *runnerQueue, runner string) (bool, int) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	pending, occupied := queue.waiting[runner]
	return occupied, len(pending)
}

func TestBridgeQueueRemovesDisconnectedCaller(t *testing.T) {
	c, runner, queue, server := queueTestBridge(t, 30)
	hold := queue.tryAcquire(runner)
	defer hold()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := queueRunRequest(t, ctx, c, server.URL, RunRequest{Project: "test", Args: []string{"touch", "unexpected"}})
	done := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(r)
		if err == nil {
			err = response.Body.Close()
		}
		done <- err
	}()
	waitForQueueTest(t, "request to enter the queue", func() bool {
		_, n := queueDepth(queue, runner)
		return n == 1
	})
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disconnected request got a response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not cancel")
	}
	waitForQueueTest(t, "canceled request to leave the queue while the runner was busy", func() bool {
		occupied, n := queueDepth(queue, runner)
		return occupied && n == 0
	})
	if _, err := os.Stat(filepath.Join(runner, "unexpected")); !os.IsNotExist(err) {
		t.Fatalf("disconnected command started: %v", err)
	}
}

func TestBridgeQueueExpiryReportsNotStarted(t *testing.T) {
	c, runner, queue, server := queueTestBridge(t, 1)
	hold := queue.tryAcquire(runner)
	r := queueRunRequest(t, context.Background(), c, server.URL, RunRequest{Project: "test", Args: []string{"touch", "unexpected"}})
	response, err := server.Client().Do(r)
	hold()
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusRequestTimeout || !strings.Contains(string(body), "execution=not_started") {
		t.Fatalf("queue timeout = HTTP %d %q (read error %v)", response.StatusCode, body, err)
	}
	assertEmptyRunnerQueue(t, queue)
	if _, err := os.Stat(filepath.Join(runner, "unexpected")); !os.IsNotExist(err) {
		t.Fatalf("expired command started: %v", err)
	}
}

func queueRawRequest(t *testing.T, server *httptest.Server, r *http.Request, extra string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: %s\r\nX-Stealthbox-Request: %s\r\n%s\r\n",
		r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Stealthbox-Request"), extra)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestBridgeQueueReleasesRunnerForStalledClient(t *testing.T) {
	for _, scenario := range []string{"output", "archive"} {
		t.Run(scenario, func(t *testing.T) {
			c, runner, queue, server := queueTestBridge(t, 1)
			spec := RunRequest{Project: "test", Args: []string{"sh", "-c", "echo $$ > command.pid; exec yes test-output"}}
			extra := ""
			if scenario == "archive" {
				spec.Sync = true
				extra = "Content-Length: 1024\r\n"
			}
			r := queueRunRequest(t, context.Background(), c, server.URL, spec)
			queueRawRequest(t, server, r, extra)
			waitForQueueTest(t, "stalled request to own the runner", func() bool {
				occupied, _ := queueDepth(queue, runner)
				return occupied
			})
			pidPath := filepath.Join(runner, "command.pid")
			var pid int
			if scenario == "output" {
				waitForQueueTest(t, "streaming command to start", func() bool {
					pid = readProcessFixturePID(pidPath)
					return pid > 0
				})
			}
			waitForQueueTest(t, "runner release while the unresponsive connection stays open", func() bool {
				occupied, _ := queueDepth(queue, runner)
				return !occupied
			})
			assertEmptyRunnerQueue(t, queue)
			if pid > 0 {
				waitForTerminatedProcessGroup(t, pid)
			} else if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
				t.Fatalf("command started with an incomplete archive: %v", err)
			}
			next := queueRunRequest(t, context.Background(), c, server.URL, RunRequest{Project: "test", Args: []string{"echo", "next-agent"}})
			response, err := server.Client().Do(next)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "next-agent") {
				t.Fatalf("next request = HTTP %d %s: %v", response.StatusCode, body, err)
			}
		})
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu       sync.Mutex
	deadline time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadline = deadline
	return nil
}

func TestBridgeSilentCommandHasNoIdleWriteDeadline(t *testing.T) {
	c := bridgeConfig(t)
	c.Bridge.AllowExec = true
	runner := c.Projects["test"].Runners["mac"].Path
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := queueRunRequest(t, ctx, c, "http://bridge", RunRequest{Project: "test", Args: []string{"sh", "-c", "touch started; while [ ! -f finish ]; do sleep .02; done"}})
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		BridgeHandler(c).ServeHTTP(rec, req)
	}()
	waitForQueueTest(t, "silent command to start", func() bool {
		_, err := os.Stat(filepath.Join(runner, "started"))
		return err == nil
	})
	rec.mu.Lock()
	deadline := rec.deadline
	rec.mu.Unlock()
	if !deadline.IsZero() {
		t.Fatalf("silent command kept a response deadline: %v", deadline)
	}
	if err := os.WriteFile(filepath.Join(runner, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if !strings.Contains(rec.Body.String(), `"stream":"exit"`) {
			t.Fatalf("missing exit event: %s", rec.Body)
		}
	case <-ctx.Done():
		t.Fatal("silent command did not finish")
	}
}

func TestBridgeQueueFullRejectsUnreadArchive(t *testing.T) {
	c, runner, queue, server := queueTestBridge(t, 30)
	hold := queue.tryAcquire(runner)
	defer hold()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waiters []<-chan runnerAdmission
	for depth := 1; depth <= maxRunnerWaiters; depth++ {
		waiters = append(waiters, beginRunnerWait(t, queue, runner, depth, ctx))
	}
	r := queueRunRequest(t, ctx, c, server.URL, RunRequest{Project: "test", Sync: true, Args: []string{"touch", "unexpected"}})
	conn := queueRawRequest(t, server, r, "Content-Length: 1024\r\n")
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "execution=not_started") {
		t.Fatalf("queue full = HTTP %d %q (read error %v)", response.StatusCode, body, err)
	}
	cancel()
	for _, waiter := range waiters {
		takeRunnerAdmission(t, waiter)
	}
}

func TestBridgeQueueStopsInheritedPipesAfterLeaderExit(t *testing.T) {
	c, runner, queue, server := queueTestBridge(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := RunRequest{Project: "test", Args: []string{"sh", "-c", "echo $$ > command.pid; sleep 30 & exit 7"}}
	req := queueRunRequest(t, ctx, c, server.URL, cmd)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"stream":"exit","code":7`) {
		t.Fatalf("leader's exit code was lost: HTTP %d %s: %v", response.StatusCode, body, err)
	}
	pid := readProcessFixturePID(filepath.Join(runner, "command.pid"))
	if pid <= 0 {
		t.Fatal("command process group was not recorded")
	}
	waitForTerminatedProcessGroup(t, pid)
	assertEmptyRunnerQueue(t, queue)
}

func TestBridgeQueueStalledArtifactReleasesRunner(t *testing.T) {
	c, runner, queue, server := queueTestBridge(t, 1)
	copyFile, err := os.Create(filepath.Join(runner, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFile.Truncate(32 << 20); err != nil {
		copyFile.Close()
		t.Fatal(err)
	}
	if err := copyFile.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/file?project=test&path=large", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	queueRawRequest(t, server, req, "")
	waitForQueueTest(t, "artifact to acquire the runner", func() bool {
		occupied, _ := queueDepth(queue, runner)
		return occupied
	})
	waitForQueueTest(t, "artifact to release the runner with the client still connected", func() bool {
		occupied, _ := queueDepth(queue, runner)
		return !occupied
	})
	assertEmptyRunnerQueue(t, queue)
}

func TestBridgeQueueWaitPreservesExecutionBudget(t *testing.T) {
	c, runner, queue, server := queueTestBridge(t, 2)
	hold := queue.tryAcquire(runner)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spec := RunRequest{Project: "test", Args: []string{"sh", "-c", "sleep 1.2; echo completed"}}
	req := queueRunRequest(t, ctx, c, server.URL, spec)
	result := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(req)
		if err == nil {
			defer response.Body.Close()
			var output []byte
			output, err = io.ReadAll(response.Body)
			if err == nil && (response.StatusCode != http.StatusOK || !strings.Contains(string(output), "completed")) {
				err = fmt.Errorf("HTTP %d %s", response.StatusCode, output)
			}
		}
		result <- err
	}()
	waitForQueueTest(t, "command to enter the queue", func() bool {
		_, n := queueDepth(queue, runner)
		return n == 1
	})
	time.Sleep(1200 * time.Millisecond)
	hold()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("queue time consumed the command budget: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("queued command did not finish")
	}
}
