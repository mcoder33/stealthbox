package stealthbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// PrepareBridgeSocket only removes a closed socket in the private VM state
// directory. An active listener, unknown owner, symlink or kernel socket entry
// is preserved. Linux sshd can leave this file behind after -R disconnects.
func PrepareBridgeSocket(ctx context.Context, c Config) error {
	path := c.Bridge.RemoteSocket
	root := c.Workspace.RemoteDir
	if path == "" || c.Bridge.Socket != path || !filepath.IsAbs(root) || !filepath.IsAbs(path) || !within(root, path) {
		return fmt.Errorf("bridge socket cleanup requires the VM state directory")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || int(identity.Uid) != os.Geteuid() {
		return fmt.Errorf("bridge socket has an unverified owner or type; nothing removed")
	}
	for _, dir := range []string{root, filepath.Dir(path)} {
		canonical, e := filepath.EvalSymlinks(dir)
		if e != nil || canonical != filepath.Clean(dir) {
			return fmt.Errorf("bridge state directory is not canonical; nothing removed")
		}
		entry, e := os.Stat(dir)
		if e != nil {
			return e
		}
		owner, ok := entry.Sys().(*syscall.Stat_t)
		if !ok || !entry.IsDir() || int(owner.Uid) != os.Geteuid() || entry.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("bridge state directory is not private; nothing removed")
		}
	}
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
	if err == nil {
		connection.Close()
		return nil
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("bridge socket cannot be inspected safely; nothing removed")
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("stale bridge socket requires manual inspection on this platform")
	}
	sockets, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return fmt.Errorf("cannot verify bridge socket kernel ownership; nothing removed")
	}
	for _, line := range strings.Split(string(sockets), "\n") {
		if strings.HasSuffix(line, " "+path) || strings.HasSuffix(line, "\t"+path) {
			return fmt.Errorf("bridge socket still has a kernel owner; nothing removed")
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return fmt.Errorf("bridge socket changed during inspection; nothing removed")
	}
	return os.Remove(path)
}
