package stealthbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const localContextLimit = 1 << 20

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type localContextSnapshot struct {
	Environment map[string]string `json:"environment"`
	SSHAgent    bool              `json:"ssh_agent"`
}

func LocalContextEnabled(c Config) bool {
	return c.Bridge.Enabled && (c.Bridge.LocalContext == nil || *c.Bridge.LocalContext)
}

func remoteLocalContext(c Config) bool {
	return LocalContextEnabled(c) && c.Bridge.RemoteSocket != "" && c.Bridge.Socket == c.Bridge.RemoteSocket
}

func localContextLaunch(c Config) bool {
	_, inherited := os.LookupEnv("STEALTHBOX_LOCAL_ENV_KEYS")
	return remoteLocalContext(c) || inherited && c.Bridge.RemoteSocket != "" && c.Bridge.Socket == c.Bridge.RemoteSocket
}

func transferableEnvironment(name string) bool {
	if !environmentName.MatchString(name) {
		return false
	}
	for _, prefix := range []string{"STEALTHBOX_", "SSH_", "TMUX", "TERM", "ITERM_", "KITTY_", "XDG_", "LC_", "__CF", "GIT_CONFIG", "GIT_TRACE"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	switch name {
	case "HOME", "USER", "LOGNAME", "PATH", "PWD", "OLDPWD", "SHELL", "SHLVL", "_", "TMP", "TEMP", "TMPDIR", "LANG", "LANGUAGE", "COLORTERM", "COLORFGBG", "EDITOR", "VISUAL", "ZDOTDIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_TERMINAL_PROMPT", "GCM_INTERACTIVE":
		return false
	}
	return true
}

func terminalEnvironment() map[string]string {
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && transferableEnvironment(name) {
			environment[name] = value
		}
	}
	return environment
}

func localAgentSocket() string {
	socket := os.Getenv("SSH_AUTH_SOCK")
	info, err := os.Stat(socket)
	if err == nil && info.Mode()&os.ModeSocket != 0 {
		return socket
	}
	return ""
}

func contextConfigHash(c Config, environment map[string]string, agent string) string {
	var runtime any
	if LocalContextEnabled(c) {
		runtime = struct {
			Environment map[string]string
			Agent       string
		}{environment, agent}
	}
	b, _ := json.Marshal(struct {
		Config  Config
		Runtime any
	}{c, runtime})
	hash := sha256.Sum256(b)
	return fmt.Sprintf("%x", hash)
}

// Values travel over the authenticated SSH bridge, never through config or argv.
func forwardedContext(ctx context.Context, c Config) (localContextSnapshot, error) {
	var snapshot localContextSnapshot
	if !remoteLocalContext(c) {
		return snapshot, nil
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probe, "GET", "http://unix/local-context", nil)
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	response, err := bridgeHTTP(c.Bridge.Socket).Do(req)
	if err != nil {
		return snapshot, fmt.Errorf("local terminal context is unavailable; reconnect Stealthbox: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return snapshot, fmt.Errorf("local terminal context: HTTP %d; update and reconnect Stealthbox", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, localContextLimit+1))
	if err != nil || len(data) > localContextLimit {
		return snapshot, fmt.Errorf("local terminal context exceeds its limit or could not be read")
	}
	if err = json.Unmarshal(data, &snapshot); err != nil {
		return snapshot, fmt.Errorf("invalid local terminal context")
	}
	for name, value := range snapshot.Environment {
		if !transferableEnvironment(name) || strings.ContainsRune(value, 0) || len(name)+len(value) > 64<<10 {
			return localContextSnapshot{}, fmt.Errorf("invalid forwarded environment entry")
		}
	}
	return snapshot, nil
}

func contextEnvironment(ctx context.Context, c Config, configPath string) ([]string, error) {
	snapshot, err := forwardedContext(ctx, c)
	if err != nil {
		return nil, err
	}
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			environment[name] = value
		}
	}
	if localContextLaunch(c) {
		for _, name := range strings.Split(os.Getenv("STEALTHBOX_LOCAL_ENV_KEYS"), ",") {
			if transferableEnvironment(name) {
				delete(environment, name)
			}
		}
		if !remoteLocalContext(c) {
			delete(environment, "STEALTHBOX_LOCAL_ENV_KEYS")
			for name := range environment {
				if strings.HasPrefix(name, "GIT_CONFIG_") || name == "GIT_SSH_COMMAND" || name == "GIT_SSH_VARIANT" {
					delete(environment, name)
				}
			}
			if environment["SSH_AUTH_SOCK"] == credentialAgentSocket(c) {
				delete(environment, "SSH_AUTH_SOCK")
			}
		}
	}
	for name, value := range snapshot.Environment {
		environment[name] = value
	}
	if remoteLocalContext(c) {
		forwardedKeys := make([]string, 0, len(snapshot.Environment))
		for name := range snapshot.Environment {
			forwardedKeys = append(forwardedKeys, name)
		}
		sort.Strings(forwardedKeys)
		environment["STEALTHBOX_LOCAL_ENV_KEYS"] = strings.Join(forwardedKeys, ",")
		if snapshot.SSHAgent {
			if err = ensureAgentProxy(ctx, c, configPath); err != nil {
				return nil, err
			}
			environment["SSH_AUTH_SOCK"] = credentialAgentSocket(c)
		}
		// Reset helpers for this process only; the Mac's helpers provide credentials.
		environment["GIT_CONFIG_COUNT"] = "3"
		environment["GIT_CONFIG_KEY_0"] = "credential.helper"
		environment["GIT_CONFIG_VALUE_0"] = ""
		environment["GIT_CONFIG_KEY_1"] = "credential.helper"
		bin := filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox")
		environment["GIT_CONFIG_VALUE_1"] = "!" + shellArgs([]string{bin, "git-credential", "--config", configPath})
		environment["GIT_SSH_COMMAND"] = shellArgs([]string{bin, "git-ssh", "--config", configPath, "--"})
		environment["GIT_SSH_VARIANT"] = "ssh"
		environment["GIT_CONFIG_KEY_2"] = "credential.useHttpPath"
		environment["GIT_CONFIG_VALUE_2"] = "true"
	}
	keys := make([]string, 0, len(environment))
	for name := range environment {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, name := range keys {
		result = append(result, name+"="+environment[name])
	}
	return result, nil
}

func RunWithLocalContext(ctx context.Context, c Config, configPath string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("local context needs a command")
	}
	environment, err := contextEnvironment(ctx, c, configPath)
	if err != nil {
		return err
	}
	return ExecuteContext(ctx, Command{args[0], args[1:]}, stdin, stdout, stderr, environment)
}

type gitCredentialRequest struct {
	Protocol  string `json:"protocol"`
	Host      string `json:"host"`
	Path      string `json:"path,omitempty"`
	Username  string `json:"username,omitempty"`
	Directory string `json:"directory,omitempty"`
}

func (request gitCredentialRequest) input() (string, error) {
	if request.Protocol != "http" && request.Protocol != "https" || request.Host == "" {
		return "", fmt.Errorf("Git credential request requires an HTTP(S) host")
	}
	fields := [][2]string{{"protocol", request.Protocol}, {"host", request.Host}, {"path", request.Path}, {"username", request.Username}}
	var input strings.Builder
	for _, field := range fields {
		if strings.ContainsAny(field[1], "\x00\r\n") {
			return "", fmt.Errorf("invalid Git credential field")
		}
		if field[1] != "" {
			fmt.Fprintf(&input, "%s=%s\n", field[0], field[1])
		}
	}
	input.WriteByte('\n')
	return input.String(), nil
}

func macGitCredential(ctx context.Context, c Config, request gitCredentialRequest) ([]byte, error) {
	input, err := request.input()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "git", "credential", "fill")
	if c.Workspace.LocalRoot != "" && request.Directory != "" {
		if request.Directory == "." {
			command.Dir = c.Workspace.LocalRoot
		} else {
			command.Dir, err = checkedSourcePath(c.Workspace.LocalRoot, request.Directory)
			if err != nil {
				return nil, fmt.Errorf("Git credential directory is outside the local workspace")
			}
		}
	}
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	// Some checkouts use an authenticated URL instead of a credential helper.
	// The imported URL is sanitized, so resolve its credential privately here.
	if command.Dir != "" && request.Path != "" {
		remotes := exec.CommandContext(ctx, "git", "config", "--get-regexp", `^remote\..*\.(url|pushurl)$`)
		remotes.Dir, remotes.Env = command.Dir, command.Env
		if values, e := remotes.Output(); e == nil {
			for _, line := range strings.Split(string(values), "\n") {
				_, raw, ok := strings.Cut(line, " ")
				u, e := url.Parse(raw)
				if !ok || e != nil || u.User == nil || u.Scheme != request.Protocol || !strings.EqualFold(u.Host, request.Host) || strings.TrimPrefix(u.Path, "/") != request.Path {
					continue
				}
				password, present := u.User.Password()
				username := u.User.Username()
				if present && !strings.ContainsAny(username+password, "\x00\n\r") {
					return []byte("username=" + username + "\npassword=" + password + "\n\n"), nil
				}
			}
		}
	}
	command.Stdin = strings.NewReader(input)
	output, err := command.Output()
	if err == nil && len(output) <= 64<<10 {
		return output, nil
	}
	// gh keeps credentials independently from Git's helper configuration.
	// Its native Git helper resolves the requested host without copying its
	// credential store or guessing which account to use.
	helper := exec.CommandContext(ctx, "gh", "auth", "git-credential", "get")
	helper.Dir, helper.Env, helper.Stdin = command.Dir, command.Env, strings.NewReader(input)
	output, err = helper.Output()
	if err == nil && len(output) <= 64<<10 && strings.Contains(string(output), "password=") {
		return output, nil
	}
	// glab's stored token is selected by the exact requested host. Do not
	// allow a generic CI/exported token to be reused for an unrelated host.
	glab := exec.CommandContext(ctx, "glab", "config", "get", "token", "--host", request.Host, "--global")
	for _, entry := range command.Env {
		name, _, _ := strings.Cut(entry, "=")
		if name != "GITLAB_TOKEN" && name != "GITLAB_ACCESS_TOKEN" && name != "OAUTH_TOKEN" && name != "CI_JOB_TOKEN" {
			glab.Env = append(glab.Env, entry)
		}
	}
	output, err = glab.Output()
	token := strings.TrimSpace(string(output))
	if err == nil && token != "" && len(output) <= 64<<10 && !strings.ContainsAny(token, "\x00\n\r") {
		return []byte("username=oauth2\npassword=" + token + "\n\n"), nil
	}
	// Helper diagnostics can contain tokens or usernames; do not relay them.
	return nil, fmt.Errorf("the Mac has no available Git credential for this request")
}

func serveGitCredential(w http.ResponseWriter, r *http.Request, c Config) {
	var request gitCredentialRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid Git credential request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	credential, err := macGitCredential(ctx, c, request)
	if err != nil {
		http.Error(w, "Git credential is unavailable on the Mac", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(credential)
}

// Git invokes this helper on the VM; store/erase never change the Mac's keychain.
func GitCredential(ctx context.Context, c Config, action string, stdin io.Reader, stdout io.Writer) error {
	if action != "get" || !remoteLocalContext(c) {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return fmt.Errorf("Git credential request exceeds its limit")
	}
	fields := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			fields[name] = value
		}
	}
	if err = scanner.Err(); err != nil {
		return fmt.Errorf("invalid Git credential request")
	}
	request := gitCredentialRequest{Protocol: fields["protocol"], Host: fields["host"], Path: fields["path"], Username: fields["username"]}
	if _, err = request.input(); err != nil {
		return err
	}
	if c.Workspace.VMRoot != "" {
		root, rootErr := filepath.EvalSymlinks(c.Workspace.VMRoot)
		if directory, e := os.Getwd(); rootErr == nil && e == nil && within(root, directory) {
			request.Directory, _ = filepath.Rel(root, directory)
		}
	}
	body, _ := json.Marshal(request)
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probe, "POST", "http://unix/git-credential", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	response, err := bridgeHTTP(c.Bridge.Socket).Do(req)
	if err != nil {
		return fmt.Errorf("Git credential bridge is unavailable; reconnect Stealthbox")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil // Let Git report its normal authentication failure.
	}
	credential, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(credential) > 64<<10 {
		return fmt.Errorf("invalid Git credential response")
	}
	_, err = stdout.Write(credential)
	return err
}
