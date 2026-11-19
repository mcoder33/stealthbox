//go:build !linux

package stealthbox

import "context"

// Automatic cleanup relies on the Linux kernel's Unix socket table.
func RecoverRemoteSocket(context.Context, string) error { return nil }
