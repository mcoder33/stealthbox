package stealthbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoteBridgeReadinessAllowsSlowSSH(t *testing.T) {
	root := localDeploymentSSH(t)
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte("#!/bin/sh\nexec sleep 2.3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := Config{}
	c.Workspace.Host = "fixture-vm"
	c.Workspace.RemoteDir = root
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := WaitRemoteBridge(ctx, c); err != nil {
		t.Fatal("healthy bridge with a slow SSH handshake was rejected:", err)
	}
}

func TestRemoteBridgeReadinessHonorsCancellation(t *testing.T) {
	root := localDeploymentSSH(t)
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := Config{}
	c.Workspace.Host = "fixture-vm"
	c.Workspace.RemoteDir = root
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := WaitRemoteBridge(ctx, c); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("caller deadline was not preserved:", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatal("canceled SSH probe kept readiness waiting:", elapsed)
	}
}
