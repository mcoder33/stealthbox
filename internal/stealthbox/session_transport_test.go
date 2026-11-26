package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSessionListReturnsTheResponsiveProvider(t *testing.T) {
	stopped := make(chan struct{})
	t.Cleanup(func() { close(stopped) })
	hanging := sessionProvider{"claude", func(context.Context, Config) ([]VMSession, error) {
		<-stopped
		return nil, errors.New("provider stopped")
	}}
	ready := sessionProvider{"codex", func(context.Context, Config) ([]VMSession, error) {
		return []VMSession{{Agent: "codex", ID: "available"}}, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var diagnostics bytes.Buffer
	sessions, err := listSessionProviders(ctx, Config{}, &diagnostics, hanging, ready)
	if err != nil || len(sessions) != 1 || sessions[0].ID != "available" {
		t.Fatalf("sessions=%v, error=%v, stderr=%q", sessions, err, diagnostics.String())
	}
	if !strings.Contains(diagnostics.String(), "claude sessions: context deadline exceeded") {
		t.Fatalf("missing provider error on stderr: %q", diagnostics.String())
	}
}

func TestSessionListFailsWhenBothProvidersFail(t *testing.T) {
	missing := errors.New("native CLI unavailable")
	probe := func(context.Context, Config) ([]VMSession, error) { return nil, missing }
	var diagnostics bytes.Buffer
	sessions, err := listSessionProviders(t.Context(), Config{}, &diagnostics,
		sessionProvider{"codex", probe}, sessionProvider{"claude", probe},
	)
	if !errors.Is(err, missing) || sessions != nil ||
		!strings.Contains(err.Error(), "codex sessions") || !strings.Contains(err.Error(), "claude sessions") {
		t.Fatalf("sessions=%v, error=%v", sessions, err)
	}
}

func TestSessionVMRefusesAnIncompleteMessage(t *testing.T) {
	var stdout, diagnostics bytes.Buffer
	cmd := SessionCommand{Target: "codex:session-id", Timeout: time.Second}
	err := cmd.RunOnVM(t.Context(), Config{}, strings.NewReader(`{"message":"partial`), &stdout, &diagnostics)
	if err == nil || !strings.Contains(err.Error(), "incomplete or invalid VM session request") || stdout.Len() != 0 {
		t.Fatalf("error=%v, stdout=%q", err, stdout.String())
	}
}

func TestSessionVMDispatchesToAnExistingCodexSession(t *testing.T) {
	config, _, rollout := codexSessionFixture(t, "answer")
	cmd := SessionCommand{Target: "codex:" + codexTestID, Timeout: 5 * time.Second}
	message := "Проверь diff.\nЭто уточнение.\n"
	request := JSONString(sessionMessage{Message: message})
	var stdout, diagnostics bytes.Buffer
	if err := cmd.RunOnVM(t.Context(), config, strings.NewReader(request), &stdout, &diagnostics); err != nil {
		t.Fatal(err)
	}

	var reply sessionReply
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if want := (sessionReply{Agent: "codex", ID: codexTestID, Answer: "correlated answer"}); reply != want {
		t.Fatalf("reply = %#v, want %#v", reply, want)
	}
	if queued, err := os.ReadFile(rollout + ".queued"); err != nil || string(queued) != message {
		t.Fatalf("queued = %q, error = %v", queued, err)
	}
}
