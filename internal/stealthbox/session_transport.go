package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrSessionOutcomeUnknown means a request may have reached the agent.
var ErrSessionOutcomeUnknown = errors.New("message outcome unknown")

// SessionCommand lists sessions when Target is empty and asks one session otherwise.
type SessionCommand struct {
	Target  string
	Timeout time.Duration
}

type sessionMessage struct {
	Message string `json:"message"`
}

type sessionReply struct {
	Agent  string `json:"agent"`
	ID     string `json:"id"`
	Answer string `json:"answer"`
}

func parseSessionTarget(target string) (agent, id string, err error) {
	agent, id, _ = strings.Cut(target, ":")
	if (agent != "codex" && agent != "claude") || !nameRE.MatchString(id) {
		return "", "", fmt.Errorf("invalid session target %q; select codex:ID or claude:ID from session list", target)
	}
	return agent, id, nil
}

// RunOverSSH uses the VM binary and configuration installed by setup.
func (cmd SessionCommand) RunOverSSH(ctx context.Context, c Config, stdin io.Reader, stdout, diagnostics io.Writer) error {
	host := c.Workspace.Host
	dir := c.Workspace.RemoteDir
	if !hostRE.MatchString(host) || dir == "" {
		return fmt.Errorf("run stealthbox setup to configure the VM session connection")
	}
	args := []string{filepath.Join(dir, "bin", "stealthbox"), "session-vm"}
	var payload io.Reader
	if cmd.Target == "" {
		args = append(args, "list")
	} else {
		if _, _, err := parseSessionTarget(cmd.Target); err != nil {
			return err
		}
		message, err := readSessionMessage(stdin)
		if err != nil {
			return err
		}
		// A truncated SSH stream must fail decoding before dispatch.
		payload = strings.NewReader(JSONString(sessionMessage{Message: message}) + "\n")
		args = append(args, "ask", cmd.Target)
	}
	args = append(args, "--config", filepath.Join(dir, "config.json"), "--timeout", cmd.Timeout.String())

	ctx, cancel := context.WithTimeout(ctx, cmd.Timeout)
	defer cancel()
	return cmd.exchangeSession(ctx, host, args, payload, stdout, diagnostics)
}

func readSessionMessage(stdin io.Reader) (string, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read session message from stdin: %w", err)
	}
	message := string(data)
	if !utf8.ValidString(message) || strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("session ask requires a non-empty UTF-8 message on stdin")
	}
	return message, nil
}

func (cmd SessionCommand) exchangeSession(ctx context.Context, host string, args []string, payload io.Reader, stdout, diagnostics io.Writer) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var reply bytes.Buffer
	ssh := sshWithoutTTY(host, remoteToolPath+"; exec "+shellArgs(args))
	if err := ExecuteNoninteractiveContext(ctx, ssh, payload, &reply, diagnostics); err != nil {
		return cmd.sessionSSHError(ctx, host, err)
	}
	if err := cmd.validateSessionReply(reply.Bytes()); err != nil {
		if cmd.Target != "" {
			return cmd.unknownSessionOutcome(err)
		}
		return err
	}
	_, err := reply.WriteTo(stdout)
	return err
}

func (cmd SessionCommand) sessionSSHError(ctx context.Context, host string, err error) error {
	var exit *exec.ExitError
	disconnected := errors.As(err, &exit) && exit.ExitCode() == 255
	if ctx.Err() != nil {
		err = ctx.Err()
		disconnected = true
	}
	if disconnected && cmd.Target != "" {
		return cmd.unknownSessionOutcome(fmt.Errorf("SSH to %s: %w", host, err))
	}
	return fmt.Errorf("VM session command via %s failed: %w", host, err)
}

func (cmd SessionCommand) unknownSessionOutcome(err error) error {
	return fmt.Errorf("%w for %s: %w; check the VM session before sending again", ErrSessionOutcomeUnknown, cmd.Target, err)
}

func (cmd SessionCommand) validateSessionReply(raw []byte) error {
	if cmd.Target == "" {
		var sessions []VMSession
		if err := json.Unmarshal(raw, &sessions); err == nil && sessions != nil {
			return nil
		}
		return fmt.Errorf("invalid VM session list; check the VM binary and SSH startup output")
	}
	var reply sessionReply
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Agent+":"+reply.ID != cmd.Target || reply.Answer == "" {
		return fmt.Errorf("invalid VM session answer; check the VM binary and SSH startup output")
	}
	return nil
}

// RunOnVM handles the framed SSH request in the VM account.
func (cmd SessionCommand) RunOnVM(ctx context.Context, c Config, stdin io.Reader, stdout, diagnostics io.Writer) error {
	timeout := cmd.Timeout
	if cmd.Target == "" {
		// Leave the SSH caller time to connect and receive partial results.
		timeout /= 2
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	encoder := json.NewEncoder(stdout)
	if cmd.Target == "" {
		sessions, err := listSessionProviders(ctx, c, diagnostics,
			sessionProvider{"codex", listCodexSessions},
			sessionProvider{"claude", listClaudeSessions},
		)
		if err != nil {
			return err
		}
		return encoder.Encode(sessions)
	}
	reply, err := askVMSession(ctx, c, cmd.Target, stdin)
	if err != nil {
		return err
	}
	return encoder.Encode(reply)
}

type sessionProvider struct {
	name string
	list func(context.Context, Config) ([]VMSession, error)
}

type sessionListResult struct {
	sessions []VMSession
	err      error
}

func (p sessionProvider) startList(ctx context.Context, c Config) <-chan sessionListResult {
	results := make(chan sessionListResult, 1)
	go func() {
		sessions, err := p.list(ctx, c)
		results <- sessionListResult{sessions: sessions, err: err}
	}()
	return results
}

func readSessionList(ctx context.Context, results <-chan sessionListResult) sessionListResult {
	select {
	case result := <-results:
		return result
	case <-ctx.Done():
		// A result may already be available when the deadline expires.
		select {
		case result := <-results:
			return result
		default:
			return sessionListResult{err: ctx.Err()}
		}
	}
}

func listSessionProviders(ctx context.Context, c Config, diagnostics io.Writer, providers ...sessionProvider) ([]VMSession, error) {
	results := make([]<-chan sessionListResult, len(providers))
	for i, p := range providers {
		results[i] = p.startList(ctx, c)
	}
	sessions := make([]VMSession, 0)
	var failures []error
	for i, p := range providers {
		result := readSessionList(ctx, results[i])
		sessions = append(sessions, result.sessions...)
		if result.err != nil {
			failures = append(failures, fmt.Errorf("%s sessions: %w", p.name, result.err))
		}
	}
	if len(failures) == len(providers) {
		return nil, errors.Join(failures...)
	}
	for _, err := range failures {
		fmt.Fprintln(diagnostics, err)
	}
	return sessions, nil
}

func askVMSession(ctx context.Context, c Config, target string, stdin io.Reader) (sessionReply, error) {
	agent, id, err := parseSessionTarget(target)
	if err != nil {
		return sessionReply{}, err
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return sessionReply{}, fmt.Errorf("read VM session request: %w", err)
	}
	var request sessionMessage
	if err := json.Unmarshal(data, &request); err != nil || strings.TrimSpace(request.Message) == "" {
		return sessionReply{}, fmt.Errorf("incomplete or invalid VM session request")
	}
	if err := ctx.Err(); err != nil {
		return sessionReply{}, err
	}
	var answer string
	if agent == "codex" {
		answer, err = askCodexSession(ctx, c, id, request.Message)
	} else {
		answer, err = askClaudeSession(ctx, c, id, request.Message)
	}
	if err != nil {
		return sessionReply{}, fmt.Errorf("ask %s: %w", target, err)
	}
	return sessionReply{Agent: agent, ID: id, Answer: answer}, nil
}
