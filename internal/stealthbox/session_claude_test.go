package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeLiveSessionExchange(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sb-claude-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	sessions := filepath.Join(dir, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	addr := filepath.Join(dir, "native.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: addr, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	pid := os.Getpid()
	reg := map[string]any{"pid": pid, "cwd": dir, "sessionId": "synthetic-id", "name": "example", "messagingSocketPath": addr, "startedAt": 100}
	store := func(path string, value any) {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Include a stopped background row and a running session outside our projects.
	store(filepath.Join(dir, "list.json"), []any{reg, map[string]any{"sessionId": "stopped", "cwd": dir}, map[string]any{"sessionId": "foreign", "pid": 3, "cwd": "/"}})
	if err := os.WriteFile(filepath.Join(dir, "native"), []byte("#!/bin/sh\n[ \"$1 $2 $3\" = 'custom agents --json' ] && cat \"$CLAUDE_CONFIG_DIR/list.json\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := Config{Workspace: WorkspaceConfig{VMRoot: dir}, Agents: map[string][]string{"claude": {filepath.Join(dir, "native"), "custom"}}}
	file := filepath.Join(sessions, fmt.Sprintf("%d.json", pid))
	store(file, reg)
	rows, err := listClaudeSessions(context.Background(), c)
	if err != nil || len(rows) != 1 || rows[0].ID != "synthetic-id" {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	reg["sessionId"] = "another-id"
	store(file, reg)
	if _, err := askClaudeSession(context.Background(), c, "synthetic-id", "secret"); err == nil || errors.Is(err, ErrSessionOutcomeUnknown) {
		t.Fatalf("changed target: %v", err)
	}
	reg["sessionId"] = "synthetic-id"
	store(file, reg)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		var wire struct {
			From    string
			ID      string `json:"msg_id"`
			Message struct{ Content string }
		}
		if json.NewDecoder(conn).Decode(&wire) != nil || strings.Contains(wire.Message.Content, "from-mode=") || !strings.Contains(wire.Message.Content, "multi\n$HOME") {
			return
		}
		for _, body := range []string{"working on it", "FINAL wrong-request\nwrong answer", "FINAL " + wire.ID + "\nanswer <&>\nsecond line"} {
			reply, err := net.Dial("unix", strings.TrimPrefix(wire.From, "uds:"))
			if err != nil {
				return
			}
			json.NewEncoder(reply).Encode(map[string]any{"msgV": 1, "type": "user", "from": "uds:" + addr, "message": map[string]string{"role": "user", "content": "<cross-session-message from=\"uds:" + addr + "\">\n" + body + "\n</cross-session-message>"}})
			// Native SendMessage half-closes and waits for EOF from the inbox.
			// On Darwin a full close here discards the kernel peer credentials.
			reply.(*net.UnixConn).CloseWrite()
			io.Copy(io.Discard, reply)
			reply.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := askClaudeSession(ctx, c, "synthetic-id", "multi\n$HOME")
	if err != nil || got != "answer <&>\nsecond line" {
		t.Fatalf("ask = %q, %v", got, err)
	}
	// An exit after dispatch leaves the result unknown; a policy-held message
	// can still reach Claude later. Repeating the request would duplicate it.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() {
		conn, err := listener.AcceptUnix()
		if err == nil {
			defer conn.Close()
			var sent json.RawMessage
			if json.NewDecoder(conn).Decode(&sent) == nil {
				cancel2()
			}
		}
	}()
	if _, err := askClaudeSession(ctx2, c, "synthetic-id", "next"); !errors.Is(err, ErrSessionOutcomeUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled after dispatch: %v", err)
	}
}

func TestClaudeAnswerRequiresTargetEnvelope(t *testing.T) {
	const addr = "/tmp/synthetic.sock"
	const id = "request-id"
	const valid = "<cross-session-message from=\"uds:" + addr + "\">\nFINAL " + id + "\nmy answer\n</cross-session-message>"

	for _, tt := range []struct {
		name  string
		alter func(*claudeMessage)
		want  string
	}{
		{"target", func(*claudeMessage) {}, "my answer"},
		{"wrong sender", func(p *claudeMessage) { p.From = "uds:/tmp/another.sock" }, ""},
		{"unsupported version", func(p *claudeMessage) { p.Version++ }, ""},
		{"non-user packet", func(p *claudeMessage) { p.Type = "notice" }, ""},
		{"non-user role", func(p *claudeMessage) { p.Message.Role = "system" }, ""},
		{"missing header", func(p *claudeMessage) {
			p.Message.Content = strings.Replace(valid, "<cross-session-message ", "<other ", 1)
		}, ""},
		{"malformed header", func(p *claudeMessage) { p.Message.Content = strings.Replace(valid, ">\n", "\n", 1) }, ""},
		{"missing footer", func(p *claudeMessage) { p.Message.Content = strings.TrimSuffix(valid, "\n</cross-session-message>") }, ""},
		{"no final marker", func(p *claudeMessage) { p.Message.Content = strings.Replace(valid, "FINAL "+id, "progress", 1) }, ""},
		{"blank final", func(p *claudeMessage) { p.Message.Content = strings.Replace(valid, "my answer", "  ", 1) }, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := claudeMessage{Version: 1, Type: "user", From: "uds:" + addr}
			p.Message.Role = "user"
			p.Message.Content = valid
			tt.alter(&p)
			wire, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeClaudeAnswer(bytes.NewReader(wire), addr, id)
			if got != tt.want || err != nil {
				t.Fatalf("decode = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
