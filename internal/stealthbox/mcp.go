package stealthbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type cappedBuffer struct {
	builder   strings.Builder
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.builder.Len()
	if left < 0 {
		left = 0
	}
	if len(p) > left {
		p = p[:left]
		b.truncated = true
	}
	_, _ = b.builder.Write(p)
	return n, nil
}
func (b *cappedBuffer) Text() string {
	s := b.builder.String()
	if b.truncated {
		s += "\n[output truncated; rerun with a narrower command or collect an artifact]"
	}
	return s
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPScope is the project selection and default runner granted to an MCP client.
// Workspace clients choose their checkout on each call. Project clients are bound
// to a single configured project.
type MCPScope struct {
	Project   string `json:"project,omitempty"`
	Runner    string `json:"runner"`
	Workspace bool   `json:"workspace,omitempty"`
}

func ServeMCP(ctx context.Context, c Config, in io.Reader, out io.Writer) error {
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 4096), 2<<20)
	enc := json.NewEncoder(out)
	scope := stdioMCPScope(c)
	for scan.Scan() {
		var req mcpRequest
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			_ = enc.Encode(mcpError(nil, -32700, "invalid JSON"))
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		var protocol string
		if req.Method == "initialize" {
			protocol = mcpProtocol(req.Params, "2024-11-05")
		}
		resp := dispatchMCP(ctx, c, scope, req, protocol, 0)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scan.Err()
}

func mcpError(id json.RawMessage, code int, message string) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpRPCError{Code: code, Message: message}}
}

func mcpProtocol(params json.RawMessage, fallback string) string {
	var p struct {
		Protocol string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	switch p.Protocol {
	case "2025-03-26", "2025-06-18":
		return p.Protocol
	case "2025-11-25":
		if fallback != "2024-11-05" {
			return p.Protocol
		}
	}
	return fallback
}

func dispatchMCP(ctx context.Context, c Config, scope MCPScope, req mcpRequest, protocol string, bridgeBudget time.Duration) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "stealthbox", "version": BuildVersion},
			"instructions":    Guide(scope.Project, scope.Runner),
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = mcpTools(c, scope)
	case "tools/call":
		resp.Result = callMCPTool(ctx, c, scope, req.Params, bridgeBudget)
	default:
		return mcpError(req.ID, -32601, "method not found")
	}
	return resp
}

func mcpTools(c Config, scope MCPScope) any {
	schema := map[string]any{"type": "object", "properties": map[string]any{"argv": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "minItems": 1}, "project": map[string]string{"type": "string", "description": "Legacy configured project name"}, "path": map[string]string{"type": "string", "description": "Workspace checkout path, absolute under VMRoot or relative to VMRoot; required per workspace call"}, "runner": map[string]any{"type": "string", "enum": []string{"vm", "local", "mac"}}, "cwd": map[string]string{"type": "string", "description": "Command directory relative to the selected checkout; selecting a subdirectory path syncs the whole checkout"}}, "required": []string{"argv"}, "additionalProperties": false}
	tools := []map[string]any{{"name": "runner_run", "description": "The required tool for Docker, builds, test suites, dependency installation, full lint, and other heavy workloads. Omit runner to use the configured runner. If unavailable, report the blocker without a shell or VM fallback. Local runs synchronize uncommitted files, preserve dependency caches, and return stdout/stderr/exit status. Do not retry automatically after disconnection. " + runnerLifetimeGuide, "inputSchema": schema}, {"name": "vm_exec", "description": "Execute an explicit command in the selected source checkout on the VM without synchronization. Use runner_run for heavy workloads.", "inputSchema": schema}}
	if scope.Workspace {
		tools = append(tools, map[string]any{"name": "projects_list", "description": "Discover Git checkouts and worktrees under the configured VM workspace root. Each execution must supply its own path; repeated basenames are not identities.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}})
	}
	if c.Bridge.Enabled {
		tools = append(tools, pastedImageToolDefinition())
	}
	if c.Bridge.Enabled && c.Bridge.AllowExec {
		for _, name := range []string{"local_exec", "mac_exec"} {
			tools = append(tools, map[string]any{"name": name, "description": "Execute an explicit command in the connected local runner without synchronizing files. Requires local allow_exec. Runs with the local user's permissions; not a security sandbox.", "inputSchema": schema})
		}
	}
	return map[string]any{"tools": tools}
}

func callMCPTool(ctx context.Context, c Config, scope MCPScope, params json.RawMessage, bridgeBudget time.Duration) any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	err := json.Unmarshal(params, &p)
	if err == nil && p.Name == readPastedImageTool {
		result, err := callPastedImageTool(ctx, c, p.Arguments)
		if err != nil {
			return map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		}
		return result
	}
	var arguments struct {
		Argv    []string `json:"argv"`
		Project string   `json:"project"`
		Path    string   `json:"path"`
		Runner  string   `json:"runner"`
		CWD     string   `json:"cwd"`
	}
	if err == nil && len(p.Arguments) > 0 {
		err = json.Unmarshal(p.Arguments, &arguments)
	}
	stdout := &cappedBuffer{limit: 1 << 20}
	stderr := &cappedBuffer{limit: 1 << 20}
	if err == nil {
		if p.Name == "projects_list" {
			if !scope.Workspace {
				err = fmt.Errorf("projects_list requires workspace scope")
			} else {
				var projects []string
				projects, err = WorkspaceProjects(ctx, c)
				if err == nil {
					data, _ := json.Marshal(map[string]any{"root": c.Workspace.VMRoot, "paths": projects, "configured_projects": Names(c)})
					_, _ = stdout.Write(data)
				}
			}
		} else {
			var selector string
			selector, err = scope.selector(c, arguments.Project, arguments.Path)
			runner := arguments.Runner
			if runner == "" {
				runner = scope.Runner
			}
			snapshot := true
			switch p.Name {
			case "runner_run":
			case "vm_exec":
				runner = "vm"
				snapshot = false
			case "local_exec", "mac_exec":
				runner = "mac"
				snapshot = false
				if !c.Bridge.Enabled || !c.Bridge.AllowExec {
					err = fmt.Errorf("direct local execution is disabled")
				}
			default:
				err = fmt.Errorf("unknown tool")
			}
			if err == nil {
				_, timeout := limits(c)
				if bridgeBudget > 0 && mcpRunnerUsesBridge(c, selector, runner) {
					timeout = bridgeBudget
				}
				callCtx, cancel := context.WithTimeout(ctx, timeout)
				err = Run(callCtx, c, selector, runner, arguments.Argv, snapshot, arguments.CWD, stdout, stderr)
				cancel()
			}
		}
	}

	text := stdout.Text()
	if stderr.builder.Len() > 0 {
		text += "\n[stderr]\n" + stderr.Text()
	}
	if err != nil {
		text += "\n[error] " + err.Error()
	} else {
		text += "\n[exit status] 0"
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": err != nil}
}

func mcpRunnerUsesBridge(c Config, selector, runner string) bool {
	runner = normalizeRunner(runner)
	if project, ok := c.Projects[selector]; ok {
		return project.Runners[runner].Bridge
	}
	return runner == "mac" && WorkspaceRootEnabled(c) && c.Workspace.Host == ""
}

// selector chooses the project or checkout for an execution tool call and
// returns the first specific reason a selection is rejected.
func (scope MCPScope) selector(c Config, project, path string) (string, error) {
	if path != "" && project != "" {
		return "", fmt.Errorf("choose either project or path, not both")
	}
	if scope.Workspace {
		if path == "" && project == "" {
			return "", fmt.Errorf("workspace tools require a checkout path on every call")
		}
		if path == "" {
			return project, nil
		}
		return WorkspaceCheckoutPath(c, path)
	}
	bound := scope.Project
	if path != "" || (bound != "" && project != "" && project != bound) {
		return "", fmt.Errorf("this agent's tools are bound to project %s", bound)
	}
	if project == "" {
		return bound, nil
	}
	return project, nil
}

func stdioMCPScope(c Config) MCPScope {
	scope := MCPScope{
		Project:   os.Getenv("STEALTHBOX_PROJECT"),
		Runner:    os.Getenv("STEALTHBOX_RUNNER"),
		Workspace: WorkspaceRootEnabled(c) && os.Getenv("STEALTHBOX_SCOPE") == "workspace",
	}
	if scope.Workspace {
		scope.Project = ""
	}
	if scope.Runner == "" {
		scope.Runner = "vm"
	}
	return scope
}

func mcpGuide(c Config) string {
	scope := stdioMCPScope(c)
	return Guide(scope.Project, scope.Runner)
}
