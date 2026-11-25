package stealthbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The native listing identifies the conversation. Its per-process registration
// supplies the current inbox, which is deliberately absent from agents --json.
type claudeSessionIdentity struct {
	ID        string `json:"sessionId"`
	CWD       string `json:"cwd"`
	PID       int    `json:"pid"`
	StartedAt int64  `json:"startedAt"`
}

type claudeSession struct {
	claudeSessionIdentity
	Name   string `json:"name"`
	Socket string `json:"messagingSocketPath"`
}

func activeClaudeSessions(ctx context.Context, c Config) ([]claudeSession, error) {
	args := append(sessionAgentArgs(c, "claude"), "agents", "--json")
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
	if err != nil {
		return nil, fmt.Errorf("list native Claude sessions: %w", err)
	}
	var rows []claudeSession
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("invalid Claude session list: %w", err)
	}
	return rows, nil
}

func (s claudeSession) inbox() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".claude")
	}
	b, err := os.ReadFile(filepath.Join(dir, "sessions", strconv.Itoa(s.PID)+".json"))
	if err != nil {
		return "", err
	}
	var current claudeSession
	if err := json.Unmarshal(b, &current); err != nil {
		return "", err
	}
	if current.claudeSessionIdentity != s.claudeSessionIdentity || !filepath.IsAbs(current.Socket) {
		return "", fmt.Errorf("Claude session registration changed or has no peer inbox")
	}
	info, err := os.Lstat(current.Socket)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", fmt.Errorf("Claude peer inbox is not a Unix socket")
	}
	return current.Socket, nil
}

func listClaudeSessions(ctx context.Context, c Config) ([]VMSession, error) {
	rows, err := activeClaudeSessions(ctx, c)
	if err != nil {
		return nil, err
	}
	var result []VMSession
	for _, row := range rows {
		if row.PID <= 0 || !nameRE.MatchString(row.ID) || !sessionInScope(c, row.CWD) {
			continue
		}
		if _, err := row.inbox(); err != nil {
			continue
		}
		result = append(result, VMSession{ID: row.ID, Name: row.Name, CWD: row.CWD, Agent: "claude"})
	}
	return result, nil
}

func askClaudeSession(ctx context.Context, c Config, id, message string) (string, error) {
	rows, err := activeClaudeSessions(ctx, c)
	if err != nil {
		return "", err
	}
	var target *claudeSession
	for _, row := range rows {
		if row.ID == id && row.PID > 0 {
			if target != nil {
				return "", fmt.Errorf("multiple live Claude processes have session ID %s", id)
			}
			target = &row
		}
	}
	if target == nil || !sessionInScope(c, target.CWD) {
		return "", fmt.Errorf("Claude session %s is unavailable in the configured workspace", id)
	}
	addr, err := target.inbox()
	if err != nil {
		return "", fmt.Errorf("resolve Claude inbox: %w", err)
	}
	return exchangeClaudeMessage(ctx, target.PID, addr, message)
}

type claudeMessage struct {
	Version  int    `json:"msgV"`
	ID       string `json:"msg_id,omitempty"`
	Type     string `json:"type"`
	From     string `json:"from"`
	Priority string `json:"priority,omitempty"`
	Message  struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

func exchangeClaudeMessage(ctx context.Context, pid int, addr, message string) (string, error) {
	// One private reply inbox per question also separates concurrent requests.
	dir, err := os.MkdirTemp("/tmp", "sb-peer-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	replyPath := filepath.Join(dir, "reply.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: replyPath, Net: "unix"})
	if err != nil {
		return "", err
	}
	defer listener.Close()
	stopAccept := context.AfterFunc(ctx, func() { listener.Close() })
	defer stopAccept()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	socket := conn.(*net.UnixConn)
	if err := checkClaudePeer(socket, pid); err != nil {
		return "", err
	}
	stopSend := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopSend()
	packet := newClaudeQuestion(replyPath, message)
	frame, err := json.Marshal(packet)
	if err != nil {
		return "", err
	}
	if len(frame) > 1_000_000 {
		return "", fmt.Errorf("message exceeds the Claude peer inbox frame limit")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		return "", fmt.Errorf("%w: write to Claude inbox: %w", ErrSessionOutcomeUnknown, err)
	}
	if err := socket.CloseWrite(); err != nil {
		return "", fmt.Errorf("%w: close Claude inbox write side: %w", ErrSessionOutcomeUnknown, err)
	}
	answer, err := receiveClaudeAnswer(ctx, listener, pid, addr, packet.ID)
	if err != nil {
		return "", fmt.Errorf("%w: no verified Claude reply (the receiving session may hold or refuse messages): %w", ErrSessionOutcomeUnknown, err)
	}
	return answer, nil
}

func newClaudeQuestion(replyPath, message string) claudeMessage {
	var key [16]byte
	rand.Read(key[:])
	key[6] = key[6]&15 | 64
	key[8] = key[8]&63 | 128
	packet := claudeMessage{Version: 1, ID: fmt.Sprintf("%x-%x-%x-%x-%x", key[0:4], key[4:6], key[6:8], key[8:10], key[10:]), Type: "user", From: "uds:" + replyPath, Priority: "next"}
	packet.Message.Role = "user"
	// Script senders have no Claude permission class. The destination applies
	// its own inbound controls to this message, including hold and refuse.
	packet.Message.Content = "<cross-session-message from=\"" + packet.From + "\" from-name=\"Stealth Box\">\nUse SendMessage to reply to this sender. Send the complete final answer in one message, starting with this exact line:\nFINAL " + packet.ID + "\n\n" + message + "\n</cross-session-message>"
	return packet
}

func checkClaudePeer(conn *net.UnixConn, pid int) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peerErr error
	if err := raw.Control(func(fd uintptr) { peerErr = verifyClaudePeerPID(int(fd), pid) }); err != nil {
		return err
	}
	return peerErr
}

func receiveClaudeAnswer(ctx context.Context, listener *net.UnixListener, pid int, addr, id string) (string, error) {
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", err
		}
		if err := checkClaudePeer(conn, pid); err != nil {
			conn.Close()
			continue
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		answer, err := decodeClaudeAnswer(conn, addr, id)
		stop()
		conn.Close()
		if err != nil || answer != "" {
			return answer, err
		}
	}
}

func decodeClaudeAnswer(r io.Reader, addr, id string) (string, error) {
	var packet claudeMessage
	if err := json.NewDecoder(io.LimitReader(r, 4<<20)).Decode(&packet); err != nil {
		return "", err
	}
	if packet.Type != "user" || packet.Version != 1 || packet.From != "uds:"+addr || packet.Message.Role != "user" {
		return "", nil
	}
	header, body, found := strings.Cut(packet.Message.Content, ">\n")
	if !found || !strings.HasPrefix(header, "<cross-session-message ") {
		return "", nil
	}
	body, closed := strings.CutSuffix(body, "\n</cross-session-message>")
	answer, final := strings.CutPrefix(body, "FINAL "+id+"\n")
	if !closed || !final || strings.TrimSpace(answer) == "" {
		return "", nil
	}
	return answer, nil
}
