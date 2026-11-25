package stealthbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type codexRollout struct {
	file             *os.File
	reader           *bufio.Reader
	partial          []byte
	activeTurn       string
	cwd              string
	matchedTurn      string
	modelAnswer      string
	modelAnswerFinal bool
	answerRestricted bool
	answer           *codexSessionAnswer
}

type codexSessionAnswer struct {
	text string
	err  error
}

type codexRolloutRecord struct {
	Type    string            `json:"type"`
	Payload codexRolloutEvent `json:"payload"`
}

type codexRolloutEvent struct {
	Type          string          `json:"type"`
	Turn          string          `json:"turn_id"`
	CWD           string          `json:"cwd"`
	ClientID      string          `json:"client_id"`
	Message       json.RawMessage `json:"message"`
	Phase         string          `json:"phase"`
	LastMessage   string          `json:"last_agent_message"`
	TerminalError json.RawMessage `json:"error"`
	Item          json.RawMessage `json:"item"`
	Moderation    json.RawMessage `json:"moderation"`
}

type codexRolloutItem struct {
	Type       string          `json:"type"`
	ClientID   string          `json:"client_id"`
	Phase      string          `json:"phase"`
	Moderation json.RawMessage `json:"moderation"`
	Content    json.RawMessage `json:"content"`
}

func openCodexRollout(path string) (*codexRollout, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// Serialize StealthBox sends without taking the separate lock Codex
	// uses to own this writer.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("another StealthBox request is already waiting for this Codex session")
		}
		return nil, err
	}
	return &codexRollout{file: f, reader: bufio.NewReader(f)}, nil
}

func (rollout *codexRollout) read(ctx context.Context, clientID string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := rollout.reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			rollout.partial = append(rollout.partial, line...)
			return nil
		}
		if err != nil {
			return err
		}
		if len(rollout.partial) != 0 {
			line = append(rollout.partial, line...)
			rollout.partial = nil
		}
		var record codexRolloutRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return fmt.Errorf("decode Codex rollout record: %w", err)
		}
		if err := rollout.applyRecord(record, clientID); err != nil {
			return fmt.Errorf("decode Codex rollout item: %w", err)
		}
		if rollout.answer != nil {
			return nil
		}
	}
}

func (rollout *codexRollout) applyRecord(record codexRolloutRecord, clientID string) error {
	event := record.Payload
	switch record.Type {
	case "turn_context":
		if event.CWD != "" {
			rollout.cwd = event.CWD
		}
	case "event_msg":
		return rollout.applyEvent(event, clientID)
	}
	return nil
}

func (rollout *codexRollout) applyEvent(event codexRolloutEvent, clientID string) error {
	switch event.Type {
	case "task_started", "turn_started":
		rollout.activeTurn = event.Turn
	case "task_complete", "turn_complete", "turn_aborted":
		rollout.finishTurn(event)
	default:
		if clientID != "" {
			return rollout.trackMessage(event, clientID)
		}
	}
	return nil
}

func (rollout *codexRollout) trackMessage(event codexRolloutEvent, clientID string) error {
	switch event.Type {
	case "item_completed":
		return rollout.trackItem(event, clientID)
	case "user_message":
		if event.ClientID == clientID {
			rollout.matchedTurn = rollout.activeTurn
		}
	case "agent_message":
		if rollout.matchedTurn != "" && rollout.matchedTurn == rollout.activeTurn {
			var text string
			if err := json.Unmarshal(event.Message, &text); err != nil {
				return err
			}
			// Legacy agent messages have no turn ID. Require the terminal
			// record to confirm their text even when a phase is available.
			rollout.trackAgentMessage(event.Phase, text, event.Moderation, false)
		}
	}
	return nil
}

func (rollout *codexRollout) trackItem(event codexRolloutEvent, clientID string) error {
	var item codexRolloutItem
	if err := json.Unmarshal(event.Item, &item); err != nil {
		return err
	}
	switch {
	case item.Type == "UserMessage" && item.ClientID == clientID:
		rollout.matchedTurn = event.Turn
	case item.Type == "AgentMessage" && rollout.matchedTurn != "" && event.Turn == rollout.matchedTurn:
		text, err := item.agentText()
		if err != nil {
			return err
		}
		rollout.trackAgentMessage(item.Phase, text, item.Moderation, true)
	}
	return nil
}

func (item codexRolloutItem) agentText() (string, error) {
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(item.Content, &content); err != nil {
		return "", err
	}
	var text strings.Builder
	for _, part := range content {
		if part.Type == "Text" {
			text.WriteString(part.Text)
		}
	}
	return text.String(), nil
}

func (rollout *codexRollout) trackAgentMessage(phase, text string, moderation json.RawMessage, hasTurnID bool) {
	if phase == "final_answer" || phase == "" {
		rollout.modelAnswer = text
		rollout.modelAnswerFinal = hasTurnID && phase == "final_answer"
		rollout.answerRestricted = codexHasValue(moderation)
	}
}

func codexHasValue(value json.RawMessage) bool {
	return len(value) > 0 && string(value) != "null"
}

func (rollout *codexRollout) finishTurn(event codexRolloutEvent) {
	turnID := event.Turn
	if turnID == "" {
		turnID = rollout.activeTurn
	}
	if turnID == rollout.activeTurn {
		rollout.activeTurn = ""
	}
	if rollout.matchedTurn != "" && turnID == rollout.matchedTurn {
		answer := rollout.completedAnswer(event)
		rollout.answer = &answer
	}
}

func (rollout *codexRollout) completedAnswer(event codexRolloutEvent) codexSessionAnswer {
	switch {
	case event.Type == "turn_aborted":
		return codexSessionAnswer{err: errors.New("Codex interrupted the queued turn")}
	case codexHasValue(event.TerminalError):
		return codexSessionAnswer{err: errors.New("Codex failed the queued turn")}
	case rollout.answerRestricted:
		return codexSessionAnswer{err: errors.New("Codex restricted display of the queued answer")}
	case !rollout.confirmedAnswer(event.LastMessage):
		return codexSessionAnswer{err: errors.New("Codex completed without an answer")}
	default:
		return codexSessionAnswer{text: rollout.modelAnswer}
	}
}

func (rollout *codexRollout) confirmedAnswer(lastMessage string) bool {
	return strings.TrimSpace(rollout.modelAnswer) != "" &&
		(rollout.modelAnswerFinal || rollout.modelAnswer == lastMessage)
}

func (rollout *codexRollout) waitForAnswer(ctx context.Context, lock, clientID string) (codexSessionAnswer, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		live, err := codexWriterIsLive(lock)
		if err != nil {
			return codexSessionAnswer{}, err
		}
		if err := rollout.read(ctx, clientID); err != nil {
			return codexSessionAnswer{}, err
		}
		if rollout.answer != nil {
			return *rollout.answer, nil
		}
		if !live {
			return codexSessionAnswer{}, errors.New("Codex session closed before its reply")
		}
		select {
		case <-ctx.Done():
			return codexSessionAnswer{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
