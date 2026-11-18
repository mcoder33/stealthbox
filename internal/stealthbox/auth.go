package stealthbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const (
	signedIn                = "signed in"
	signInNeeded            = "sign-in needed"
	authStatusUnavailable   = "status unavailable (check the native CLI on the VM)"
	nativeCLIIsMissing      = "stealthbox-native-cli-missing"
	defaultAuthProbeTimeout = 20 * time.Second
)

type agentSignIn struct {
	name, statusArgs, login string
	classify                func(code int, stdout, stderr []byte) string
}

var codexSignIn = agentSignIn{"codex", "login status", "codex login --device-auth", codexAuthStatus}
var claudeSignIn = agentSignIn{"claude", "auth status --json", "claude auth login", claudeAuthStatus}

func selectAgentSignInChecks(agent string) ([]agentSignIn, error) {
	switch agent {
	case "":
		return []agentSignIn{codexSignIn, claudeSignIn}, nil
	case "codex":
		return []agentSignIn{codexSignIn}, nil
	case "claude":
		return []agentSignIn{claudeSignIn}, nil
	default:
		return nil, fmt.Errorf("--agent must be codex or claude")
	}
}

// AgentAuthStatus checks the native CLIs in the VM user's SSH environment.
// Only normalized status is reported; CLI output can include account data.
func AgentAuthStatus(ctx context.Context, c Config, agent string, w io.Writer) error {
	host := c.Workspace.Host
	if !hostRE.MatchString(host) {
		return fmt.Errorf("configure workspace.host with an SSH alias")
	}
	agents, err := selectAgentSignInChecks(agent)
	if err != nil {
		return err
	}
	ready := true
	for _, agent := range agents {
		status := agent.probe(ctx, c)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n", agent.name, status); err != nil {
			return err
		}
		ready = ready && status == signedIn
	}
	if !ready {
		return fmt.Errorf("agent sign-in is incomplete or could not be verified on %s", host)
	}
	return nil
}

func (agent agentSignIn) probe(ctx context.Context, c Config) string {
	// Custom launchers may supply a separate account or have interactive effects.
	// Avoid running one during a read-only status check or probing the wrong account.
	if args, configured := c.Agents[agent.name]; configured && !slices.Equal(args, []string{agent.name}) {
		return "status unavailable (custom launcher; run its " + agent.statusArgs + " command in the VM shell)"
	}
	ctx, cancel := context.WithTimeout(ctx, defaultAuthProbeTimeout)
	defer cancel()
	script := remoteToolPath + "; command -v " + agent.name + " >/dev/null 2>&1 || { printf '%s' " + nativeCLIIsMissing + "; exit 127; }; exec " + agent.name + " " + agent.statusArgs
	var stdout, stderr bytes.Buffer
	err := ExecuteNoninteractiveContext(ctx, sshWithoutTTY(c.Workspace.Host, script), nil, &stdout, &stderr)
	status := agent.classifyProbeResult(ctx, err, stdout.Bytes(), stderr.Bytes())
	if status == signInNeeded {
		return signInNeeded + " (on the VM, run " + agent.login + ")"
	}
	return status
}

func (agent agentSignIn) classifyProbeResult(ctx context.Context, err error, stdout, stderr []byte) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "status unavailable (check timed out)"
	case err == nil:
		return agent.classify(0, stdout, stderr)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return "status unavailable (cannot run SSH)"
	}
	switch exit.ExitCode() {
	case 127:
		if string(stdout) == nativeCLIIsMissing {
			return "not installed (run stealthbox setup)"
		}
		return authStatusUnavailable
	case 255:
		return "SSH connection failed (check the SSH alias and key)"
	default:
		return agent.classify(exit.ExitCode(), stdout, stderr)
	}
}

func codexAuthStatus(code int, _, stderr []byte) string {
	if code == 0 {
		return signedIn
	}
	if code == 1 && strings.TrimSpace(string(stderr)) == "Not logged in" {
		return signInNeeded
	}
	return authStatusUnavailable
}

func claudeAuthStatus(code int, stdout, _ []byte) string {
	var response struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if err := json.Unmarshal(stdout, &response); err != nil || response.LoggedIn == nil {
		return authStatusUnavailable
	}
	if code == 1 && !*response.LoggedIn && response.AuthMethod == "none" {
		return signInNeeded
	}
	if code != 0 || !*response.LoggedIn {
		return authStatusUnavailable
	}
	switch response.AuthMethod {
	case "claude.ai", "oauth_token", "api_key", "api_key_helper":
		return signedIn
	case "third_party":
		return "external provider (verify access with that provider)"
	default:
		return authStatusUnavailable
	}
}
