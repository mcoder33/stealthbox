package stealthbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type codexSessionApp struct {
	ctx      context.Context
	home     string
	shutdown func()
	output   *json.Decoder
	input    *json.Encoder
	nextID   int
}

type codexSessionRPCRejected struct {
	Method string
	Code   int
}

func (e codexSessionRPCRejected) Error() string {
	return fmt.Sprintf("Codex app-server rejected %s (code %d)", e.Method, e.Code)
}

func openCodexSessionApp(ctx context.Context, c Config) (*codexSessionApp, error) {
	argv := sessionAgentArgs(c, "codex")
	if len(argv) == 0 {
		return nil, errors.New("Codex command is not configured")
	}
	app, err := startCodexSessionApp(ctx, argv)
	if err != nil {
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	if err := app.initialize(); err != nil {
		app.close()
		return nil, fmt.Errorf("initialize Codex app-server: %w", err)
	}
	return app, nil
}

func startCodexSessionApp(ctx context.Context, argv []string) (*codexSessionApp, error) {
	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, argv[0], append(argv[1:], "app-server")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	stdin, stdout, err := codexSessionPipes(cmd)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	stop := context.AfterFunc(childCtx, func() {
		_ = stdin.Close()
		_ = stdout.Close()
	})
	return &codexSessionApp{
		ctx:    ctx,
		input:  json.NewEncoder(stdin),
		output: json.NewDecoder(stdout),
		shutdown: func() {
			cancel()
			_ = cmd.Cancel()
			_ = cmd.Wait()
			stop()
		},
	}, nil
}

func codexSessionPipes(cmd *exec.Cmd) (io.WriteCloser, io.ReadCloser, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	return stdin, stdout, nil
}

func (app *codexSessionApp) close() {
	app.shutdown()
}

func (app *codexSessionApp) initialize() error {
	params := map[string]any{
		"clientInfo":   map[string]string{"name": "stealthbox", "version": BuildVersion},
		"capabilities": map[string]bool{"experimentalApi": true, "requestAttestation": false},
	}
	var hello struct {
		Home string `json:"codexHome"`
	}
	if err := app.call("initialize", params, &hello); err != nil {
		return err
	}
	if !filepath.IsAbs(hello.Home) {
		return errors.New("Codex app-server did not report a local home directory")
	}
	app.home = hello.Home
	return app.input.Encode(map[string]string{"method": "initialized"})
}

func (app *codexSessionApp) call(method string, params map[string]any, result any) error {
	if err := app.ctx.Err(); err != nil {
		return err
	}
	app.nextID++
	request := map[string]any{"id": app.nextID, "method": method, "params": params}
	if err := app.input.Encode(request); err != nil {
		return app.transportError(method, err)
	}
	return app.readResult(method, app.nextID, result)
}

func (app *codexSessionApp) readResult(method string, id int, result any) error {
	for {
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := app.output.Decode(&frame); err != nil {
			return app.transportError(method, err)
		}
		if frame.Method != "" && codexHasValue(frame.ID) {
			return fmt.Errorf("Codex app-server requires interactive handling for %s", frame.Method)
		}
		if string(frame.ID) != fmt.Sprint(id) || frame.Method != "" {
			continue
		}
		if frame.Error != nil {
			return codexSessionRPCRejected{Method: method, Code: frame.Error.Code}
		}
		if len(frame.Result) == 0 {
			return fmt.Errorf("empty Codex app-server response to %s", method)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(frame.Result, result)
	}
}

func (app *codexSessionApp) transportError(method string, err error) error {
	if ctxErr := app.ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	return fmt.Errorf("Codex app-server %s: %w", method, err)
}

func (app *codexSessionApp) readThread(id string) (codexSessionThread, error) {
	var result struct {
		Thread codexSessionThread `json:"thread"`
	}
	if err := app.call("thread/read", map[string]any{"threadId": id}, &result); err != nil {
		return codexSessionThread{}, err
	}
	if result.Thread.ID != id {
		return codexSessionThread{}, errors.New("Codex app-server returned a different thread")
	}
	return result.Thread, nil
}

func (app *codexSessionApp) queueMessage(threadID, clientID, message string) error {
	var result struct {
		Submission struct {
			ID       string `json:"id"`
			ClientID string `json:"clientUserMessageId"`
		} `json:"queuedSubmission"`
	}
	params := map[string]any{
		"threadId": threadID, "clientUserMessageId": clientID,
		"input": []map[string]any{{"type": "text", "text": message, "text_elements": []any{}}},
	}
	err := app.call("thread/queue/add", params, &result)
	var rejected codexSessionRPCRejected
	if errors.As(err, &rejected) {
		return err
	}
	if err == nil && (result.Submission.ID == "" || result.Submission.ClientID != clientID) {
		err = errors.New("queue acknowledgement was missing or mismatched")
	}
	if err != nil {
		return unknownCodexMessage(clientID, err)
	}
	return nil
}
