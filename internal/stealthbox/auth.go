package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type agentSignIn struct {
	name, statusArgs, login string
}

var codexSignIn = agentSignIn{"codex", "login status", "codex login --device-auth"}
var claudeSignIn = agentSignIn{"claude", "auth status --json", "claude auth login"}

// AgentAuthStatus checks the native CLIs in the VM user's SSH environment.
// Only normalized status is reported; CLI output can include account data.
func AgentAuthStatus(ctx context.Context, host, agent string, w io.Writer) error {
	if !hostRE.MatchString(host) {
		return fmt.Errorf("configure workspace.host with an SSH alias")
	}
	var agents []agentSignIn
	switch agent {
	case "":
		agents = []agentSignIn{codexSignIn, claudeSignIn}
	case "codex":
		agents = []agentSignIn{codexSignIn}
	case "claude":
		agents = []agentSignIn{claudeSignIn}
	default:
		return fmt.Errorf("--agent must be codex or claude")
	}
	ready := true
	for _, agent := range agents {
		status := agent.probe(ctx, host)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n", agent.name, status); err != nil {
			return err
		}
		ready = ready && status == "signed in"
	}
	if !ready {
		return fmt.Errorf("agent sign-in is incomplete or could not be verified on %s", host)
	}
	return nil
}

func (agent agentSignIn) probe(ctx context.Context, host string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	script := remoteToolPath + "; command -v " + agent.name + " >/dev/null 2>&1 || exit 127; exec " + agent.name + " " + agent.statusArgs
	cmd := sshCommand(host, script, false)
	cmd.Args = append([]string{"-T"}, cmd.Args...)
	var stdout, stderr bytes.Buffer
	err := ExecuteNoninteractiveContext(ctx, cmd, nil, &stdout, &stderr)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "status unavailable (check timed out)"
	case err == nil:
		return agent.status(0, stdout.Bytes(), stderr.String())
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return "status unavailable (cannot run SSH)"
	}
	switch exit.ExitCode() {
	case 127:
		return "not installed (run stealthbox setup)"
	case 255:
		return "SSH connection failed (check the SSH alias and key)"
	default:
		return agent.status(exit.ExitCode(), stdout.Bytes(), stderr.String())
	}
}

func (agent agentSignIn) status(code int, stdout []byte, stderr string) string {
	switch agent.name {
	case "codex":
		if code == 0 {
			return "signed in"
		}
		if code == 1 && strings.TrimSpace(stderr) == "Not logged in" {
			return "sign-in needed (on the VM, run " + agent.login + ")"
		}
	case "claude":
		var response struct {
			LoggedIn   *bool  `json:"loggedIn"`
			AuthMethod string `json:"authMethod"`
		}
		if err := json.Unmarshal(stdout, &response); err != nil || response.LoggedIn == nil {
			break
		}
		if code == 1 && !*response.LoggedIn && response.AuthMethod == "none" {
			return "sign-in needed (on the VM, run " + agent.login + ")"
		}
		if code == 0 && *response.LoggedIn {
			switch response.AuthMethod {
			case "claude.ai", "oauth_token", "api_key", "api_key_helper":
				return "signed in"
			case "third_party":
				return "external provider (verify access with that provider)"
			}
		}
	}
	return "status unavailable (check the native CLI on the VM)"
}
