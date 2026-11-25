package stealthbox

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func verifyClaudePeerPID(fd, pid int) error {
	peer, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return err
	}
	if int(peer.Pid) != pid || peer.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("Claude peer process does not match the selected session")
	}
	return nil
}
