package stealthbox

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitSSHFullDuplexBinaryAndExitCode(t *testing.T) {
	c := contextTestConfig(t)
	bin := t.TempDir()
	// Emit binary data before reading stdin. Keeping stdin open after SSH
	// exits must not deadlock the response or turn a successful exit into 1.
	script := "#!/bin/sh\nprintf '\\377\\000\\376ready'\nhead -c 3\nprintf 'transport diagnostic' >&2\nexit 7\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_PROTOCOL", "version=2")
	startContextBridge(t, c, BridgeHandler(c))
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- GitSSH(ctx, c, []string{"-o", "SendEnv=GIT_PROTOCOL", "git@mac-alias", "git-upload-pack 'team/repo.git'"}, reader, stdout, stderr)
	}()
	if _, err := writer.Write([]byte{0x80, 0xff, 0x00}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		exit, ok := err.(*ExitError)
		if !ok || exit.Code != 7 {
			t.Fatal("lost SSH exit status", err)
		}
	case <-ctx.Done():
		t.Fatal("full duplex transport deadlocked")
	}
	wanted := append([]byte{0xff, 0x00, 0xfe}, []byte("ready")...)
	wanted = append(wanted, 0x80, 0xff, 0x00)
	if !bytes.Equal(stdout.Bytes(), wanted) {
		t.Fatalf("pack bytes corrupted: %x", stdout.Bytes())
	}
	if stderr.String() != "transport diagnostic" {
		t.Fatal("stderr mixed with Git pack", stderr.String())
	}
}

func TestGitSSHRejectsNonGitCommandsAndOptionInjection(t *testing.T) {
	for _, args := range [][]string{
		{"-o", "ProxyCommand=evil", "host", "git-upload-pack 'repo'"},
		{"-oProxyCommand=evil", "host", "git-upload-pack 'repo'"},
		{"-host", "git-upload-pack 'repo'"},
		{"host", "sh -c arbitrary"},
		{"host", "git-upload-pack 'repo'; arbitrary"},
		{"-p", "22;bad", "host", "git-upload-pack 'repo'"},
		{"host\nnext", "git-upload-pack 'repo'"},
	} {
		if err := (gitSSHRequest{Args: args}).validate(); err == nil {
			t.Fatal("accepted invalid request", strings.Join(args, " "))
		}
	}
	if err := (gitSSHRequest{Args: []string{"-p", "2222", "git@host", "git-receive-pack 'repo'"}}).validate(); err != nil {
		t.Fatal(err)
	}
	if err := (gitSSHRequest{Args: []string{"host", `git-upload-pack 'repo'\''s path'`}}).validate(); err != nil {
		t.Fatal("valid Git quoting rejected", err)
	}
}
