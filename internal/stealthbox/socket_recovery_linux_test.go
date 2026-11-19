package stealthbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoverRemoteSocket(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bridge.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.SetUnlinkOnClose(false)
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	recover := func() {
		t.Helper()
		if err := RecoverRemoteSocket(context.Background(), path); err != nil {
			t.Fatal(err)
		}
	}
	recover()
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("lost live listener", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	recover()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("abandoned socket remains", err)
	}
	recover()
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	recover()
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
		t.Fatal("changed regular file", err)
	}
}
