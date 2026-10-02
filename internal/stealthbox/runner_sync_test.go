package stealthbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeat := root + "/heartbeat"
	childScript := "echo $$ > " + Quote(root+"/child.pid") + "; while :; do printf x >> " + Quote(heartbeat) + "; sleep 0.02; done"
	script := "echo $$ > " + Quote(root+"/pid") + "; sh -c " + Quote(childScript) + " & wait"
	done := make(chan error, 1)
	go func() { done <- executeRunnerSync(ctx, Command{"sh", []string{"-c", script}}, io.Discard, io.Discard) }()
	// Cancel only after both shell and child exist and the child has made progress.
	// A timeout before startup would not exercise cancellation of descendants.
	deadline := time.Now().Add(5 * time.Second)
	var parentPID, childPID int
	for time.Now().Before(deadline) {
		parentPID = readProcessFixturePID(root + "/pid")
		childPID = readProcessFixturePID(root + "/child.pid")
		data, err := os.ReadFile(heartbeat)
		if parentPID > 0 && childPID > 0 && err == nil && len(data) >= 3 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("sync exited before child readiness: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if parentPID > 0 {
		t.Cleanup(func() { _ = syscall.Kill(-parentPID, syscall.SIGKILL) })
	}
	data, err := os.ReadFile(heartbeat)
	if parentPID == 0 || childPID == 0 || parentPID == childPID || err != nil || len(data) < 3 {
		t.Fatal("parent and heartbeat child did not become ready", parentPID, childPID, err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled sync succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled sync remained blocked")
	}
	waitForTerminatedProcessGroup(t, parentPID)
	stopped, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(stopped) {
		t.Fatal("descendant heartbeat survived cancellation")
	}
}

func readProcessFixturePID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

type processGroupMember struct {
	pid   int
	state string
}

// Group existence alone is not liveness: SIGKILL leaves terminated children in
// Linux process groups until their new parent reaps them. Allow bounded reaping,
// but accept only absent processes or the explicitly dead states Z and X.
func waitForTerminatedProcessGroup(t *testing.T, group int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		members, err := processGroupMembers(group)
		if err != nil {
			t.Fatal("inspect cancelled process group:", err)
		}
		var live []processGroupMember
		for _, member := range members {
			if member.state != "Z" && member.state != "X" {
				live = append(live, member)
			}
		}
		if len(members) == 0 {
			return
		}
		if time.Now().After(deadline) {
			if len(live) > 0 {
				t.Fatalf("live descendants survived cancellation: %+v", live)
			}
			t.Logf("cancelled group contains only terminated processes awaiting reaping: %+v", members)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processGroupMembers(group int) ([]processGroupMember, error) {
	var members []processGroupMember
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid < 1 {
				continue
			}
			data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read process %d stat: %w", pid, err)
			}
			processGroup, state, err := linuxProcessGroupState(string(data))
			if err != nil {
				return nil, fmt.Errorf("parse process %d stat: %w", pid, err)
			}
			if processGroup == group {
				members = append(members, processGroupMember{pid: pid, state: state})
			}
		}
		return members, nil
	}
	// macOS has no /proc; ps exposes the same group and leading process state.
	output, err := exec.Command("ps", "-eo", "pid=,pgid=,stat=").Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 || len(fields[2]) == 0 {
			return nil, fmt.Errorf("invalid process listing")
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, err
		}
		processGroup, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		if processGroup == group {
			members = append(members, processGroupMember{pid: pid, state: fields[2][:1]})
		}
	}
	return members, nil
}

func linuxProcessGroupState(stat string) (group int, state string, err error) {
	// comm is enclosed in parentheses and can itself contain spaces and ')'.
	end := strings.LastIndex(stat, ")")
	if end < 0 {
		return 0, "", fmt.Errorf("process stat has no closing comm delimiter")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, "", fmt.Errorf("process stat lacks state/parent/group fields")
	}
	group, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, "", fmt.Errorf("invalid process group: %w", err)
	}
	return group, fields[0], nil
}

func TestLinuxProcessStateParserHandlesCommDelimiters(t *testing.T) {
	for _, state := range []string{"R", "S", "D", "T", "Z", "X"} {
		group, parsed, err := linuxProcessGroupState("123 (a (worker) name) " + state + " 1 456 0 0")
		if err != nil || group != 456 || parsed != state {
			t.Fatal(group, parsed, err)
		}
	}
	for _, stat := range []string{"no closing delimiter", "123 (cmd) Z", "123 (cmd) Z 1 invalid", "123 (cmd) alive 1 456"} {
		if _, _, err := linuxProcessGroupState(stat); err == nil {
			t.Fatal("invalid process stat accepted", stat)
		}
	}
}
