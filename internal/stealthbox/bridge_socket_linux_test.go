package stealthbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareBridgeSocketOnlyRemovesVerifiedStaleSockets(t *testing.T) {
	for _, kind := range []string{"stale", "active", "file", "symlink", "public-parent"} {
		t.Run(kind, func(t *testing.T) {
			c := contextTestConfig(t)
			c.Bridge.Socket = filepath.Join(c.Workspace.RemoteDir, "run", "mac.sock")
			c.Bridge.RemoteSocket = c.Bridge.Socket
			if err := os.MkdirAll(filepath.Dir(c.Bridge.Socket), 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				os.WriteFile(c.Bridge.Socket, []byte("keep"), 0600)
			case "symlink":
				os.Symlink(filepath.Join(t.TempDir(), "other"), c.Bridge.Socket)
			default:
				l, err := net.ListenUnix("unix", &net.UnixAddr{Name: c.Bridge.Socket, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				l.SetUnlinkOnClose(false)
				defer l.Close()
				if kind != "active" {
					l.Close()
				}
				if kind == "public-parent" {
					os.Chmod(filepath.Dir(c.Bridge.Socket), 0755)
				}
			}
			err := PrepareBridgeSocket(context.Background(), c)
			_, exists := os.Lstat(c.Bridge.Socket)
			if kind == "stale" {
				if err != nil || !os.IsNotExist(exists) {
					t.Fatal("stale socket retained", err, exists)
				}
			} else {
				if exists != nil {
					t.Fatal("unverified or active entry removed", err, exists)
				}
				if kind != "active" && err == nil {
					t.Fatal("unverified socket accepted")
				}
			}
		})
	}
}

func TestPrepareBridgeSocketRefusesPathsOutsideStateDirectory(t *testing.T) {
	c := contextTestConfig(t)
	if err := PrepareBridgeSocket(context.Background(), c); err == nil {
		t.Fatal("outside path accepted")
	}
}
