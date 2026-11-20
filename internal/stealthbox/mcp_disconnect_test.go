package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPMacDisconnect(t *testing.T) {
	for _, stage := range []string{"offline", "silent", "after-submit", "stalled-after-submit"} {
		t.Run(stage, func(t *testing.T) {
			c := bridgeConfig(t)
			c.Bridge.AllowExec = true
			root, err := os.MkdirTemp("/tmp", "sb-mcp-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			c.Bridge.Socket = filepath.Join(root, "m.sock")
			c.Projects["test"].Runners["mac"] = Endpoint{Bridge: true, Path: c.Projects["test"].Source.Path}
			var calls atomic.Int32
			if stage != "offline" {
				ln, err := net.Listen("unix", c.Bridge.Socket)
				if err != nil {
					t.Fatal(err)
				}
				srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/health" {
						if stage == "silent" || (stage == "stalled-after-submit" && calls.Load() > 0) {
							<-r.Context().Done()
							return
						}
						w.WriteHeader(200)
						return
					}
					calls.Add(1)
					if stage == "stalled-after-submit" {
						w.WriteHeader(200)
						w.(http.Flusher).Flush()
						<-r.Context().Done()
						return
					}
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
				})}
				go srv.Serve(ln)
				defer srv.Close()
			}
			input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mac_exec","arguments":{"project":"test","argv":["true"]}}}
{"jsonrpc":"2.0","id":2,"method":"ping"}`)
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			var out bytes.Buffer
			if err := ServeMCP(ctx, c, input, &out); err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(&out)
			var reply struct {
				Result struct {
					IsError bool
					Content []struct{ Text string }
				}
			}
			if err := dec.Decode(&reply); err != nil {
				t.Fatal(err)
			}
			state := "execution=not_started"
			var wantCalls int32
			if strings.HasSuffix(stage, "after-submit") {
				state = "execution=unknown"
				wantCalls = 1
			}
			if ctx.Err() != nil || !reply.Result.IsError || len(reply.Result.Content) != 1 || !strings.Contains(reply.Result.Content[0].Text, state) || calls.Load() != wantCalls {
				t.Fatalf("response=%+v calls=%d timeout=%v", reply, calls.Load(), ctx.Err())
			}
			var ping struct{ ID int }
			if err := dec.Decode(&ping); err != nil || ping.ID != 2 {
				t.Fatal("MCP stopped responding", err)
			}
		})
	}
}
