package stealthbox

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("command exited with status %d", e.Code) }

type RunRequest struct {
	WorkspaceID string   `json:"workspace_id,omitempty"`
	Path        string   `json:"path,omitempty"`
	Transport   string   `json:"transport,omitempty"`
	Project     string   `json:"project"`
	Args        []string `json:"args"`
	Sync        bool     `json:"sync"`
	CWD         string   `json:"cwd,omitempty"`
}
type Event struct {
	Stream string `json:"stream"`
	Data   string `json:"data,omitempty"`
	Code   int    `json:"code,omitempty"`
}

// eventWriter streams command output as JSON strings. A UTF-8 sequence split
// between pipe reads is held back until complete, so it is not replaced by U+FFFD.
type eventWriter struct {
	mu      *sync.Mutex
	write   func(Event) error
	stream  string
	pending []byte
}

func (w *eventWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data := append(w.pending, p...)
	complete := completeUTF8Prefix(data)
	w.pending = append([]byte(nil), data[complete:]...)
	if complete == 0 {
		return len(p), nil
	}
	if err := w.emit(data[:complete]); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close emits a trailing sequence that never completed.
func (w *eventWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	data := w.pending
	w.pending = nil
	return w.emit(data)
}

func (w *eventWriter) emit(data []byte) error {
	return w.write(Event{Stream: w.stream, Data: string(data)})
}

// completeUTF8Prefix returns the length of b without a trailing incomplete UTF-8 sequence.
func completeUTF8Prefix(b []byte) int {
	for i := len(b) - 1; i >= 0 && i > len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}
func limits(c Config) (int64, time.Duration) {
	m := c.Bridge.MaxBytes
	if m == 0 {
		m = 1 << 30
	}
	s := c.Bridge.TimeoutSeconds
	if s == 0 {
		s = 1800
	}
	return m, time.Duration(s) * time.Second
}
func BridgeHandler(c Config) http.Handler {
	return bridgeHandler(c, &runnerQueue{waiting: make(map[string][]chan struct{})})
}

func bridgeHandler(c Config, queue *runnerQueue) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(c.Bridge.Token) < 32 || subtle.ConstantTimeCompare([]byte(supplied), []byte(c.Bridge.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /health":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "allow_exec": c.Bridge.AllowExec, "config_hash": configHash(c)})
		case "GET " + pastedImageEndpoint:
			servePastedImage(w, r)
		case "GET /file":
			serveFile(w, r, c, queue)
		case "POST /run":
			serveBridgeRun(w, r, c, queue)
		default:
			http.NotFound(w, r)
		}
	})
}

func decodeBridgeRunRequest(encoded string) (RunRequest, error) {
	var req RunRequest
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) > 16384 {
		return req, errors.New("invalid request")
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, errors.New("invalid request")
	}
	_, err = relativeCommandDirectory(req.CWD)
	return req, err
}

func validateBridgeArgs(args []string) error {
	if len(args) == 0 || len(args) > 256 {
		return errors.New("missing or excessive command arguments")
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return errors.New("argument contains NUL")
		}
	}
	return nil
}

func serveBridgeRun(w http.ResponseWriter, r *http.Request, c Config, queue *runnerQueue) {
	req, err := decodeBridgeRunRequest(r.Header.Get("X-Stealthbox-Request"))
	if err != nil {
		writeBridgeRunError(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	_, operationTimeout := limits(c)
	resolveCtx, finishResolution := context.WithTimeout(r.Context(), operationTimeout)
	resolved, err := resolveBridgeProject(resolveCtx, c, req)
	finishResolution()
	if err != nil {
		writeBridgeRunError(w, r, err.Error(), http.StatusForbidden)
		return
	}
	dst, ok := resolved.Project.Runners["mac"]
	if !ok || dst.Host != "" || dst.Bridge {
		writeBridgeRunError(w, r, "local runner is unavailable", http.StatusBadRequest)
		return
	}
	if err := validateBridgeArgs(req.Args); err != nil {
		writeBridgeRunError(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	if !req.Sync && !c.Bridge.AllowExec {
		writeBridgeRunError(w, r, "direct Mac execution is disabled; enable allow_exec locally", http.StatusForbidden)
		return
	}
	transport, err := bridgeSyncTransport(c, req)
	if err != nil {
		writeBridgeRunError(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	runQueuedBridgeCommand(w, r, c, queue, req, resolved, dst, transport)
}

func runQueuedBridgeCommand(w http.ResponseWriter, r *http.Request, c Config, queue *runnerQueue, req RunRequest, resolved ResolvedProject, dst Endpoint, transport string) {
	maxBytes, operationTimeout := limits(c)
	var archive *queuedBridgeArchive
	closeArchive := func() {
		if archive != nil {
			archive.close()
			archive = nil
		}
	}
	defer closeArchive()
	waitCtx, finishWaiting := context.WithTimeout(r.Context(), operationTimeout)
	defer finishWaiting()
	release, err := queue.acquire(waitCtx, filepath.Clean(dst.Path), func() error {
		if transport != "archive" {
			return nil
		}
		var err error
		archive, err = stageQueuedBridgeArchive(waitCtx, w, r, queue, maxBytes)
		return err
	})
	finishWaiting()
	if err != nil {
		closeArchive()
		writeRunnerQueueError(w, r, err)
		return
	}
	defer release()

	operationCtx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if err := operationCtx.Err(); err != nil {
		closeArchive()
		writeRunnerQueueError(w, r, err)
		return
	}
	syncStatus := "not-requested"
	if req.Sync {
		err := syncBridgeRunner(operationCtx, w, r, c, resolved.Project.Source, dst, transport, archive, maxBytes)
		closeArchive()
		if err != nil {
			writeBridgeRunError(w, r, err.Error(), http.StatusBadRequest)
			return
		}
		syncStatus = "completed"
	}
	cwd, err := bridgeRunnerCWD(dst.Path, resolved.RelativeCWD, req.Sync)
	if err != nil {
		writeBridgeRunError(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	streamBridgeCommand(operationCtx, cancel, w, resolved, req.Args, cwd, syncStatus, transport)
}

func bridgeRunnerCWD(path, relativeCWD string, synced bool) (string, error) {
	if !synced {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := validateRunnerRoot(path); err != nil {
				return "", err
			}
		}
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("runner directory is unavailable")
	}
	if relativeCWD == "" {
		return root, nil
	}
	if !filepath.IsLocal(relativeCWD) {
		return "", errors.New("cwd must stay inside runner directory")
	}
	cwd, err := filepath.EvalSymlinks(filepath.Join(root, relativeCWD))
	if err != nil || !within(root, cwd) {
		return "", errors.New("cwd is outside runner directory or unavailable")
	}
	return cwd, nil
}

func bridgeSyncTransport(c Config, req RunRequest) (string, error) {
	if !req.Sync {
		return "", nil
	}
	transport := req.Transport
	if transport == "" {
		transport = "archive"
	}
	if transport != "archive" && transport != "rsync" {
		return "", errors.New("unsupported runner sync transport")
	}
	if c.Workspace.SyncTransport == "rsync" && transport != "rsync" {
		return "", errors.New("this local runner requires rsync transport")
	}
	return transport, nil
}

func writeRunnerQueueError(w http.ResponseWriter, r *http.Request, err error) {
	var sizeError *http.MaxBytesError
	switch {
	case errors.Is(err, errRunnerQueueFull), errors.Is(err, errBridgeArchiveStagingFull):
		writeBridgeRunError(w, r, "execution=not_started: "+err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, context.DeadlineExceeded):
		writeBridgeRunError(w, r, "execution=not_started: runner queue wait expired", http.StatusRequestTimeout)
	case errors.Is(err, context.Canceled):
		writeBridgeRunError(w, r, "execution=not_started: runner request canceled", http.StatusRequestTimeout)
	case errors.As(err, &sizeError):
		writeBridgeRunError(w, r, "execution=not_started: queued archive exceeds limit", http.StatusRequestEntityTooLarge)
	default:
		writeBridgeRunError(w, r, "execution=not_started: "+err.Error(), http.StatusBadRequest)
	}
}

func writeBridgeRunError(w http.ResponseWriter, r *http.Request, message string, status int) {
	// An unread archive must not delay an error response while a client uploads.
	if r.ContentLength != 0 {
		w.Header().Set("Connection", "close")
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(bridgeIOIdleTimeout))
	http.Error(w, message, status)
}

func syncBridgeRunner(ctx context.Context, w http.ResponseWriter, r *http.Request, c Config, src, dst Endpoint, transport string, archive *queuedBridgeArchive, maxBytes int64) error {
	if transport == "rsync" {
		var diagnostic strings.Builder
		return PullRunner(ctx, c, src, dst.Path, &diagnostic)
	}
	if archive != nil {
		return syncSnapshot(ctx, dst.Path, archive.file, maxBytes, false)
	}
	return syncSnapshot(ctx, dst.Path, bridgeArchiveRequest(ctx, w, r, archiveRequestLimit(maxBytes)), maxBytes, true)
}

func streamBridgeCommand(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, resolved ResolvedProject, argv []string, cwd, syncStatus, transport string) {
	deadline, _ := ctx.Deadline()
	// Allow a brief final write after the command times out.
	response := newBridgeResponse(w, deadline.Add(time.Second))
	w.Header().Set("Content-Type", "application/x-ndjson")
	if err := response.flush(); err != nil {
		return
	}
	enc := json.NewEncoder(response)
	var writeErr error
	writeEvent := func(event Event) error {
		if writeErr == nil {
			writeErr = enc.Encode(event)
			if writeErr == nil {
				writeErr = response.flush()
			}
			if writeErr != nil {
				cancel()
			}
		}
		return writeErr
	}
	var mu sync.Mutex
	stdout := &eventWriter{mu: &mu, write: writeEvent, stream: "stdout"}
	stderr := &eventWriter{mu: &mu, write: writeEvent, stream: "stderr"}
	if _, err := io.WriteString(stderr, executionReceipt(resolved, "mac", cwd, syncStatus, transport)); err != nil {
		return
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "STEALTHBOX_PROJECT="+resolved.DisplayName, "STEALTHBOX_RUNNER=local")
	if resolved.StaticName == "" {
		cmd.Env = append(cmd.Env, "COMPOSE_PROJECT_NAME="+resolved.ID)
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := runNoninteractiveCommand(ctx, cmd)
	_ = stdout.Close()
	_ = stderr.Close()
	if err != nil {
		_ = writeEvent(Event{Stream: "stderr", Data: err.Error() + "\n"})
	}
	_ = writeEvent(Event{Stream: "exit", Code: bridgeCommandExitCode(err)})
}

func bridgeCommandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			return code
		}
		return 130
	}
	return 1
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
func ServeBridge(ctx context.Context, c Config, ready chan<- struct{}) error {
	return serveManagedBridge(ctx, c, ready, nil)
}
func serveManagedBridge(ctx context.Context, c Config, ready chan<- struct{}, stop context.CancelFunc) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !c.Bridge.Enabled {
		return fmt.Errorf("Mac bridge is disabled in local settings")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	socket := c.Bridge.Socket
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(socket); err == nil {
		return fmt.Errorf("socket already exists: %s (stop the other bridge; remove a stale socket manually)", socket)
	} else if !os.IsNotExist(err) {
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
	handler := BridgeHandler(c)
	var activity sync.Mutex
	activeRequests := 0
	draining := false
	managed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(c.Bridge.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if (r.URL.Path == "/shutdown" || r.URL.Path == "/shutdown-idle") && r.Method == "POST" && stop != nil {
			if r.URL.Path == "/shutdown-idle" {
				activity.Lock()
				if activeRequests > 0 {
					activity.Unlock()
					http.Error(w, "bridge is busy", http.StatusConflict)
					return
				}
				draining = true
				activity.Unlock()
			}
			w.WriteHeader(200)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			stop()
			return
		}
		if r.URL.Path == "/run" && r.Method == http.MethodPost ||
			(r.URL.Path == "/file" || r.URL.Path == pastedImageEndpoint) && r.Method == http.MethodGet {
			activity.Lock()
			if draining {
				activity.Unlock()
				http.Error(w, "bridge is refreshing; retry after reconnect", http.StatusServiceUnavailable)
				return
			}
			activeRequests++
			activity.Unlock()
			defer func() {
				activity.Lock()
				activeRequests--
				activity.Unlock()
			}()
		}
		handler.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: managed, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, finish := context.WithTimeout(context.Background(), 5*time.Second)
		defer finish()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
	}()
	if ready != nil {
		close(ready)
	}
	err = server.Serve(listener)
	cancel()
	<-done
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}
func bridgeHTTP(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, DisableKeepAlives: true}}
}
func BridgeHealth(ctx context.Context, c Config) error {
	return checkBridgeClient(ctx, c.bridgeClient(), c.Bridge.Token)
}
func checkBridgeClient(ctx context.Context, client *http.Client, token string) error {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probe, "GET", "http://unix/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Mac bridge unavailable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("Mac bridge health: HTTP %d", res.StatusCode)
	}
	return nil
}
func RunBridge(ctx context.Context, c Config, name string, args []string, snapshot bool, cwd string, stdout, stderr io.Writer) error {
	if !c.Bridge.Enabled {
		return fmt.Errorf("Mac bridge is disabled")
	}
	resolved, err := ResolveProject(ctx, c, name, cwd)
	if err != nil {
		return err
	}
	p := resolved.Project
	if snapshot && p.Source.Host != "" {
		return fmt.Errorf("run the bridge client on the source VM")
	}
	client := c.bridgeClient()
	if err := checkBridgeClient(ctx, client, c.Bridge.Token); err != nil {
		return fmt.Errorf("execution=not_started: Mac bridge check failed before sending the command. Retry when the Mac bridge is ready: %w", err)
	}
	transport := runnerSyncTransport(c, p)
	max, _ := limits(c)
	var body io.Reader
	var archive *os.File
	if snapshot && transport == "archive" {
		var err error
		archive, err = os.CreateTemp("", "stealthbox-snapshot-*.tar")
		if err != nil {
			return err
		}
		defer os.Remove(archive.Name())
		defer archive.Close()
		if err = snapshotContext(ctx, p.Source.Path, archive, max); err != nil {
			return err
		}
		if _, err = archive.Seek(0, 0); err != nil {
			return err
		}
		body = archive
	}
	request := RunRequest{Project: resolved.StaticName, Args: args, Sync: snapshot, CWD: resolved.RelativeCWD, Transport: transport}
	if resolved.StaticName == "" {
		request.WorkspaceID = c.Workspace.ID
		if request.WorkspaceID == "" {
			copyConfig := c
			_ = EnsureWorkspaceIdentity(&copyConfig)
			request.WorkspaceID = copyConfig.Workspace.ID
		}
		request.Path = resolved.RelativePath
	}
	meta, err := json.Marshal(request)
	if err != nil {
		return err
	}
	// Probe the same tunnel as this request, even if recovery publishes a new one.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := checkBridgeClient(ctx, client, c.Bridge.Token); err != nil {
					stop()
					return
				}
			}
		}
	}()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://unix/run", body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	req.Header.Set("X-Stealthbox-Request", base64.RawURLEncoding.EncodeToString(meta))
	if archive != nil {
		stat, _ := archive.Stat()
		req.ContentLength = stat.Size()
	}
	res, err := client.Do(req)
	if err != nil {
		var dial *net.OpError
		if errors.As(err, &dial) && dial.Op == "dial" {
			return fmt.Errorf("execution=not_started: Mac disconnected before sending the command. Retry after reconnect: %w", err)
		}
		return fmt.Errorf("execution=unknown: lost the Mac response. Check the command result before retrying: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("Mac runner: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	dec := json.NewDecoder(res.Body)
	for {
		var e Event
		if err = dec.Decode(&e); err != nil {
			return fmt.Errorf("execution=unknown: Mac command stream ended without a result; check its effects before retrying: %w", err)
		}
		switch e.Stream {
		case "stdout":
			if _, err = io.WriteString(stdout, e.Data); err != nil {
				return err
			}
		case "stderr":
			if _, err = io.WriteString(stderr, e.Data); err != nil {
				return err
			}
		case "exit":
			if e.Code != 0 {
				return &ExitError{e.Code}
			}
			return nil
		default:
			return fmt.Errorf("unknown bridge event")
		}
	}
}

func configHash(c Config) string {
	b, _ := json.Marshal(c)
	if transport := os.Getenv("STEALTHBOX_SSH_CONFIG"); transport != "" {
		b = append(append(b, 0), transport...)
	}
	h := sha256.Sum256(append(b, []byte("per-session-forward-v1")...))
	return fmt.Sprintf("%x", h)
}

func resolveBridgeProject(ctx context.Context, c Config, request RunRequest) (ResolvedProject, error) {
	if request.Project != "" {
		if _, ok := c.Projects[request.Project]; !ok {
			return ResolvedProject{}, fmt.Errorf("project is not enabled on this local runner")
		}
		return ResolveProject(ctx, c, request.Project, request.CWD)
	}
	if !WorkspaceRootEnabled(c) {
		return ResolvedProject{}, fmt.Errorf("workspace projects are not enabled")
	}
	if err := EnsureWorkspaceIdentity(&c); err != nil {
		return ResolvedProject{}, err
	}
	if request.WorkspaceID == "" || request.WorkspaceID != c.Workspace.ID {
		return ResolvedProject{}, fmt.Errorf("workspace ID is not enabled on this local runner")
	}
	if !filepath.IsLocal(request.Path) || request.Path == "." {
		return ResolvedProject{}, fmt.Errorf("checkout path must be relative to the workspace")
	}
	selector, err := WorkspaceCheckoutPath(c, request.Path)
	if err != nil {
		return ResolvedProject{}, err
	}
	resolved, err := ResolveProject(ctx, c, selector, request.CWD)
	if err != nil {
		return ResolvedProject{}, err
	}
	if resolved.RelativePath != filepath.ToSlash(filepath.Clean(request.Path)) {
		return ResolvedProject{}, fmt.Errorf("request must identify the canonical checkout root")
	}
	// Config with an empty source host is an archive test/local setup. Its derived
	// runner is local to this process, not the VM-side bridge client.
	if c.Workspace.Host == "" {
		endpoint := resolved.Project.Runners["mac"]
		endpoint.Bridge = false
		resolved.Project.Runners["mac"] = endpoint
	}
	return resolved, nil
}
