package stealthbox

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RecoverRemoteSocket clears an abandoned reverse forward in its owner's private directory.
func RecoverRemoteSocket(ctx context.Context, path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	parent, err := dir.Stat()
	if err != nil {
		return err
	}
	if !parent.IsDir() || parent.Mode().Perm()&0077 != 0 || parent.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil
	}
	// Serialize competing cleanup attempts until unlink is complete.
	if err := unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	original, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if original.Mode()&os.ModeSocket == 0 || original.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || time.Since(original.ModTime()) < 2*time.Second {
		return nil
	}
	// Preserve sockets that are bound but have not yet entered listen().
	table, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(table), "\n") {
		if strings.HasSuffix(line, " "+path) {
			return nil
		}
	}
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
	if err == nil {
		return conn.Close()
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if os.SameFile(original, current) {
		return os.Remove(path)
	}
	return nil
}
