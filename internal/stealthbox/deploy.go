package stealthbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func sshCommand(host, script string, tty bool) Command { return At(Endpoint{Host: host}, script, tty) }
func upload(ctx context.Context, host, path string, content io.Reader) error {
	script := "umask 077; mkdir -p " + Quote(filepath.Dir(path)) + " && cat > " + Quote(path+".tmp") + " && mv " + Quote(path+".tmp") + " " + Quote(path)
	var stderr bytes.Buffer
	if err := ExecuteContext(ctx, sshCommand(host, script, false), content, io.Discard, &stderr); err != nil {
		return fmt.Errorf("upload %s: %w: %s", filepath.Base(path), err, stderr.String())
	}
	return nil
}
func resolveRemote(ctx context.Context, c *Config) (string, string, error) {
	host := c.Workspace.Host
	if host == "" {
		for _, name := range Names(*c) {
			if c.Projects[name].Source.Host != "" {
				host = c.Projects[name].Source.Host
				break
			}
		}
	}
	if !hostRE.MatchString(host) {
		return "", "", fmt.Errorf("configure workspace.host with an SSH alias")
	}
	c.Workspace.Host = host
	out, err := Output(ctx, sshCommand(host, "uname -sm; printf '%s\\n' \"$HOME\"", false))
	if err != nil {
		return "", "", fmt.Errorf("SSH probe failed: %w (test ssh %s first)", err, host)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("unexpected SSH probe output; remove shell startup output")
	}
	parts := strings.Fields(lines[0])
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected remote platform")
	}
	goos := strings.ToLower(parts[0])
	arch := parts[1]
	switch arch {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported remote architecture: %s", arch)
	}
	if goos != "linux" && goos != "darwin" {
		return "", "", fmt.Errorf("unsupported remote OS: %s", goos)
	}
	if c.Workspace.RemoteDir == "" {
		c.Workspace.RemoteDir = filepath.Join(lines[1], ".stealthbox")
	}
	if c.Workspace.VMRoot == "~" || strings.HasPrefix(c.Workspace.VMRoot, "~/") {
		c.Workspace.VMRoot = filepath.Join(lines[1], strings.TrimPrefix(strings.TrimPrefix(c.Workspace.VMRoot, "~"), "/"))
	}
	if err = NormalizeLocalWorkspaceRoots(c); err != nil {
		return "", "", err
	}
	if c.Workspace.Name == "" {
		c.Workspace.Name = "stealthbox"
	}
	if c.Bridge.RemoteSocket == "" {
		c.Bridge.RemoteSocket = filepath.Join(c.Workspace.RemoteDir, "run", "mac.sock")
	}
	if err = c.Validate(); err != nil {
		return "", "", err
	}
	return goos, arch, nil
}
func remoteConfig(c Config) (Config, error) {
	if err := EnsureWorkspaceIdentity(&c); err != nil {
		return Config{}, err
	}
	out := c
	if WorkspaceRootEnabled(c) {
		out.Workspace.Host = ""
		if out.Workspace.SyncTransport == "" || out.Workspace.SyncTransport == "auto" {
			if c.Workspace.Host != "" {
				out.Workspace.SyncTransport = "rsync"
			}
		}
	}
	out.Projects = make(map[string]Project)
	for n, p := range c.Projects {
		if p.Source.Host != "" && p.Source.Host != c.Workspace.Host {
			return Config{}, fmt.Errorf("project %s uses a different VM; use a separate config for each VM", n)
		}
		q := p
		q.Source.Host = ""
		q.Runners = make(map[string]Endpoint)
		for name, e := range p.Runners {
			if name == "mac" {
				if e.Host != "" || e.Bridge {
					return Config{}, fmt.Errorf("local mac runner must have an empty host")
				}
				e.Bridge = true
			} else if e.Host == c.Workspace.Host {
				e.Host = ""
			}
			q.Runners[name] = e
		}
		out.Projects[n] = q
	}
	out.Bridge.Socket = c.Bridge.RemoteSocket
	return out, out.Validate()
}
func binaryFor(ctx context.Context, goos, arch string) (string, func(), error) {
	if runtime.GOOS == goos && runtime.GOARCH == arch {
		p, e := os.Executable()
		return p, func() {}, e
	}
	// A source checkout can bootstrap before a release exists; installed binaries use release assets.
	wd, _ := os.Getwd()
	executable, _ := os.Executable()
	visited := map[string]bool{}
	for _, start := range []string{wd, filepath.Dir(executable)} {
		for root := start; !visited[root]; root = filepath.Dir(root) {
			visited[root] = true
			data, e := os.ReadFile(filepath.Join(root, "go.mod"))
			if e == nil && strings.Contains(string(data), "module github.com/mcoder33/stealthbox") {
				dir, e := os.MkdirTemp("", "stealthbox-build-")
				if e != nil {
					return "", nil, e
				}
				out := filepath.Join(dir, "stealthbox")
				cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", "-X main.version="+BuildVersion, "-o", out, "./cmd/stealthbox")
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+arch, "CGO_ENABLED=0")
				b, e := cmd.CombinedOutput()
				if e != nil {
					os.RemoveAll(dir)
					return "", nil, fmt.Errorf("cross-build: %w: %s", e, b)
				}
				return out, func() { os.RemoveAll(dir) }, nil
			}
			if root == filepath.Dir(root) {
				break
			}
		}
	}
	return releaseBinary(ctx, goos, arch)
}
func releaseBinary(ctx context.Context, goos, arch string) (string, func(), error) {
	name := "stealthbox-" + goos + "-" + arch
	if !strings.HasPrefix(BuildVersion, "v") || strings.ContainsAny(BuildVersion, "/\\ \n\r") {
		return "", nil, fmt.Errorf("this development build needs a source checkout with Go or --binary pointing to a matching VM binary")
	}
	base := "https://github.com/mcoder33/stealthbox/releases/download/" + BuildVersion + "/"
	client := &http.Client{Timeout: 2 * time.Minute}
	fetch := func(asset string, limit int64) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+asset, nil)
		res, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("release download HTTP %d; pass --binary for offline setup", res.StatusCode)
		}
		b, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
		if int64(len(b)) > limit {
			return nil, fmt.Errorf("release asset exceeds limit")
		}
		return b, e
	}
	sums, err := fetch("checksums.txt", 1<<20)
	if err != nil {
		return "", nil, err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if len(want) != 64 {
		return "", nil, fmt.Errorf("binary is missing from release checksums")
	}
	b, err := fetch(name, 100<<20)
	if err != nil {
		return "", nil, err
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != want {
		return "", nil, fmt.Errorf("release checksum mismatch")
	}
	dir, err := os.MkdirTemp("", "stealthbox-release-")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, name)
	if err = os.WriteFile(path, b, 0700); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return path, func() { os.RemoveAll(dir) }, nil
}

// Setup prepares the workspace and its two standard agents. User credentials
// and source working copies are never implicitly copied or overwritten.
func Setup(ctx context.Context, c *Config, configPath, binary string, stdout io.Writer) error {
	goos, arch, err := resolveRemote(ctx, c)
	if err != nil {
		return err
	}
	if err = prepareRemoteTools(ctx, *c, stdout); err != nil {
		return err
	}
	if err = prepareWorkspaceTheme(ctx, c, configPath, stdout); err != nil {
		return err
	}
	if binary == "" {
		var clean func()
		binary, clean, err = binaryFor(ctx, goos, arch)
		if err != nil {
			return err
		}
		defer clean()
	}
	file, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer file.Close()
	remoteBin := filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox")
	candidate := remoteBin + ".candidate"
	if err = upload(ctx, c.Workspace.Host, candidate, file); err != nil {
		return err
	}
	if err = ExecuteContext(ctx, sshCommand(c.Workspace.Host, "chmod 700 "+Quote(candidate), false), nil, io.Discard, io.Discard); err != nil {
		return err
	}
	probe, err := Output(ctx, sshCommand(c.Workspace.Host, Quote(candidate)+" handshake", false))
	if err != nil || strings.TrimSpace(string(probe)) != Handshake() {
		return fmt.Errorf("VM binary does not match this Stealth Box version; supply a matching --binary (existing installation was kept)")
	}
	if err = ExecuteContext(ctx, sshCommand(c.Workspace.Host, "mv "+Quote(candidate)+" "+Quote(remoteBin)+" && umask 077 && mkdir -p "+Quote(filepath.Dir(c.Bridge.RemoteSocket)), false), nil, io.Discard, io.Discard); err != nil {
		return err
	}
	if WorkspaceRootEnabled(*c) {
		if err = ExecuteContext(ctx, sshCommand(c.Workspace.Host, "umask 077; mkdir -p "+Quote(c.Workspace.VMRoot), false), nil, io.Discard, io.Discard); err != nil {
			return err
		}
	}
	if err = DeployConfigWithOutput(ctx, *c, stdout); err != nil {
		return err
	}
	if err = prepareTerminfo(ctx, *c); err != nil {
		return err
	}
	if err = Save(configPath, *c); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Installed Stealth Box on %s (%s/%s).\n", c.Workspace.Host, goos, arch)
	return nil
}
func DeployConfig(ctx context.Context, c Config) error {
	return DeployConfigWithOutput(ctx, c, io.Discard)
}

func DeployConfigWithOutput(ctx context.Context, c Config, stdout io.Writer) error {
	remote, err := remoteConfig(c)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(remote, "", "  ")
	if err != nil {
		return err
	}
	if err = upload(ctx, c.Workspace.Host, filepath.Join(c.Workspace.RemoteDir, "config.json"), bytes.NewReader(b)); err != nil {
		return err
	}
	if c.Workspace.Theme != "" {
		b, err = os.ReadFile(c.Workspace.Theme)
		if err != nil {
			return err
		}
		candidate := filepath.Join(c.Workspace.RemoteDir, "tmux.source.conf")
		if err = upload(ctx, c.Workspace.Host, candidate, bytes.NewReader(b)); err != nil {
			return err
		}
		args := []string{filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox"), "theme-apply", "--config", filepath.Join(c.Workspace.RemoteDir, "config.json"), "--file", candidate}
		if err = ExecuteContext(ctx, sshCommand(c.Workspace.Host, shellArgs(args), false), nil, stdout, stdout); err != nil {
			return fmt.Errorf("validate remote tmux style: %w", err)
		}
	}
	guide := []byte(Guide("configured project", "selected runner") + "\n")
	return upload(ctx, c.Workspace.Host, filepath.Join(c.Workspace.RemoteDir, "AGENT_GUIDE.md"), bytes.NewReader(guide))
}
