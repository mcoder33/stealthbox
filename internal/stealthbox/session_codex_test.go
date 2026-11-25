package stealthbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const codexTestID = "0188bcdd-3178-7c58-a226-27f3ea4f1fbb"

func TestCodexSessionAskCorrelatesOriginalTurn(t *testing.T) {
	c, _, rollout := codexSessionFixture(t, "answer")
	message := strings.Repeat("привет\n", 32*1024)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer, err := askCodexSession(ctx, c, codexTestID, message)
	if err != nil || answer != "correlated answer" {
		t.Fatalf("answer = %q, err = %v", answer, err)
	}
	if raw, err := os.ReadFile(rollout + ".queued"); err != nil || string(raw) != message {
		t.Fatalf("queued text did not round trip (len = %d, err = %v)", len(raw), err)
	}
}

func TestCodexSessionListUsesLiveWriterAndScope(t *testing.T) {
	c, home, _ := codexSessionFixture(t, "answer")
	stale := "0188bcdd-3178-7c58-a226-27f3ea4f1fbc"
	if err := os.WriteFile(filepath.Join(home, "thread-writer-locks", stale+".lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := listCodexSessions(context.Background(), c)
	if err != nil || len(result) != 1 || result[0].ID != codexTestID {
		t.Fatalf("sessions = %#v, err = %v", result, err)
	}
	c.Workspace.VMRoot = t.TempDir()
	result, err = listCodexSessions(context.Background(), c)
	if err != nil || len(result) != 0 {
		t.Fatalf("out-of-scope sessions = %#v, err = %v", result, err)
	}
	if _, err = askCodexSession(context.Background(), c, codexTestID, "disallowed"); err == nil {
		t.Fatal("out-of-scope ask succeeded")
	}
}

func TestCodexSessionListsInteractiveFork(t *testing.T) {
	c, _, _ := codexSessionFixture(t, "fork")
	list, err := listCodexSessions(context.Background(), c)
	if err != nil || len(list) != 1 || list[0].ID != codexTestID {
		t.Fatalf("live interactive fork = %#v, err = %v", list, err)
	}
}

func TestCodexSessionRejectsBusyAndReportsUnknown(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		c, _, rollout := codexSessionFixture(t, "answer")
		appendCodexFixture(t, rollout, []any{codexFixtureEvent("task_started", "active")})
		_, err := askCodexSession(context.Background(), c, codexTestID, "must stay unsent")
		if err == nil || !strings.Contains(err.Error(), "busy") || errors.Is(err, ErrSessionOutcomeUnknown) {
			t.Fatalf("busy ask err = %v", err)
		}
		if _, err := os.Stat(rollout + ".queued"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("busy ask queued message, sentinel err = %v", err)
		}
	})
	t.Run("partially persisted turn", func(t *testing.T) {
		c, _, rollout := codexSessionFixture(t, "answer")
		f, err := os.OpenFile(rollout, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(`{"type":"event_msg","payload":{"type":"task_started"`)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = askCodexSession(context.Background(), c, codexTestID, "must stay unsent")
		if err == nil || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("ask during append err = %v", err)
		}
		if _, err := os.Stat(rollout + ".queued"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ask during append queued message, sentinel err = %v", err)
		}
	})
	t.Run("lost acknowledgement", func(t *testing.T) {
		c, _, _ := codexSessionFixture(t, "lost")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := askCodexSession(ctx, c, codexTestID, "outcome may be unknown")
		if !errors.Is(err, ErrSessionOutcomeUnknown) {
			t.Fatalf("lost reply err = %v", err)
		}
	})
}

func TestCodexSessionParallelRequestCannotPostTwice(t *testing.T) {
	c, _, rollout := codexSessionFixture(t, "pending")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, err := askCodexSession(ctx, c, codexTestID, "first pending prompt")
		first <- err
	}()
	for {
		if _, err := os.Stat(rollout + ".queued"); err == nil {
			break
		}
		select {
		case err := <-first:
			t.Fatalf("first request did not queue: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	_, err := askCodexSession(ctx, c, codexTestID, "must not be queued")
	if err == nil || !strings.Contains(err.Error(), "already waiting") {
		t.Fatalf("second request error = %v", err)
	}
	cancel()
	if err := <-first; !errors.Is(err, ErrSessionOutcomeUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("first request error = %v", err)
	}
	if raw, err := os.ReadFile(rollout + ".queued"); err != nil || string(raw) != "first pending prompt" {
		t.Fatalf("unexpected queue state %q, %v", raw, err)
	}
}

func TestCodexSessionTerminalAnswers(t *testing.T) {
	cases := []struct {
		outcome string
		answer  string
		failure string
	}{
		{outcome: "legacy", answer: "correlated answer"},
		{outcome: "legacy-unconfirmed", failure: "without an answer"},
		{outcome: "commentary", failure: "without an answer"},
		{outcome: "interrupted", failure: "interrupted"},
		{outcome: "failed", failure: "failed the queued turn"},
		{outcome: "restricted", failure: "restricted display"},
	}
	for _, tc := range cases {
		t.Run(tc.outcome, func(t *testing.T) {
			c, _, _ := codexSessionFixture(t, tc.outcome)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			answer, err := askCodexSession(ctx, c, codexTestID, "correlate terminal answer")
			if answer != tc.answer || (err == nil) != (tc.failure == "") ||
				(err != nil && !strings.Contains(err.Error(), tc.failure)) {
				t.Fatalf("terminal result = %q, %v", answer, err)
			}
		})
	}
}

func TestCodexSessionListReportsMissingClient(t *testing.T) {
	c := Config{Agents: map[string][]string{"codex": {filepath.Join(t.TempDir(), "missing")}}}
	if _, err := listCodexSessions(context.Background(), c); err == nil {
		t.Fatal("missing CLI appeared as an empty session list")
	}
}

func codexSessionFixture(t *testing.T, outcome string) (Config, string, string) {
	t.Helper()
	home := t.TempDir()
	project := t.TempDir()
	locks := filepath.Join(home, "thread-writer-locks")
	if err := os.Mkdir(locks, 0700); err != nil {
		t.Fatal(err)
	}
	holder, err := os.Create(filepath.Join(locks, codexTestID+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if err = unix.Flock(int(holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(home, "sessions", "rollout-test-"+codexTestID+".jsonl")
	appendCodexFixture(t, rollout, []any{
		codexFixtureEvent("task_started", "previous"),
		codexFixtureEvent("task_complete", "previous"),
	})
	t.Setenv("STEALTHBOX_TEST_CODEX_HOME", home)
	t.Setenv("STEALTHBOX_TEST_CODEX_PROJECT", project)
	t.Setenv("STEALTHBOX_TEST_CODEX_ROLLOUT", rollout)
	t.Setenv("STEALTHBOX_TEST_CODEX_OUTCOME", outcome)
	return Config{
		Workspace: WorkspaceConfig{VMRoot: project},
		Agents:    map[string][]string{"codex": {os.Args[0], "-test.run=^TestCodexSessionFixture$", "--"}},
	}, home, rollout
}

func appendCodexFixture(t *testing.T, path string, events []any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
}

func codexFixtureEvent(kind, turn string) map[string]any {
	return map[string]any{"type": "event_msg", "payload": map[string]any{"type": kind, "turn_id": turn}}
}

func TestCodexSessionFixture(t *testing.T) {
	rollout := os.Getenv("STEALTHBOX_TEST_CODEX_ROLLOUT")
	if rollout == "" {
		return
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Thread string                  `json:"threadId"`
				Client string                  `json:"clientUserMessageId"`
				Input  []struct{ Text string } `json:"input"`
			} `json:"params"`
		}
		if err := decoder.Decode(&request); errors.Is(err, io.EOF) {
			os.Exit(0)
		} else if err != nil {
			t.Fatal(err)
		}
		var result any = struct{}{}
		outcome := os.Getenv("STEALTHBOX_TEST_CODEX_OUTCOME")
		switch request.Method {
		case "initialize":
			result = map[string]any{"codexHome": os.Getenv("STEALTHBOX_TEST_CODEX_HOME")}
		case "initialized":
			continue
		case "thread/read":
			thread := map[string]any{
				"id": request.Params.Thread, "name": "Original", "source": "cli",
				"cwd": os.Getenv("STEALTHBOX_TEST_CODEX_PROJECT"), "path": rollout,
			}
			if outcome == "fork" {
				thread["forkedFromId"] = "0188bcdd-3178-7c58-a226-27f3ea4f1fbc"
			}
			result = map[string]any{"thread": thread}
		case "thread/queue/list":
			result = map[string]any{"data": []string{}}
		case "thread/queue/add":
			if len(request.Params.Input) != 1 || request.Params.Client == "" {
				t.Fatal("bad queue input")
			}
			if err := os.WriteFile(rollout+".queued", []byte(request.Params.Input[0].Text), 0600); err != nil {
				t.Fatal(err)
			}
			if outcome == "lost" {
				os.Exit(0)
			}
			result = map[string]any{"queuedSubmission": map[string]any{"id": "queued", "clientUserMessageId": request.Params.Client}}
			if outcome == "pending" {
				break
			}
			appendCodexFixture(t, rollout, codexFixtureReply(outcome, request.Params.Client))
		default:
			t.Fatalf("unexpected RPC %s", request.Method)
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": result}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
}

func codexFixtureReply(outcome, clientID string) []any {
	const text = "correlated answer"
	phase := "final_answer"
	if outcome == "commentary" {
		phase = "commentary"
	}
	answer := map[string]any{
		"type": "AgentMessage", "phase": phase,
		"content": []map[string]any{{"type": "Text", "text": text}},
	}
	if outcome == "restricted" {
		answer["moderation"] = map[string]any{"moderated": true}
	}
	user := map[string]any{"type": "UserMessage", "client_id": clientID}
	userEvent := map[string]any{"type": "item_completed", "turn_id": "ours", "item": user}
	agentEvent := map[string]any{"type": "item_completed", "turn_id": "ours", "item": answer}
	terminal := map[string]any{"type": "task_complete", "turn_id": "ours", "last_agent_message": text}
	switch outcome {
	case "legacy", "legacy-unconfirmed":
		userEvent = map[string]any{"type": "user_message", "client_id": clientID}
		agentEvent = map[string]any{"type": "agent_message", "message": text, "phase": phase}
		if outcome == "legacy-unconfirmed" {
			terminal["last_agent_message"] = "different terminal text"
		}
	case "interrupted":
		terminal["type"] = "turn_aborted"
	case "failed":
		terminal["error"] = map[string]any{"message": "synthetic failure"}
	}
	return []any{
		codexFixtureEvent("task_started", "other"),
		codexFixtureEvent("task_complete", "other"),
		codexFixtureEvent("task_started", "ours"),
		map[string]any{"type": "event_msg", "payload": userEvent},
		map[string]any{"type": "event_msg", "payload": agentEvent},
		map[string]any{"type": "event_msg", "payload": terminal},
	}
}
