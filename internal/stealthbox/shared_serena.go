package stealthbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SerenaManifest is captured from the VM installation once at setup time.
// MCP discovery in each new chat only reads these native tool schemas.
type SerenaManifest struct {
	Context string            `json:"context,omitempty"`
	Command string            `json:"command"`
	Tools   []json.RawMessage `json:"tools"`
}

func worktreeTool(name string) bool {
	switch name {
	case "initial_instructions", "onboarding", "check_onboarding_performed", "get_current_config",
		"find_symbol", "find_referencing_symbols", "get_symbols_overview", "find_implementations",
		"find_declaration", "get_diagnostics_for_file", "get_diagnostics_for_symbol",
		"safe_delete_symbol", "search_for_pattern", "replace_in_files",
		"rename_symbol", "replace_symbol_body", "insert_before_symbol", "insert_after_symbol", "replace_content",
		"read_memory", "list_memories", "write_memory", "edit_memory", "delete_memory", "rename_memory":
		return true
	}
	return false
}

func (m *SerenaManifest) catalog(workspace bool) (map[string]json.RawMessage, error) {
	if !filepath.IsAbs(m.Command) || len(m.Tools) == 0 {
		return nil, errors.New("shared Serena requires an absolute VM executable and its tool catalog")
	}
	tools := make(map[string]json.RawMessage, len(m.Tools))
	for _, raw := range m.Tools {
		name, schema, err := serenaToolSchema(raw, workspace)
		if err != nil {
			return nil, err
		}
		if tools[name] != nil {
			return nil, fmt.Errorf("duplicate shared Serena tool %s", name)
		}
		tools[name] = schema
	}
	return tools, nil
}
func serenaToolSchema(raw json.RawMessage, workspace bool) (string, json.RawMessage, error) {
	var tool map[string]any
	if err := json.Unmarshal(raw, &tool); err != nil {
		return "", nil, err
	}
	name, _ := tool["name"].(string)
	schema, _ := tool["inputSchema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	if !worktreeTool(name) || schema["type"] != "object" || properties == nil {
		return "", nil, errors.New("invalid shared Serena tool catalog")
	}
	if _, exists := properties["worktree"]; exists {
		return "", nil, errors.New("Serena schema already defines worktree")
	}
	if workspace {
		properties["worktree"] = map[string]string{"type": "string", "description": "VM checkout path under the workspace root; pass the same path for all operations on this worktree."}
		required, _ := schema["required"].([]any)
		schema["required"] = append(required, "worktree")
		raw, _ = json.Marshal(tool)
	}
	return name, raw, nil
}

func (m *SerenaManifest) validate() error {
	_, err := m.catalog(false)
	return err
}

// Close the complete process group exactly once when the SDK tears down its
// connection, including failed initialization and an unexpected server exit.
type serenaTransport struct {
	command    mcp.CommandTransport
	connection *serenaConnection
}
type serenaConnection struct {
	mcp.Connection
	finish func() error
}

func (c *serenaConnection) Close() error { return c.finish() }
func (t *serenaTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.command.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pid := t.command.Command.Process.Pid
	t.connection = &serenaConnection{Connection: conn, finish: sync.OnceValue(func() error {
		err := conn.Close()
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return err
	})}
	return t.connection, nil
}

func openSerena(ctx context.Context, executable, cwd, toolContext string, pinned bool) (*mcp.ClientSession, error) {
	if toolContext == "" {
		toolContext = "claude-code"
	}
	args := []string{"start-mcp-server", "--context", toolContext, "--enable-web-dashboard", "false", "--open-web-dashboard", "false"}
	if pinned {
		args = append(args, "--project", cwd)
	}
	cmd := exec.Command(executable, args...)
	cmd.Dir = cwd
	cmd.Env = sharedMCPServiceEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	client := mcp.NewClient(&mcp.Implementation{Name: "stealthbox", Version: BuildVersion}, nil)
	transport := &serenaTransport{command: mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		if transport.connection != nil {
			_ = transport.connection.Close()
		}
		return nil, err
	}
	return session, nil
}

func DiscoverSerena(ctx context.Context, executable, toolContext string) (*SerenaManifest, error) {
	if !filepath.IsAbs(executable) {
		return nil, errors.New("provide an absolute path to the VM Serena executable")
	}
	dir, err := os.MkdirTemp("", "stealthbox-serena-discovery-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	worker, err := openSerena(ctx, executable, dir, toolContext, false)
	if err != nil {
		return nil, err
	}
	defer worker.Close()
	m := &SerenaManifest{Command: executable, Context: toolContext}
	for tool, err := range worker.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		if worktreeTool(tool.Name) {
			raw, err := json.Marshal(tool)
			if err != nil {
				return nil, err
			}
			m.Tools = append(m.Tools, raw)
		}
	}
	return m, m.validate()
}

const maxSerenaWorktrees = 8
const serenaIdleTimeout = 5 * time.Minute

// The gate serializes native Serena calls and process startup within one root.
// refs includes callers waiting for the gate, so idle expiry cannot stop their worker.
type serenaEntry struct {
	gate       chan struct{}
	refs       int
	generation uint64
	timer      *time.Timer
	stopping   chan struct{}
	worker     *mcp.ClientSession
}
type serenaPool struct {
	starting    chan struct{}
	allowed     map[string]json.RawMessage
	inventories map[bool][]json.RawMessage
	mu          sync.Mutex
	entries     map[string]*serenaEntry
	closed      bool
	idle        time.Duration
}

func newSerenaPool(manifest *SerenaManifest, idle time.Duration) (*serenaPool, error) {
	named, err := manifest.catalog(false)
	if err != nil {
		return nil, err
	}
	workspace, err := manifest.catalog(true)
	if err != nil {
		return nil, err
	}
	inventories := map[bool][]json.RawMessage{}
	for _, name := range slices.Sorted(maps.Keys(named)) {
		inventories[false] = append(inventories[false], named[name])
		inventories[true] = append(inventories[true], workspace[name])
	}
	return &serenaPool{starting: make(chan struct{}, 1), entries: map[string]*serenaEntry{}, idle: idle, allowed: named, inventories: inventories}, nil
}
func (p *serenaPool) inventory(workspace bool) any {
	return map[string]any{"tools": p.inventories[workspace]}
}
func (p *serenaPool) acquire(ctx context.Context, root string) (*serenaEntry, error) {
	for {
		e, stopped, err := p.reserve(root)
		if err != nil {
			return nil, err
		}
		if stopped != nil {
			select {
			case <-stopped:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		select {
		case e.gate <- struct{}{}:
			return e, nil
		case <-ctx.Done():
			p.release(root, e)
			return nil, ctx.Err()
		}
	}
}
func (p *serenaPool) reserve(root string) (*serenaEntry, <-chan struct{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, errors.New("Serena service is stopping")
	}
	e := p.entries[root]
	if e == nil {
		if len(p.entries) >= maxSerenaWorktrees {
			return nil, nil, errors.New("shared Serena worktree capacity exhausted")
		}
		e = &serenaEntry{gate: make(chan struct{}, 1)}
		p.entries[root] = e
	}
	if e.stopping != nil {
		return nil, e.stopping, nil
	}
	e.refs++
	if e.timer != nil {
		e.timer.Stop()
	}
	return e, nil, nil
}

func (p *serenaPool) release(root string, e *serenaEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.refs--
	if e.refs == 0 && !p.closed {
		if e.worker == nil {
			delete(p.entries, root)
			return
		}
		e.generation++
		generation := e.generation
		e.timer = time.AfterFunc(p.idle, func() { p.expire(root, e, generation) })
	}
}
func (p *serenaPool) expire(root string, e *serenaEntry, generation uint64) {
	p.mu.Lock()
	if p.closed || p.entries[root] != e || e.refs != 0 || e.generation != generation {
		p.mu.Unlock()
		return
	}
	e.stopping = make(chan struct{})
	p.mu.Unlock()
	if e.worker != nil {
		e.worker.Close()
	}
	p.mu.Lock()
	delete(p.entries, root)
	close(e.stopping)
	p.mu.Unlock()
}
func (p *serenaPool) close() {
	p.mu.Lock()
	p.closed = true
	entries := make([]*serenaEntry, 0, len(p.entries))
	for _, e := range p.entries {
		if e.timer != nil {
			e.timer.Stop()
		}
		entries = append(entries, e)
	}
	p.mu.Unlock()
	for _, e := range entries {
		if e.stopping != nil {
			<-e.stopping
			continue
		}
		e.gate <- struct{}{}
		if e.worker != nil {
			e.worker.Close()
			e.worker = nil
		}
		<-e.gate
	}
}
func (p *serenaPool) call(ctx context.Context, c Config, scope MCPScope, params json.RawMessage) (*mcp.CallToolResult, error) {
	var request struct {
		Name      string
		Arguments map[string]json.RawMessage
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, err
	}
	if p.allowed[request.Name] == nil {
		return nil, fmt.Errorf("unsupported shared Serena tool %q", request.Name)
	}
	var path string
	if raw, ok := request.Arguments["worktree"]; ok {
		if err := json.Unmarshal(raw, &path); err != nil {
			return nil, err
		}
		delete(request.Arguments, "worktree")
	}
	root, err := serenaRoot(ctx, c, scope, path)
	if err != nil {
		return nil, err
	}
	return p.callInWorktree(ctx, root, c.Workspace.Serena, &mcp.CallToolParams{Name: request.Name, Arguments: request.Arguments})
}

func (p *serenaPool) callInWorktree(ctx context.Context, root string, manifest *SerenaManifest, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	e, err := p.acquire(ctx, root)
	if err != nil {
		return nil, err
	}
	defer func() { <-e.gate; p.release(root, e) }()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if e.worker == nil {
		e.worker, err = p.start(ctx, manifest, root)
		if err != nil {
			return nil, err
		}
	}
	result, err := e.worker.CallTool(ctx, params)
	if err != nil {
		e.worker.Close()
		e.worker = nil
	}
	return result, err
}

// On startup Serena may register a new checkout in its user-level YAML.
// Serialize this operation; ready workers in other checkouts remain usable.
func (p *serenaPool) start(ctx context.Context, manifest *SerenaManifest, root string) (*mcp.ClientSession, error) {
	select {
	case p.starting <- struct{}{}:
		defer func() { <-p.starting }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return openSerena(ctx, manifest.Command, root, manifest.Context, true)
}

func serenaRoot(ctx context.Context, c Config, scope MCPScope, path string) (string, error) {
	selector, err := scope.selector(c, "", path)
	if err != nil {
		return "", err
	}
	project, err := ResolveProject(ctx, c, selector, "")
	if err != nil {
		return "", err
	}
	if project.Project.Source.Host != "" {
		return "", errors.New("Serena requires a checkout local to the VM")
	}
	root, err := filepath.EvalSymlinks(project.SourcePath)
	if err != nil {
		return "", err
	}
	if marker, err := os.Stat(filepath.Join(root, ".git")); err != nil || (!marker.IsDir() && !marker.Mode().IsRegular()) {
		return "", fmt.Errorf("shared Serena requires a Git checkout or worktree root: %s", root)
	}
	return root, nil
}
