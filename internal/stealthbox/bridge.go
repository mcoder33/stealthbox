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
	"syscall"
	"time"
)

type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("command exited with status %d", e.Code) }

type RunRequest struct {
	Project string   `json:"project"`
	Args    []string `json:"args"`
	Sync    bool     `json:"sync"`
	CWD     string   `json:"cwd,omitempty"`
}
type Event struct {
	Stream string `json:"stream"`
	Data   string `json:"data,omitempty"`
	Code   int    `json:"code,omitempty"`
}
type eventWriter struct {
	mu     *sync.Mutex
	enc    *json.Encoder
	flush  http.Flusher
	stream string
}

func (w eventWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.enc.Encode(Event{Stream: w.stream, Data: string(p)})
	if w.flush != nil {
		w.flush.Flush()
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
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
	locks := map[string]*sync.Mutex{}
	for n := range c.Projects {
		locks[n] = &sync.Mutex{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(c.Bridge.Token) < 32 || subtle.ConstantTimeCompare([]byte(supplied), []byte(c.Bridge.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.URL.Path == "/health" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "allow_exec": c.Bridge.AllowExec, "config_hash": configHash(c)})
			return
		}
		if r.URL.Path == "/file" && r.Method == "GET" {
			serveFile(w, r, c, locks)
			return
		}
		if r.URL.Path != "/run" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Stealthbox-Request"))
		if err != nil || len(raw) > 16384 {
			http.Error(w, "invalid request", 400)
			return
		}
		var req RunRequest
		if err = json.Unmarshal(raw, &req); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		p, ok := c.Projects[req.Project]
		if !ok {
			http.Error(w, "project is not enabled on this Mac", 403)
			return
		}
		dst, ok := p.Runners["mac"]
		if !ok || dst.Host != "" || dst.Bridge {
			http.Error(w, "Mac runner is not local", 400)
			return
		}
		if len(req.Args) == 0 || len(req.Args) > 256 {
			http.Error(w, "missing or excessive command arguments", 400)
			return
		}
		for _, a := range req.Args {
			if strings.ContainsRune(a, 0) {
				http.Error(w, "argument contains NUL", 400)
				return
			}
		}
		if !req.Sync && !c.Bridge.AllowExec {
			http.Error(w, "direct Mac execution is disabled; enable allow_exec locally", 403)
			return
		}
		lock := locks[req.Project]
		if !lock.TryLock() {
			http.Error(w, "another command is using this runner", 409)
			return
		}
		defer lock.Unlock()
		max, timeout := limits(c)
		if req.Sync {
			if err = SyncSnapshot(dst.Path, http.MaxBytesReader(w, r.Body, max+(64<<20)), max); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		if !req.Sync {
			if _, e := os.Stat(dst.Path); os.IsNotExist(e) {
				if e = validateRunnerRoot(dst.Path); e != nil {
					http.Error(w, e.Error(), 400)
					return
				}
			}
		}
		root, err := filepath.EvalSymlinks(dst.Path)
		if err != nil {
			http.Error(w, "runner directory is unavailable", 400)
			return
		}
		cwd := root
		if req.CWD != "" {
			if !filepath.IsLocal(req.CWD) {
				http.Error(w, "cwd must stay inside runner directory", 400)
				return
			}
			cwd, err = filepath.EvalSymlinks(filepath.Join(root, req.CWD))
			if err != nil || !within(root, cwd) {
				http.Error(w, "cwd is outside runner directory or unavailable", 400)
				return
			}
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		enc := json.NewEncoder(w)
		mu := &sync.Mutex{}
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, req.Args[0], req.Args[1:]...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "STEALTHBOX_PROJECT="+req.Project, "STEALTHBOX_RUNNER=mac")
		cmd.Stdout = eventWriter{mu, enc, flusher, "stdout"}
		cmd.Stderr = eventWriter{mu, enc, flusher, "stderr"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cmd.WaitDelay = 2 * time.Second
		code := 0
		if err = cmd.Run(); err != nil {
			code = 1
			var ex *exec.ExitError
			if errors.As(err, &ex) {
				code = ex.ExitCode()
				if code < 0 {
					code = 130
				}
			}
			mu.Lock()
			_ = enc.Encode(Event{Stream: "stderr", Data: err.Error() + "\n"})
			mu.Unlock()
		}
		mu.Lock()
		_ = enc.Encode(Event{Stream: "exit", Code: code})
		if flusher != nil {
			flusher.Flush()
		}
		mu.Unlock()
	})
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
	managed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shutdown" && r.Method == "POST" && stop != nil {
			if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(c.Bridge.Token)) != 1 {
				http.Error(w, "unauthorized", 401)
				return
			}
			w.WriteHeader(200)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			stop()
			return
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
	client := bridgeHTTP(c.Bridge.Socket)
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://unix/health", nil)
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
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
	p, ok := c.Projects[name]
	if !ok {
		return fmt.Errorf("unknown project")
	}
	if snapshot && p.Source.Host != "" {
		return fmt.Errorf("run the bridge client on the source VM")
	}
	max, _ := limits(c)
	var body io.Reader
	var archive *os.File
	if snapshot {
		var err error
		archive, err = os.CreateTemp("", "stealthbox-snapshot-*.tar")
		if err != nil {
			return err
		}
		defer os.Remove(archive.Name())
		defer archive.Close()
		if err = Snapshot(p.Source.Path, archive, max); err != nil {
			return err
		}
		if _, err = archive.Seek(0, 0); err != nil {
			return err
		}
		body = archive
	}
	meta, err := json.Marshal(RunRequest{Project: name, Args: args, Sync: snapshot, CWD: cwd})
	if err != nil {
		return err
	}
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
	res, err := bridgeHTTP(c.Bridge.Socket).Do(req)
	if err != nil {
		return fmt.Errorf("Mac is disconnected or unavailable: %w", err)
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
			return fmt.Errorf("Mac command stream ended without a result; do not retry automatically: %w", err)
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
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h)
}
