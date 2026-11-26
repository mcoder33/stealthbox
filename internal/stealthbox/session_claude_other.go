//go:build !linux && !darwin

package stealthbox

import "errors"

func verifyClaudePeerPID(fd, pid int) error {
	return errors.New("Claude peer messaging requires Linux or macOS")
}
