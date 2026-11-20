package stealthbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

var forwardName = regexp.MustCompile("^sb-[0-9a-f]{16}$")

func newForwardName() string {
	var id [8]byte
	rand.Read(id[:])
	return "sb-" + hex.EncodeToString(id[:])
}

// The marker selects where authenticated requests are sent. Only trust a
// directory owned by this user with no group or other access.
func privateForwardDir(socket string) bool {
	info, err := os.Stat(filepath.Dir(socket))
	return err == nil && info.IsDir() && info.Mode().Perm()&0077 == 0 && info.Sys().(*syscall.Stat_t).Uid == uint32(os.Geteuid())
}

// A running VM agent resolves the current transport before each request.
func (c Config) bridgeClient() *http.Client {
	socket := c.Bridge.Socket
	if socket == c.Bridge.RemoteSocket && privateForwardDir(socket) {
		if name, err := os.Readlink(socket + ".active"); err == nil && forwardName.MatchString(name) {
			socket = filepath.Join(filepath.Dir(socket), name)
		}
	}
	return bridgeHTTP(socket)
}

// HoldBridgeForward runs inside the SSH session that owns this reverse forward.
func HoldBridgeForward(ctx context.Context, c Config, name string) error {
	if !forwardName.MatchString(name) || c.Bridge.RemoteSocket == "" || c.Bridge.Socket != c.Bridge.RemoteSocket {
		return fmt.Errorf("invalid VM bridge forward")
	}
	if !privateForwardDir(c.Bridge.Socket) {
		return fmt.Errorf("bridge socket directory must be private and owned by this user")
	}
	marker := c.Bridge.Socket + ".active"
	c.Bridge.Socket = filepath.Join(filepath.Dir(marker), name)
	info, err := os.Lstat(c.Bridge.Socket)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("forward is not a socket")
	}
	defer func() {
		if now, e := os.Lstat(c.Bridge.Socket); e == nil && os.SameFile(info, now) {
			_ = os.Remove(c.Bridge.Socket)
		}
	}()
	check := func() error {
		probe, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		_, err := BridgeStatus(probe, c)
		return err
	}
	if err = check(); err != nil {
		return err
	}
	// Publish atomically and leave occupied legacy sockets alone. When the
	// original path is free, previously started clients can use it as well.
	pending := c.Bridge.Socket + ".link"
	if err = os.Symlink(name, pending); err != nil {
		return err
	}
	defer os.Remove(pending)
	if err = os.Rename(pending, marker); err != nil {
		return err
	}
	if err = os.Symlink(filepath.Base(marker), c.Bridge.RemoteSocket); err != nil && !os.IsExist(err) {
		return err
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			active, err := os.Readlink(marker)
			if err != nil {
				return err
			}
			if active != name {
				return nil
			}
			if err = check(); err != nil {
				return err
			}
		}
	}
}
