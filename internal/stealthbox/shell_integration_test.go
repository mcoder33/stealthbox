package stealthbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRootWorkspaceLaunchClearsStaticBinding(t *testing.T) {
	c := sourceTestConfig(t)
	c.Bridge.Enabled = true
	c.Workspace.RemoteDir = filepath.Join(t.TempDir(), "state")
	launch, err := resolveWorkspaceLaunch(context.Background(), c, "", "shell", "local")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(c.Workspace.VMRoot)
	if launch.scope != "workspace" || launch.binding != "" || launch.directory != root {
		t.Fatal(launch)
	}
	script, err := workspaceLaunchScript(c, "/tmp/config.json", launch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "STEALTHBOX_PROJECT=''") || !strings.Contains(script, "STEALTHBOX_SCOPE='workspace'") {
		t.Fatal(script)
	}
}

func TestRootAgentUsesExternalExecutableAndWorkspaceScope(t *testing.T) {
	c := sourceTestConfig(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "agent-log")
	agent := filepath.Join(dir, "codex-real")
	program := "#!/bin/sh\nprintf '%s\\n' \"$STEALTHBOX_SCOPE|$STEALTHBOX_PROJECT|$PWD\" >> " + Quote(log) + "\nprintf '%s\\n' \"$@\" >> " + Quote(log) + "\n"
	if err := os.WriteFile(agent, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	c.Agents["codex"] = []string{agent}
	t.Setenv("STEALTHBOX_PROJECT", "stale-static-binding")
	t.Setenv("STEALTHBOX_SCOPE", "static")
	if err := LaunchAgent(context.Background(), c, "/tmp/config.json", c.Workspace.VMRoot, "codex", "vm", []string{"argument with spaces"}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	actual := string(b)
	if strings.Count(actual, "workspace||") != 1 || !strings.Contains(actual, "mcp_servers.stealthbox.env.STEALTHBOX_SCOPE=\"workspace\"") || !strings.Contains(actual, "argument with spaces") {
		t.Fatal(actual)
	}
}

func TestManagedWrapperCallsRealAgentOnce(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	program := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + Quote(log) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "stealthbox"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", shellAgentFunctions()+"\ncodex 'argument with spaces' '$literal'\n")
	command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "STEALTHBOX_RUNNER=local")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	actual, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	expected := "agent\ncodex\n--\nargument with spaces\n$literal\n"
	if string(actual) != expected {
		t.Fatalf("wrapper argv or number of calls differs: %q", actual)
	}
}

func TestManagedShellDoesNotModifyUserStartupFiles(t *testing.T) {
	c := sourceTestConfig(t)
	c.Workspace.RemoteDir = filepath.Join(t.TempDir(), "state")
	home := t.TempDir()
	startup := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(startup, []byte("export USER_STYLE=kept\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", home)
	command, err := managedShellCommand(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(command, " "), "ZDOTDIR="+filepath.Join(c.Workspace.RemoteDir, "shell")) {
		t.Fatal(command)
	}
	content, _ := os.ReadFile(startup)
	if string(content) != "export USER_STYLE=kept\n" {
		t.Fatal("changed user startup file")
	}
	rc, _ := os.ReadFile(filepath.Join(c.Workspace.RemoteDir, "shell/.zshrc"))
	if !strings.Contains(string(rc), "$ZDOTDIR/.zshrc") || !strings.Contains(string(rc), "stealthbox agent codex") {
		t.Fatal(string(rc))
	}
}

func TestManagedZshPreservesStartupPathAndOverridesAliases(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh unavailable")
	}
	c := sourceTestConfig(t)
	c.Workspace.RemoteDir = filepath.Join(t.TempDir(), "state")
	home := t.TempDir()
	original := filepath.Join(home, "config-zsh")
	if err = os.MkdirAll(original, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, ".zshenv"), []byte("export ZDOTDIR="+Quote(original)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(original, ".zshrc"), []byte("alias codex='echo old-alias'\nexport USER_STYLE=kept\nexport PATH=/user-custom-tools:$PATH\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", zsh)
	t.Setenv("ZDOTDIR", home)
	argv, err := managedShellCommand(c, []string{"-i", "-c", "printf '%s\\n' \"$USER_STYLE\" \"$PATH\"; whence -w codex"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(b))
	}
	if !strings.Contains(string(b), "kept") || !strings.Contains(string(b), "/user-custom-tools:") || !strings.Contains(string(b), "codex: function") {
		t.Fatal(string(b))
	}
}

func TestRunningAgentRetainsCapturedRunnerAfterDefaultChanges(t *testing.T) {
	c := sourceTestConfig(t)
	c.Bridge.Enabled = true
	c.Workspace.Runner = "vm"
	c.Workspace.RemoteDir = t.TempDir()
	config := filepath.Join(c.Workspace.RemoteDir, "config.json")
	ready, finish, result := filepath.Join(c.Workspace.RemoteDir, "ready"), filepath.Join(c.Workspace.RemoteDir, "finish"), filepath.Join(c.Workspace.RemoteDir, "result")
	agent := filepath.Join(c.Workspace.RemoteDir, "codex-fixture")
	program := "#!/bin/sh\nprintf '%s|%s|%s\\n' \"$STEALTHBOX_RUNNER\" \"$STEALTHBOX_RUNNER_SOURCE\" \"$$\" > " + Quote(ready) + "\nwhile [ ! -f " + Quote(finish) + " ]; do sleep 0.02; done\nprintf '%s|%s|%s\\n' \"$STEALTHBOX_RUNNER\" \"$STEALTHBOX_RUNNER_SOURCE\" \"$$\" > " + Quote(result) + "\n"
	if err := os.WriteFile(agent, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	c.Agents["codex"] = []string{agent}
	if err := Save(config, c); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func(initialConfig Config) {
		done <- LaunchAgent(ctx, initialConfig, config, initialConfig.Workspace.VMRoot, "codex", "", nil, &strings.Builder{}, &strings.Builder{})
	}(c)
	defer os.WriteFile(finish, nil, 0600)
	waitForFixtureFile(t, ready)
	before, err := os.ReadFile(ready)
	if err != nil || !strings.HasPrefix(string(before), "vm|captured|") {
		t.Fatal("agent did not capture initial runner", err)
	}
	c.Workspace.Runner = "mac"
	if err := Save(config, c); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finish, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(result)
	if err != nil || string(after) != string(before) {
		t.Fatal("running agent changed runner or PID", err)
	}
}
