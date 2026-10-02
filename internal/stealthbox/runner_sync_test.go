package stealthbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRsyncSelectedSizeFormats(t *testing.T) {
	for _, stats := range []string{"Total file size: 26,259 bytes\n", "Total file size: 26259 B\n", "Total file size: 26259\n"} {
		match := rsyncTotalSizeRE.FindStringSubmatch(stats)
		if len(match) != 2 || strings.ReplaceAll(match[1], ",", "") != "26259" {
			t.Fatal(stats, match)
		}
	}
	if rsyncTotalSizeRE.MatchString("Total transferred file size: 26259 B\n") {
		t.Fatal("matched transferred bytes instead of selected source size")
	}
}
func TestRsyncHonorsSSHConfigAndQuotesRemotePath(t *testing.T) {
	t.Setenv("STEALTHBOX_SSH_CONFIG", "/private/tmp/config with spaces")
	command := runnerRsyncCommand(Endpoint{Host: "vm", Path: "/home/dev/Projects/repo with spaces"}, Endpoint{Path: "/private/tmp/runner"})
	args := strings.Join(command.Args, " ")
	if !strings.Contains(args, "--checksum") || !strings.Contains(args, "--no-links") || !strings.Contains(args, "'config with spaces'") && !strings.Contains(args, "'/private/tmp/config with spaces'") {
		t.Fatal(args)
	}
	if !strings.Contains(args, "vm:'/home/dev/Projects/repo with spaces/'") {
		t.Fatal(args)
	}
}
func TestRsyncSameSizeMtimeEditAndDependencySymlinkPreservation(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	source := canonicalTemp(t)
	runner := filepath.Join(canonicalTemp(t), "runner")
	deps := canonicalTemp(t)
	if err := os.WriteFile(source+"/code", []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+"/stale", []byte("remove"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+"/.envrc", []byte("source-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source+"/.codex", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+"/.codex/auth.json", []byte("source-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source+"/code", source+"/source-link"); err != nil {
		t.Fatal(err)
	}
	if err := validateRunnerRoot(runner); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(deps, runner+"/vendor"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deps+"/cache", []byte("dependency-cache"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runner+"/.envrc", []byte("local-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	sync := func() {
		t.Helper()
		var stderr strings.Builder
		if err := ExecuteContext(context.Background(), runnerRsyncCommand(Endpoint{Path: source}, Endpoint{Path: runner}), nil, io.Discard, &stderr); err != nil {
			t.Fatal(err, stderr.String())
		}
	}
	sync()
	stat, err := os.Stat(source + "/code")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source+"/code", []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(source+"/code", stat.ModTime(), stat.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(source + "/stale"); err != nil {
		t.Fatal(err)
	}
	sync()
	if data, err := os.ReadFile(runner + "/code"); err != nil || string(data) != "other" {
		t.Fatal("same size/mtime edit lost", string(data), err)
	}
	if data, err := os.ReadFile(runner + "/.envrc"); err != nil || string(data) != "local-secret" {
		t.Fatal("local secret overwritten", err)
	}
	if data, err := os.ReadFile(runner + "/vendor/cache"); err != nil || string(data) != "dependency-cache" {
		t.Fatal("dependency symlink cache lost", err)
	}
	for _, path := range []string{"stale", "source-link", ".codex"} {
		if _, err := os.Lstat(runner + "/" + path); !os.IsNotExist(err) {
			t.Fatal("excluded/stale path retained", path, err)
		}
	}
}
func TestRunnerSyncCancellationKillsProcessGroup(t *testing.T) {
	root := canonicalTemp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := executeRunnerSync(ctx, Command{"sh", []string{"-c", "echo $$ > " + Quote(root+"/pid") + "; sleep 60"}}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("cancelled sync succeeded")
	}
	data, err := os.ReadFile(root + "/pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(-pid, 0); err == nil {
		t.Fatal("sync process group survived cancellation")
	}
}
