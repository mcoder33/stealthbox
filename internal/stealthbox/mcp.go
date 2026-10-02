package stealthbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type cappedBuffer struct {
	strings.Builder
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.Len()
	if left < 0 {
		left = 0
	}
	if len(p) > left {
		p = p[:left]
		b.truncated = true
	}
	_, _ = b.Builder.Write(p)
	return n, nil
}
func (b *cappedBuffer) Text() string {
	s := b.String()
	if b.truncated {
		s += "\n[output truncated; rerun with a narrower command or collect an artifact]"
	}
	return s
}
func ServeMCP(ctx context.Context, c Config, in io.Reader, out io.Writer) error {
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 4096), 2<<20)
	enc := json.NewEncoder(out)
	for scan.Scan() {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "invalid JSON"}})
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		var result any
		var rpcErr any
		switch req.Method {
		case "initialize":
			var p struct {
				Protocol string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			protocol := "2024-11-05"
			if p.Protocol == "2025-03-26" || p.Protocol == "2025-06-18" {
				protocol = p.Protocol
			}
			result = map[string]any{"protocolVersion": protocol, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "stealthbox", "version": "0.3.0"}, "instructions": mcpGuide(c)}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			schema := map[string]any{"type": "object", "properties": map[string]any{"argv": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "minItems": 1}, "project": map[string]string{"type": "string", "description": "Legacy configured project name"}, "path": map[string]string{"type": "string", "description": "Workspace checkout path, absolute under VMRoot or relative to VMRoot; required per workspace call"}, "runner": map[string]any{"type": "string", "enum": []string{"vm", "local", "mac"}}, "cwd": map[string]string{"type": "string", "description": "Command directory relative to the selected checkout; selecting a subdirectory path syncs the whole checkout"}}, "required": []string{"argv"}, "additionalProperties": false}
			tools := []map[string]any{{"name": "runner_run", "description": "Run Docker/test commands on the selected VM or local runner. Local runs synchronize uncommitted checkout files using the configured transport, preserve dependency caches, and return stdout/stderr/exit status. Do not retry automatically after disconnection.", "inputSchema": schema}, {"name": "vm_exec", "description": "Execute an explicit command in the selected source checkout on the VM without synchronization.", "inputSchema": schema}}
			if workspaceMCPScope(c) {
				tools = append(tools, map[string]any{"name": "projects_list", "description": "Discover Git checkouts and worktrees under the configured VM workspace root. Each execution must supply its own path; repeated basenames are not identities.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}})
			}
			if c.Bridge.Enabled && c.Bridge.AllowExec {
				for _, name := range []string{"local_exec", "mac_exec"} {
					tools = append(tools, map[string]any{"name": name, "description": "Execute an explicit command in the connected local runner without synchronizing files. Requires local allow_exec. Runs with the local user's permissions; not a security sandbox.", "inputSchema": schema})
				}
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Argv    []string `json:"argv"`
					Project string   `json:"project"`
					Path    string   `json:"path"`
					Runner  string   `json:"runner"`
					CWD     string   `json:"cwd"`
				} `json:"arguments"`
			}
			err := json.Unmarshal(req.Params, &p)
			stdout := &cappedBuffer{limit: 1 << 20}
			stderr := &cappedBuffer{limit: 1 << 20}
			if err == nil {
				if p.Name == "projects_list" {
					if !workspaceMCPScope(c) {
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
					selector := p.Arguments.Project
					if p.Arguments.Path != "" {
						if selector != "" {
							err = fmt.Errorf("choose either project or path, not both")
						} else {
							selector, err = WorkspaceCheckoutPath(c, p.Arguments.Path)
						}
					}
					if workspaceMCPScope(c) {
						if selector == "" {
							err = fmt.Errorf("workspace tools require a checkout path on every call")
						}
					} else {
						bound := os.Getenv("STEALTHBOX_PROJECT")
						if selector == "" {
							selector = bound
						}
						if p.Arguments.Path != "" || (bound != "" && selector != bound) {
							err = fmt.Errorf("this agent's tools are bound to project %s", bound)
						}
					}
					runner := p.Arguments.Runner
					if runner == "" {
						runner = envOr("STEALTHBOX_RUNNER", "vm")
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
						callCtx, cancel := context.WithTimeout(ctx, timeout)
						err = Run(callCtx, c, selector, runner, p.Arguments.Argv, snapshot, p.Arguments.CWD, stdout, stderr)
						cancel()
					}
				}
			}

			text := stdout.Text()
			if stderr.Len() > 0 {
				text += "\n[stderr]\n" + stderr.Text()
			}
			if err != nil {
				text += "\n[error] " + err.Error()
			} else {
				text += "\n[exit status] 0"
			}
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": err != nil}
		default:
			rpcErr = map[string]any{"code": -32601, "message": "method not found"}
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scan.Err()
}
func envOr(k, d string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return d
}

func workspaceMCPScope(c Config) bool {
	return WorkspaceRootEnabled(c) && os.Getenv("STEALTHBOX_SCOPE") == "workspace"
}

func mcpGuide(c Config) string {
	project := os.Getenv("STEALTHBOX_PROJECT")
	if workspaceMCPScope(c) {
		project = ""
	}
	return Guide(project, envOr("STEALTHBOX_RUNNER", "vm"))
}
