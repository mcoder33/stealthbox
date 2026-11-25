//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mcoder33/stealthbox/internal/stealthbox"
)

func TestSessionCLIThroughSSH(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "local.json")
	c := stealthbox.Config{Workspace: stealthbox.WorkspaceConfig{Host: "dialogue-vm", RemoteDir: dir}}
	if err := stealthbox.Save(config, c); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEALTHBOX_CONFIG", config)
	t.Setenv("STEALTHBOX_SSH_CONFIG", filepath.Join(dir, "ssh.config"))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TEST_SESSION_DIR", dir)

	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("ssh", `#!/bin/sh
set -eu
test "$1" = -T
test "$2" = -o
test "$3" = BatchMode=yes
test "$4" = -F
test "$5" = "$TEST_SESSION_DIR/ssh.config"
test "$6" = dialogue-vm
printf "VM banner on stderr\n" >&2
exec sh -c "$7"
`)
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	write("bin/stealthbox", `#!/bin/sh
set -eu
test "$1" = session-vm
action="$2"
shift 2
if [ "$action" = ask ]; then
	test "$1" = "codex:thread-id"
	shift
fi
test "$1" = --config
test "$2" = "$TEST_SESSION_DIR/config.json"
test "$3" = --timeout
case "$action" in
list)
	test "$4" = 20s
	printf '[{"agent":"codex","id":"thread-id","name":"First thread","cwd":"/vm/work"}]\n'
	;;
ask)
	test "$4" = 15s
	cat >> "$TEST_SESSION_DIR/requests.jsonl"
	case "${TEST_SESSION_MODE:-}" in
	disconnect) exit 255 ;;
	incomplete)
		printf '{"agent":"codex","id":"thread-id","answer":'
		exit 0
		;;
	esac
	printf '{"agent":"codex","id":"thread-id","answer":"answer from the VM"}\n'
	;;
esac
`)

	list, diagnostic, err := invokeSessionCLI(t, "", "list", "--timeout", "20s")
	if err != nil || !strings.Contains(diagnostic, "VM banner") || strings.Contains(list, "VM banner") {
		t.Fatalf("list err=%v, stdout=%q, stderr=%q", err, list, diagnostic)
	}
	var sessions []struct{ Agent, ID, Name, CWD string }
	if err := json.Unmarshal([]byte(list), &sessions); err != nil || len(sessions) != 1 ||
		sessions[0].Agent != "codex" || sessions[0].ID != "thread-id" || sessions[0].Name != "First thread" || sessions[0].CWD != "/vm/work" {
		t.Fatalf("unexpected list %q: %v", list, err)
	}

	messages := []string{"  Explain this: \"привет\" $(whoami)\nnext line\n", "Follow-up for the same session.\n"}
	for _, message := range messages {
		answer, stderr, err := invokeSessionCLI(t, message, "ask", "codex:thread-id", "--timeout", "15s")
		if err != nil || !strings.Contains(stderr, "VM banner") {
			t.Fatalf("ask failed: %v, stderr=%q", err, stderr)
		}
		var reply struct{ Agent, ID, Answer string }
		if err := json.Unmarshal([]byte(answer), &reply); err != nil ||
			reply.Agent != "codex" || reply.ID != "thread-id" || reply.Answer != "answer from the VM" {
			t.Fatalf("unexpected answer %q: %v", answer, err)
		}
	}

	for _, test := range []struct{ message, target string }{
		{"", "codex:thread-id"},
		{"invalid target", "codex:--help"},
		{"invalid agent", "unconfigured:thread-id"},
	} {
		answer, diagnostic, err := invokeSessionCLI(t, test.message, "ask", test.target)
		if err == nil || answer != "" || diagnostic != "" {
			t.Fatalf("invalid ask called SSH: target=%q, err=%v, stdout=%q, stderr=%q", test.target, err, answer, diagnostic)
		}
	}
	for _, mode := range []string{"disconnect", "incomplete"} {
		t.Setenv("TEST_SESSION_MODE", mode)
		message := "might be processed: " + mode + "\n"
		answer, _, err := invokeSessionCLI(t, message, "ask", "codex:thread-id", "--timeout", "15s")
		if !errors.Is(err, stealthbox.ErrSessionOutcomeUnknown) || answer != "" {
			t.Fatalf("%s err=%v, stdout=%q", mode, err, answer)
		}
		messages = append(messages, message)
	}
	file, err := os.Open(filepath.Join(dir, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	for _, message := range messages {
		var request struct{ Message string }
		if err := decoder.Decode(&request); err != nil || request.Message != message {
			t.Fatalf("request = %#v, error=%v, want %q", request, err, message)
		}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("ask replayed or sent invalid data: %v", err)
	}
}

func TestSessionCLIHelpDoesNotRequireAProfile(t *testing.T) {
	t.Setenv("STEALTHBOX_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	for _, args := range [][]string{
		nil, {"--help"}, {"list", "--help"}, {"ask", "--help"}, {"ask", "claude:session-id", "--help"},
	} {
		stdout, stderr, err := invokeSessionCLI(t, "", args...)
		if err != nil || stderr != "" || !strings.Contains(stdout, "session list [--timeout 30s]") ||
			!strings.Contains(stdout, "session ask AGENT:ID") {
			t.Errorf("session %v: error=%v, stdout=%q, stderr=%q", args, err, stdout, stderr)
		}
	}
}

func invokeSessionCLI(t *testing.T, message string, args ...string) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	messagePath := filepath.Join(dir, "input")
	if err := os.WriteFile(messagePath, []byte(message), 0600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(messagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.Create(filepath.Join(dir, "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	diagnostic, err := os.Create(filepath.Join(dir, "diagnostic"))
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	in, out, stderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = input, output, diagnostic
	defer func() { os.Stdin, os.Stdout, os.Stderr = in, out, stderr }()

	runErr := run(context.Background(), append([]string{"session"}, args...))
	stdoutBytes, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	stderrBytes, err := os.ReadFile(diagnostic.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(stdoutBytes), string(stderrBytes), runErr
}
