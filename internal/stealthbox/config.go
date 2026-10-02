package stealthbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type WorkspaceConfig struct {
	VMRoot           string    `json:"vm_root,omitempty"`
	LocalRoot        string    `json:"local_root,omitempty"`
	RunnerRoot       string    `json:"runner_root,omitempty"`
	ID               string    `json:"id,omitempty"`
	ShellIntegration bool      `json:"shell_integration,omitempty"`
	SyncTransport    string    `json:"sync_transport,omitempty"`
	Host             string    `json:"host"`
	RemoteDir        string    `json:"remote_dir"`
	Name             string    `json:"name"`
	Theme            string    `json:"theme,omitempty"`
	Forwards         []Forward `json:"forwards,omitempty"`
}
type Forward struct {
	Local  int `json:"local"`
	Remote int `json:"remote"`
}
type BridgeConfig struct {
	Enabled        bool   `json:"enabled"`
	Socket         string `json:"socket"`
	RemoteSocket   string `json:"remote_socket"`
	Token          string `json:"token,omitempty"`
	AllowExec      bool   `json:"allow_exec"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	MaxBytes       int64  `json:"max_bytes,omitempty"`
}

func DefaultConfig() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return Config{}, err
	}
	return Config{Projects: map[string]Project{}, Workspace: WorkspaceConfig{Host: "dev-vm", Name: "stealthbox"}, Bridge: BridgeConfig{Socket: filepath.Join(home, ".config", "stealthbox", "mac.sock"), Token: hex.EncodeToString(token), TimeoutSeconds: 1800, MaxBytes: 1 << 30}, Agents: map[string][]string{"codex": {"codex"}, "claude": {"claude"}, "shell": {"sh", "-c", "exec \"${SHELL:-/bin/sh}\" -l"}}}, nil
}
func (c Config) Validate() error {
	if err := validateWorkspaceRoots(c); err != nil {
		return err
	}
	if c.Workspace.Host != "" && !hostRE.MatchString(c.Workspace.Host) {
		return fmt.Errorf("invalid workspace SSH alias")
	}
	if c.Workspace.Name != "" && !nameRE.MatchString(c.Workspace.Name) {
		return fmt.Errorf("invalid workspace name")
	}
	for _, p := range []string{c.Workspace.RemoteDir, c.Bridge.RemoteSocket} {
		if p != "" && (!filepath.IsAbs(p) || !remotePathRE.MatchString(p) || filepath.Clean(p) == "/") {
			return fmt.Errorf("invalid remote state path %q", p)
		}
	}
	if c.Bridge.Socket != "" && (!filepath.IsAbs(c.Bridge.Socket) || len(c.Bridge.Socket) > 100 || strings.ContainsAny(c.Bridge.Socket, "\n\r:")) {
		return fmt.Errorf("local socket must be an absolute path of at most 100 bytes without colon")
	}
	if c.Bridge.RemoteSocket != "" && len(c.Bridge.RemoteSocket) > 100 {
		return fmt.Errorf("remote socket path exceeds 100 bytes")
	}
	if c.Bridge.Enabled && (len(c.Bridge.Token) < 32 || c.Bridge.Socket == "") {
		return fmt.Errorf("enabled bridge needs a socket and a token of at least 32 characters")
	}
	if c.Bridge.TimeoutSeconds < 0 || c.Bridge.MaxBytes < 0 {
		return fmt.Errorf("bridge limits must not be negative")
	}
	for n, args := range c.Agents {
		if !nameRE.MatchString(n) || len(args) == 0 || args[0] == "" {
			return fmt.Errorf("invalid agent %q", n)
		}
		for _, a := range args {
			if strings.ContainsRune(a, 0) {
				return fmt.Errorf("agent contains NUL")
			}
		}
	}
	for _, f := range c.Workspace.Forwards {
		if f.Local < 1 || f.Local > 65535 || f.Remote < 1 || f.Remote > 65535 {
			return fmt.Errorf("forward ports must be between 1 and 65535")
		}
	}
	for n, p := range c.Projects {
		if !nameRE.MatchString(n) {
			return fmt.Errorf("invalid project name %q", n)
		}
		if err := ValidateEndpoint(p.Source); err != nil {
			return err
		}
		for name, e := range p.Runners {
			if !nameRE.MatchString(name) {
				return fmt.Errorf("invalid runner %q", name)
			}
			if err := ValidateEndpoint(e); err != nil {
				return err
			}
		}
		if p.Agent != "" && !nameRE.MatchString(p.Agent) {
			return fmt.Errorf("invalid agent for %s", n)
		}
		if p.Runner != "" {
			if _, ok := p.Runners[p.Runner]; !ok {
				return fmt.Errorf("default runner for %s is missing", n)
			}
		}
	}
	for _, p := range c.Projects {
		if dst, ok := p.Runners["mac"]; ok && p.Source.Host == "" && dst.Host == "" && pathsOverlap(p.Source.Path, dst.Path) {
			return fmt.Errorf("local source and runner directories must not overlap")
		}
	}
	var macRoots []string
	for _, p := range c.Projects {
		if e, ok := p.Runners["mac"]; ok {
			macRoots = append(macRoots, filepath.Clean(e.Path))
		}
	}
	for i, a := range macRoots {
		for _, b := range macRoots[i+1:] {
			if pathsOverlap(a, b) {
				return fmt.Errorf("Mac runner directories must not overlap: %s and %s", a, b)
			}
		}
	}
	return nil
}
func Save(path string, c Config) error {
	if err := EnsureWorkspaceIdentity(&c); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func Defaults(p Project, agent, runner string) (string, string) {
	if agent == "" {
		agent = p.Agent
	}
	if agent == "" {
		agent = "codex"
	}
	if runner == "" {
		runner = p.Runner
	}
	if runner == "" {
		runner = "vm"
	}
	return agent, runner
}
