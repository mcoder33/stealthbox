package stealthbox

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func verifyClaudePeerPID(fd, pid int) error {
	peer, err := unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	if err != nil {
		return err
	}
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return err
	}
	if peer != pid || cred.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("Claude peer process does not match the selected session")
	}
	return nil
}
