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
			result = map[string]any{"protocolVersion": protocol, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "stealthbox", "version": "0.2.0"}, "instructions": Guide(os.Getenv("STEALTHBOX_PROJECT"), envOr("STEALTHBOX_RUNNER", "vm"))}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			schema := map[string]any{"type": "object", "properties": map[string]any{"argv": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "minItems": 1}, "project": map[string]string{"type": "string"}, "runner": map[string]any{"type": "string", "enum": []string{"vm", "mac"}}, "cwd": map[string]string{"type": "string", "description": "Relative directory within the Mac runner; mac_exec only"}}, "required": []string{"argv"}, "additionalProperties": false}
			tools := []map[string]any{{"name": "runner_run", "description": "Run Docker/test commands on the selected runner (default STEALTHBOX_RUNNER). Mac runs sync uncommitted VM code into a disposable Mac checkout first. Return stdout, stderr and exit status. Do not retry automatically after disconnection.", "inputSchema": schema}, {"name": "vm_exec", "description": "Execute an explicit command on the source VM without synchronizing code.", "inputSchema": schema}}
			if c.Bridge.Enabled && c.Bridge.AllowExec {
				tools = append(tools, map[string]any{"name": "mac_exec", "description": "Execute an explicit command on the connected Mac without replacing files. Requires allow_exec enabled locally. Runs as the Mac user; not a security sandbox. cwd is relative to its project runner.", "inputSchema": schema})
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Argv    []string `json:"argv"`
					Project string   `json:"project"`
					Runner  string   `json:"runner"`
					CWD     string   `json:"cwd"`
				} `json:"arguments"`
			}
			err := json.Unmarshal(req.Params, &p)
			stdout := &cappedBuffer{limit: 1 << 20}
			stderr := &cappedBuffer{limit: 1 << 20}
			if err == nil {
				project := p.Arguments.Project
				if project == "" {
					project = os.Getenv("STEALTHBOX_PROJECT")
				}
				if bound := os.Getenv("STEALTHBOX_PROJECT"); bound != "" && project != bound {
					err = fmt.Errorf("this agent's tools are bound to project %s", bound)
				}
				runner := p.Arguments.Runner
				if runner == "" {
					runner = os.Getenv("STEALTHBOX_RUNNER")
				}
				if runner == "" {
					runner = "vm"
				}
				snapshot := true
				switch p.Name {
				case "runner_run":
				case "vm_exec":
					runner = "vm"
					snapshot = false
				case "mac_exec":
					runner = "mac"
					snapshot = false
					if !c.Bridge.Enabled || !c.Bridge.AllowExec {
						err = fmt.Errorf("direct Mac execution is disabled")
					}
				default:
					err = fmt.Errorf("unknown tool")
				}
				if err == nil {
					_, timeout := limits(c)
					callCtx, cancel := context.WithTimeout(ctx, timeout)
					err = Run(callCtx, c, project, runner, p.Arguments.Argv, snapshot, p.Arguments.CWD, stdout, stderr)
					cancel()
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
