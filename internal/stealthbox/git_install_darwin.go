package stealthbox

import "golang.org/x/sys/unix"

func installGitDirectory(source, destination string) error {
	return unix.RenamexNp(source, destination, unix.RENAME_EXCL)
}
