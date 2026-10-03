package stealthbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type gitSSHRequest struct {
	Args     []string `json:"args"`
	Protocol string   `json:"protocol,omitempty"`
}

var gitSSHCommand = regexp.MustCompile(`^(git-upload-pack|git-receive-pack|git-upload-archive) '(?:[^']|'\\'')*'$`)

func (request gitSSHRequest) validate() error {
	if len(request.Args) < 2 || len(request.Args) > 16 || (request.Protocol != "" && request.Protocol != "version=1" && request.Protocol != "version=2") {
		return fmt.Errorf("invalid Git SSH request")
	}
	args := request.Args
	for len(args) > 2 {
		switch args[0] {
		case "-4", "-6":
			args = args[1:]
		case "-p":
			if len(args) < 4 || args[1] == "" || strings.Trim(args[1], "0123456789") != "" {
				return fmt.Errorf("invalid Git SSH port")
			}
			args = args[2:]
		case "-o":
			if len(args) < 4 || args[1] != "SendEnv=GIT_PROTOCOL" {
				return fmt.Errorf("unsupported Git SSH option")
			}
			args = args[2:]
		default:
			return fmt.Errorf("unsupported Git SSH option")
		}
	}
	if len(args) != 2 || args[0] == "" || strings.HasPrefix(args[0], "-") || strings.ContainsAny(args[0], "\x00\n\r \t") {
		return fmt.Errorf("invalid Git SSH target")
	}
	command := args[1]
	if !gitSSHCommand.MatchString(command) {
		return fmt.Errorf("unsupported Git SSH command")
	}
	for _, arg := range request.Args {
		if len(arg) > 8192 || strings.ContainsAny(arg, "\x00\n\r") {
			return fmt.Errorf("invalid Git SSH argument")
		}
	}
	return nil
}

// []byte is encoded as base64 by JSON: Git pack data must survive byte-for-byte.
type gitSSHEvent struct {
	Stream string `json:"stream"`
	Data   []byte `json:"data,omitempty"`
	Code   int    `json:"code,omitempty"`
}

type gitSSHWriter struct {
	mu      *sync.Mutex
	encoder *json.Encoder
	flush   http.Flusher
	stream  string
}

func (writer gitSSHWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if err := writer.encoder.Encode(gitSSHEvent{Stream: writer.stream, Data: data}); err != nil {
		return 0, err
	}
	writer.flush.Flush()
	return len(data), nil
}

func serveGitSSH(w http.ResponseWriter, r *http.Request, c Config) {
	raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Stealthbox-Git-SSH"))
	var request gitSSHRequest
	if err != nil || len(raw) > 16384 || json.Unmarshal(raw, &request) != nil || request.validate() != nil {
		http.Error(w, "invalid Git SSH request", 400)
		return
	}
	// Git's transport reads and writes concurrently. Waiting for all stdin
	// before sending stdout would deadlock clone/fetch/push.
	if err = http.NewResponseController(w).EnableFullDuplex(); err != nil {
		http.Error(w, "full duplex transport unavailable", 500)
		return
	}
	_, timeout := limits(c)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	sshArgs := []string{"-o", "BatchMode=yes"}
	if config := os.Getenv("STEALTHBOX_SSH_CONFIG"); config != "" {
		sshArgs = append(sshArgs, "-F", config)
	}
	cmd := exec.CommandContext(ctx, "ssh", append(sshArgs, request.Args...)...)
	cmd.Env = append(os.Environ(), "GIT_PROTOCOL="+request.Protocol)
	input, err := cmd.StdinPipe()
	if err != nil {
		http.Error(w, "cannot start Git SSH transport", 500)
		return
	}
	defer input.Close()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream transport unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "close")
	w.WriteHeader(200)
	flusher.Flush()
	encoder := json.NewEncoder(w)
	mu := &sync.Mutex{}
	cmd.Stdout = gitSSHWriter{mu, encoder, flusher, "stdout"}
	cmd.Stderr = gitSSHWriter{mu, encoder, flusher, "stderr"}
	code := 0
	if err = cmd.Start(); err == nil {
		max, _ := limits(c)
		go func() { io.Copy(input, io.LimitReader(r.Body, max)); input.Close() }()
		defer http.NewResponseController(w).SetReadDeadline(time.Now())
		err = cmd.Wait()
	}
	if err != nil {
		code = 1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
			if code < 0 {
				code = 130
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	encoder.Encode(gitSSHEvent{Stream: "exit", Code: code})
	flusher.Flush()
}

// GitSSH runs the actual SSH transport on the Mac. This preserves its SSH
// host aliases, identity selection, known_hosts, keychain and network access.
func GitSSH(ctx context.Context, c Config, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if !remoteLocalContext(c) {
		return fmt.Errorf("Git SSH bridge is disabled")
	}
	request := gitSSHRequest{Args: args, Protocol: os.Getenv("GIT_PROTOCOL")}
	if err := request.validate(); err != nil {
		return err
	}
	data, _ := json.Marshal(request)
	req, err := http.NewRequestWithContext(ctx, "POST", "http://unix/git-ssh", stdin)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	req.Header.Set("X-Stealthbox-Git-SSH", base64.RawURLEncoding.EncodeToString(data))
	response, err := bridgeHTTP(c.Bridge.Socket).Do(req)
	if err != nil {
		return fmt.Errorf("Git SSH bridge is unavailable; reconnect Stealthbox")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("Git SSH bridge rejected the request (%d)", response.StatusCode)
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var event gitSSHEvent
		if err = decoder.Decode(&event); err != nil {
			return fmt.Errorf("Git SSH bridge closed before exit status: %w", err)
		}
		switch event.Stream {
		case "stdout":
			_, err = stdout.Write(event.Data)
		case "stderr":
			_, err = stderr.Write(event.Data)
		case "exit":
			if event.Code != 0 {
				return &ExitError{Code: event.Code}
			}
			return nil
		default:
			return fmt.Errorf("invalid Git SSH bridge event")
		}
		if err != nil {
			return err
		}
	}
}
