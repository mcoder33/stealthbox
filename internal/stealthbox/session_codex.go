package stealthbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

var codexThreadID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type codexSessionThread struct {
	ID        string          `json:"id"`
	CWD       string          `json:"cwd"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	Source    json.RawMessage `json:"source"`
	Parent    string          `json:"parentThreadId"`
	Attached  string          `json:"attachedToThreadId"`
	Transient bool            `json:"ephemeral"`
	Direct    *bool           `json:"canAcceptDirectInput"`
}

func (thread codexSessionThread) interactive() bool {
	switch string(thread.Source) {
	case `"cli"`, `"vscode"`, `"appServer"`:
		return thread.Parent == "" && thread.Attached == "" &&
			!thread.Transient && (thread.Direct == nil || *thread.Direct)
	default:
		return false
	}
}

func (thread codexSessionThread) session() VMSession {
	name := thread.Name
	if name == "" {
		name = filepath.Base(thread.CWD)
	}
	return VMSession{ID: thread.ID, Name: name, CWD: thread.CWD, Agent: "codex"}
}

func codexWriterLock(home, id string) string {
	return filepath.Join(home, "thread-writer-locks", id+".lock")
}

func codexWriterIsLive(path string) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	// Codex holds an exclusive flock for the lifetime of a live local thread.
	// A lock file left after a crash is insufficient evidence by itself.
	err = unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

func codexLiveThreadIDs(home string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(home, "thread-writer-locks"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		id, lock := strings.CutSuffix(entry.Name(), ".lock")
		if !lock || !entry.Type().IsRegular() || !codexThreadID.MatchString(id) {
			continue
		}
		live, err := codexWriterIsLive(codexWriterLock(home, id))
		if err != nil {
			return nil, err
		}
		if live {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (thread codexSessionThread) activeRollout(home string) (string, error) {
	root, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(thread.Path)
	if thread.Path == "" || err != nil || !within(filepath.Join(root, "sessions"), path) ||
		!strings.HasSuffix(filepath.Base(path), "-"+thread.ID+".jsonl") {
		return "", errors.New("Codex session has no readable active rollout")
	}
	return path, nil
}

func listCodexSessions(ctx context.Context, c Config) ([]VMSession, error) {
	app, err := openCodexSessionApp(ctx, c)
	if err != nil {
		return nil, err
	}
	defer app.close()
	ids, err := codexLiveThreadIDs(app.home)
	if err != nil {
		return nil, err
	}
	var sessions []VMSession
	for _, id := range ids {
		session, err := app.visibleSession(c, id)
		if err != nil {
			return sessions, err
		}
		if session.ID != "" {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func (app *codexSessionApp) visibleSession(c Config, id string) (VMSession, error) {
	thread, err := app.readThread(id)
	if err != nil {
		var rejected codexSessionRPCRejected
		if errors.As(err, &rejected) && rejected.Code == -32600 {
			// The writer can close or be opened before its first saved turn.
			return VMSession{}, nil
		}
		return VMSession{}, err
	}
	if !thread.interactive() || !sessionInScope(c, thread.CWD) {
		return VMSession{}, nil
	}
	if _, err := thread.activeRollout(app.home); err != nil {
		return VMSession{}, nil
	}
	live, err := codexWriterIsLive(codexWriterLock(app.home, id))
	if err != nil || !live {
		return VMSession{}, err
	}
	return thread.session(), nil
}

func askCodexSession(ctx context.Context, c Config, id, message string) (string, error) {
	if !codexThreadID.MatchString(id) {
		return "", errors.New("invalid Codex session ID")
	}
	app, err := openCodexSessionApp(ctx, c)
	if err != nil {
		return "", err
	}
	defer app.close()
	lock := codexWriterLock(app.home, id)
	if err := requireCodexWriter(lock); err != nil {
		return "", err
	}
	return app.askExisting(c, lock, id, message)
}

func (app *codexSessionApp) askExisting(c Config, lock, id, message string) (string, error) {
	thread, err := app.readThread(id)
	if err != nil {
		return "", err
	}
	if !thread.interactive() || !sessionInScope(c, thread.CWD) {
		return "", errors.New("Codex session is unavailable in this VM workspace")
	}
	path, err := thread.activeRollout(app.home)
	if err != nil {
		return "", err
	}
	rollout, err := openCodexRollout(path)
	if err != nil {
		return "", err
	}
	defer rollout.file.Close()
	if err := app.requireCodexIdle(c, id, rollout); err != nil {
		return "", err
	}
	if err := requireCodexWriter(lock); err != nil {
		return "", err
	}

	clientID := rand.Text()
	if err := app.queueMessage(id, clientID, message); err != nil {
		return "", err
	}
	answer, err := rollout.waitForAnswer(app.ctx, lock, clientID)
	if err != nil {
		return "", unknownCodexMessage(clientID, err)
	}
	return answer.text, answer.err
}

func unknownCodexMessage(clientID string, err error) error {
	return fmt.Errorf("%w: Codex message %s: %w; check the session before sending again", ErrSessionOutcomeUnknown, clientID, err)
}

func requireCodexWriter(lock string) error {
	live, err := codexWriterIsLive(lock)
	if err != nil {
		return err
	}
	if !live {
		return errors.New("Codex session is not open on the VM")
	}
	return nil
}

func (app *codexSessionApp) requireCodexIdle(c Config, id string, rollout *codexRollout) error {
	var queued struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := app.call("thread/queue/list", map[string]any{"threadId": id, "limit": 1}, &queued); err != nil {
		return err
	}
	if queued.Data == nil {
		return errors.New("Codex app-server did not report the pending queue")
	}
	if err := rollout.read(app.ctx, ""); err != nil {
		return err
	}
	if rollout.cwd != "" && !sessionInScope(c, rollout.cwd) {
		return errors.New("Codex session is unavailable in this VM workspace")
	}
	if rollout.activeTurn != "" || len(queued.Data) != 0 || len(rollout.partial) != 0 {
		return errors.New("Codex session is busy or has pending messages")
	}
	return nil
}
