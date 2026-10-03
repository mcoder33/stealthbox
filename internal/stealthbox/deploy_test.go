package stealthbox

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func localDeploymentSSH(t *testing.T) string {
	t.Helper()
	root := canonicalTemp(t)
	script := "#!/bin/sh\nwhile [ \"$#\" -gt 1 ]; do shift; done\nexec /bin/sh -c \"$1\"\n"
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TERM", "dumb")
	return root
}

type synchronizedUploadReader struct {
	reader  *bytes.Reader
	ready   chan<- struct{}
	release <-chan struct{}
	waited  bool
}

func (r *synchronizedUploadReader) Read(p []byte) (int, error) {
	if !r.waited && r.reader.Len() == 0 {
		r.waited = true
		r.ready <- struct{}{}
		<-r.release
	}
	return r.reader.Read(p)
}

func TestConcurrentUploadsAreAtomicAndIndependent(t *testing.T) {
	localDeploymentSSH(t)
	path := filepath.Join(canonicalTemp(t), "file with quote' and spaces")
	payloads := [][]byte{bytes.Repeat([]byte("first-content\n"), 10000), bytes.Repeat([]byte("second-content\n"), 10000)}
	ready, release, results := make(chan struct{}, 2), make(chan struct{}), make(chan error, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, payload := range payloads {
		go func(payload []byte) {
			results <- upload(ctx, "fixture-vm", path, &synchronizedUploadReader{reader: bytes.NewReader(payload), ready: ready, release: release})
		}(payload)
	}
	for range payloads {
		select {
		case <-ready:
		case <-ctx.Done():
			close(release)
			t.Fatal("concurrent uploads did not reach transfer barrier")
		}
	}
	// Both uploads have delivered bytes while still holding their stdin open.
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range payloads {
		if err := <-results; err != nil {
			t.Errorf("concurrent upload: %v", err)
		}
	}
	installed, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(installed, payloads[0]) && !bytes.Equal(installed, payloads[1]) {
		t.Fatal("upload installed mixed or incomplete bytes", err)
	}
	assertNoDeploymentTemps(t, path)
}

func TestFailedUploadCleansTemporaryFileAndPreservesDestination(t *testing.T) {
	bin := localDeploymentSSH(t)
	if err := os.WriteFile(filepath.Join(bin, "cat"), []byte("#!/bin/sh\nprintf partial\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(canonicalTemp(t), "destination")
	if err := os.WriteFile(path, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := upload(context.Background(), "fixture-vm", path, strings.NewReader("replacement")); err == nil {
		t.Fatal("failed transfer succeeded")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "previous" {
		t.Fatal("failed upload changed destination", err)
	}
	assertNoDeploymentTemps(t, path)
}

func TestConcurrentSetupCandidatesAndRejectedBinary(t *testing.T) {
	localDeploymentSSH(t)
	c := bridgeConfig(t)
	root, err := os.MkdirTemp("/tmp", "sb-deploy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	c.Workspace.Host = "fixture-vm"
	c.Workspace.RemoteDir = filepath.Join(root, "remote")
	c.Bridge.RemoteSocket = filepath.Join(c.Workspace.RemoteDir, "run", "mac.sock")
	c.Bridge.Socket = filepath.Join(root, "local.sock")
	project := c.Projects["test"]
	project.Source.Host = c.Workspace.Host
	project.Runners["vm"] = project.Source
	c.Projects["test"] = project
	barrier := filepath.Join(root, "handshakes")
	if err := os.Mkdir(barrier, 0700); err != nil {
		t.Fatal(err)
	}
	var binaries []string
	for _, label := range []string{"first", "second"} {
		path := filepath.Join(root, label)
		script := "#!/bin/sh\n# " + label + "\n: > " + Quote(barrier) + "/$$\nattempt=0\nwhile [ \"$attempt\" -lt 100 ]; do attempt=$((attempt + 1)); set -- " + Quote(barrier) + "/*; [ \"$#\" -ge 2 ] && break; sleep 0.02; done\nprintf '%s\\n' " + Quote(Handshake()) + "\n"
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		binaries = append(binaries, path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	results := make(chan error, len(binaries))
	var workers sync.WaitGroup
	for _, binary := range binaries {
		workers.Add(1)
		go func(binary string) {
			defer workers.Done()
			copyConfig := c
			results <- Setup(ctx, &copyConfig, binary+".json", binary, io.Discard)
		}(binary)
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent setup: %v", err)
		}
	}
	installedPath := filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox")
	installed, err := os.ReadFile(installedPath)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(binaries[0])
	second, _ := os.ReadFile(binaries[1])
	if !bytes.Equal(installed, first) && !bytes.Equal(installed, second) {
		t.Fatal("setup installed mixed bytes")
	}
	probe, err := Output(ctx, Command{installedPath, []string{"handshake"}})
	if err != nil || strings.TrimSpace(string(probe)) != Handshake() {
		t.Fatal("installed binary does not run", err)
	}
	assertNoDeploymentTemps(t, installedPath)
	invalid := filepath.Join(root, "invalid")
	if err := os.WriteFile(invalid, []byte("#!/bin/sh\nprintf wrong-version\\n\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Setup(ctx, &c, invalid+".json", invalid, io.Discard); err == nil {
		t.Fatal("invalid handshake was accepted")
	}
	if after, err := os.ReadFile(installedPath); err != nil || !bytes.Equal(after, installed) {
		t.Fatal("rejected candidate changed installed binary", err)
	}
	assertNoDeploymentTemps(t, installedPath)
}

func assertNoDeploymentTemps(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(path)+".") {
			t.Errorf("deployment left temporary file %s", entry.Name())
		}
	}
}
