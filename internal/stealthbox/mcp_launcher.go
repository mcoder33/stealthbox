package stealthbox

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	managedMCPTokenEnv = "STEALTHBOX_MCP_BEARER_TOKEN"
	mcpStartupTimeout  = 15 * time.Second
)

type managedMCPState struct {
	Generation string
	Port       int
	Key        []byte
	PID        int
}

func (s managedMCPState) url() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.Port)
}

func (s managedMCPState) ready(ctx context.Context) bool {
	probe, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(probe, http.MethodGet, s.url()+"/health", nil)
	if err != nil {
		return false
	}
	challenge := rand.Text()
	request.Header.Set(mcpHTTPChallengeHeader, challenge)
	client := managedMCPControlClient()
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	proof := response.Header.Get(mcpHTTPProofHeader)
	return response.StatusCode == http.StatusNoContent && hmac.Equal([]byte(proof), []byte(mcpHTTPHealthProof(s.Key, challenge)))
}

func (s managedMCPState) retire(ctx context.Context) error {
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probe, http.MethodPost, s.url()+mcpHTTPRetirePath, nil)
	if err != nil {
		return err
	}
	token := signMCPHTTP(s.Key, mcpRetireDomain, []byte(rand.Text()))
	request.Header.Set("Authorization", "Bearer "+token)
	client := managedMCPControlClient()
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request idle MCP service replacement: %w", err)
	}
	response.Body.Close()
	switch response.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("MCP service is busy with active tool calls; wait for them to finish and retry")
	default:
		return fmt.Errorf("MCP service could not retire safely: HTTP %d", response.StatusCode)
	}
}

func managedMCPControlClient() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func managedMCPPaths(configPath string) (config, state string, err error) {
	config, err = filepath.Abs(configPath)
	if err != nil {
		return "", "", err
	}
	config, err = filepath.EvalSymlinks(config)
	if err != nil {
		return "", "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", "", err
	}
	profile := sha256.Sum256([]byte(config))
	return config, filepath.Join(cache, "stealthbox", "mcp", fmt.Sprintf("%x", profile), "endpoint.json"), nil
}

func managedMCPGeneration(c Config, executable string) (string, error) {
	if err := NormalizeLocalWorkspaceRoots(&c); err != nil {
		return "", err
	}
	info, err := os.Stat(executable)
	if err != nil {
		return "", err
	}
	// Changes to launch defaults should not disconnect clients. Their effective
	// runner is part of the signed MCP scope.
	c.Workspace.Runner = ""
	c.Agents = nil
	c.Projects = maps.Clone(c.Projects)
	for name, project := range c.Projects {
		project.Agent, project.Runner = "", ""
		c.Projects[name] = project
	}
	if !c.Bridge.Enabled {
		// Load supplies a temporary token for configurations without a Mac bridge.
		c.Bridge.Token = ""
	}
	return fmt.Sprintf("%s-%s-%d-%d", configHash(c), BuildVersion, info.Size(), info.ModTime().UnixNano()), nil
}

func readManagedMCPState(path string) (managedMCPState, error) {
	var state managedMCPState
	info, err := os.Lstat(path)
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4096 {
		return state, fmt.Errorf("invalid private MCP service state")
	}
	contents, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(contents, &state)
	}
	if err == nil && (state.Port < 1 || state.Port > 65535 || len(state.Key) != 32) {
		err = fmt.Errorf("invalid MCP service state")
	}
	return state, err
}

func lockManagedMCPDirectory(ctx context.Context, path string) (*os.File, error) {
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err == nil && (stat.Mode&0777 != 0700 || stat.Uid != uint32(os.Geteuid())) {
		err = fmt.Errorf("MCP service directory must be owned by the current user and have mode 0700")
	}
	if err == nil {
		err = lockManagedMCPFile(ctx, dir)
	}
	if err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func lockManagedMCPFile(ctx context.Context, file *os.File) error {
	for ctx.Err() == nil {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return ctx.Err()
}

func acquireManagedMCPDaemon(ctx context.Context, statePath, generation string) (*os.File, error) {
	state, err := readManagedMCPState(statePath)
	if err == nil && state.Generation != generation && state.ready(ctx) {
		if err := state.retire(ctx); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(filepath.Dir(statePath), "service.lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	lease := os.NewFile(uintptr(fd), path)
	if err := lockManagedMCPFile(ctx, lease); err != nil {
		lease.Close()
		return nil, fmt.Errorf("existing MCP service is not ready to reuse or replace: %w", err)
	}
	return lease, nil
}

func ensureManagedMCP(parent context.Context, c Config, configPath string, scope MCPScope) (url, token string, err error) {
	if !scope.validHTTPBinding() {
		return "", "", fmt.Errorf("MCP client needs a project or workspace binding and runner")
	}
	ctx, cancel := context.WithTimeout(parent, mcpStartupTimeout)
	defer cancel()
	state, err := managedMCPInstance(ctx, c, configPath)
	if err != nil {
		return "", "", err
	}
	bearer, err := MCPHTTPBearer(state.Key, scope)
	return state.url() + mcpHTTPPath, bearer, err
}

func managedMCPInstance(ctx context.Context, c Config, configPath string) (managedMCPState, error) {
	config, statePath, err := managedMCPPaths(configPath)
	if err != nil {
		return managedMCPState{}, err
	}
	bin, err := os.Executable()
	if err != nil {
		return managedMCPState{}, err
	}
	generation, err := managedMCPGeneration(c, bin)
	if err != nil {
		return managedMCPState{}, err
	}
	directory, err := lockManagedMCPDirectory(ctx, filepath.Dir(statePath))
	if err != nil {
		return managedMCPState{}, err
	}
	defer directory.Close()
	current, err := Load(config)
	if err != nil {
		return managedMCPState{}, err
	}
	if err := verifyManagedMCPGeneration(current, generation); err != nil {
		return managedMCPState{}, err
	}
	if state, ready := readyManagedMCPInstance(ctx, statePath, generation); ready {
		return state, nil
	}
	return startManagedMCPInstance(ctx, bin, config, statePath, generation, directory)
}

func readyManagedMCPInstance(ctx context.Context, statePath, generation string) (managedMCPState, bool) {
	state, err := readManagedMCPState(statePath)
	return state, err == nil && state.Generation == generation && state.ready(ctx)
}

func startManagedMCPInstance(ctx context.Context, bin, config, statePath, generation string, directory *os.File) (managedMCPState, error) {
	lease, err := acquireManagedMCPDaemon(ctx, statePath, generation)
	if err != nil {
		return managedMCPState{}, err
	}
	defer lease.Close()
	startupLog, err := os.CreateTemp(filepath.Dir(statePath), ".mcp-start-*")
	if err != nil {
		return managedMCPState{}, err
	}
	defer startupLog.Close()
	if err := os.Remove(startupLog.Name()); err != nil {
		return managedMCPState{}, err
	}
	// The daemon inherits both locks. If the launcher dies during startup,
	// the daemon can still finish publishing the only endpoint for this profile.
	cmd := exec.Command(bin, "mcp-service", "--config", config, "--", generation)
	cmd.Dir = filepath.Dir(config)
	cmd.Stderr = startupLog
	cmd.Env = append(sharedMCPServiceEnv(os.Environ()), "PATH="+managedPath(), "STEALTHBOX_CONFIG="+config)
	cmd.ExtraFiles = []*os.File{directory, lease}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return managedMCPState{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		if state, ready := readyManagedMCPInstance(ctx, statePath, generation); ready {
			return state, nil
		}
		select {
		case err := <-done:
			detail, _ := io.ReadAll(io.NewSectionReader(startupLog, 0, 4096))
			return managedMCPState{}, fmt.Errorf("MCP service exited during startup: %v (%s)", err, strings.TrimSpace(string(detail)))
		case <-ctx.Done():
			return managedMCPState{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// ManagedMCPService is the internal child entry point. The startup lock on fd 3
// is held until publication; fd 4 guards the complete service lifetime.
func ManagedMCPService(ctx context.Context, c Config, configPath, generation string) error {
	_, statePath, err := managedMCPPaths(configPath)
	if err != nil {
		return err
	}
	if err := verifyManagedMCPGeneration(c, generation); err != nil {
		return err
	}
	startupLock, err := inheritedManagedMCPLock(3, filepath.Dir(statePath))
	if err != nil {
		return err
	}
	defer startupLock.Close()
	serviceLock, err := inheritedManagedMCPLock(4, filepath.Join(filepath.Dir(statePath), "service.lock"))
	if err != nil {
		return err
	}
	defer serviceLock.Close()
	return serveManagedMCP(ctx, c, statePath, generation, startupLock)
}

func serveManagedMCP(ctx context.Context, c Config, statePath, generation string, startupLock *os.File) error {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	defer listener.Close()
	state, err := publishManagedMCPState(statePath, generation, listener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		return err
	}
	defer os.Remove(statePath)
	serving, cancel := context.WithCancel(ctx)
	defer cancel()
	go monitorManagedMCPState(serving, statePath, state)
	return ServeMCPHTTP(serving, c, &managedMCPStartupListener{TCPListener: listener, lock: startupLock}, state.Key)
}

// Keep the startup lock until net/http enters its accept loop, even when the
// original launcher stops waiting before the daemon is ready.
type managedMCPStartupListener struct {
	*net.TCPListener
	lock *os.File
}

func (l *managedMCPStartupListener) Accept() (net.Conn, error) {
	if l.lock != nil {
		l.lock.Close()
		l.lock = nil
	}
	return l.TCPListener.Accept()
}

func verifyManagedMCPGeneration(c Config, generation string) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	current, err := managedMCPGeneration(c, bin)
	if err != nil || current != generation {
		return fmt.Errorf("MCP configuration changed during startup; retry the agent launch")
	}
	return nil
}

func inheritedManagedMCPLock(fd uintptr, path string) (*os.File, error) {
	lock := os.NewFile(fd, "MCP service lock")
	info, err := lock.Stat()
	expected, pathErr := os.Stat(path)
	if err != nil || pathErr != nil || !os.SameFile(expected, info) {
		lock.Close()
		return nil, fmt.Errorf("MCP service needs its launcher lock")
	}
	// The daemon may start other subprocesses while the lock is still held.
	unix.CloseOnExec(int(fd))
	return lock, nil
}

func publishManagedMCPState(path, generation string, port int) (managedMCPState, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return managedMCPState{}, err
	}
	state := managedMCPState{Generation: generation, Port: port, Key: key, PID: os.Getpid()}
	data, err := json.Marshal(state)
	if err != nil {
		return managedMCPState{}, err
	}
	return state, writePrivate(path, data)
}

func monitorManagedMCPState(ctx context.Context, path string, published managedMCPState) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			state, err := readManagedMCPState(path)
			if err == nil && state.PID == published.PID && state.Port == published.Port && hmac.Equal(state.Key, published.Key) {
				continue
			}
			if published.ready(ctx) && published.retire(ctx) == nil {
				return
			}
		}
	}
}

func sharedMCPServiceEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case managedMCPTokenEnv, "STEALTHBOX_PROJECT", "STEALTHBOX_RUNNER", "STEALTHBOX_RUNNER_SOURCE", "STEALTHBOX_SCOPE", "STEALTHBOX_AGENT", "STEALTHBOX_VM_ROOT":
		default:
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
