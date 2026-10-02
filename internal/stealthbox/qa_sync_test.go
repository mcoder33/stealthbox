package stealthbox

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerSyncPreservesPrivateQAState(t *testing.T) {
	for _, transport := range []string{"archive", "rsync"} {
		t.Run(transport, func(t *testing.T) {
			if transport == "rsync" {
				if _, err := exec.LookPath("rsync"); err != nil {
					t.Skip("rsync unavailable")
				}
			}
			source := canonicalTemp(t)
			runner := filepath.Join(canonicalTemp(t), "runner")
			if err := validateRunnerRoot(runner); err != nil {
				t.Fatal(err)
			}
			write := func(root, name, content string) {
				t.Helper()
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(source, "code.php", "first")
			write(source, "stale.php", "remove on next sync")
			write(source, ".stealthbox-qa/reports/common.xml", "conflicting source report")
			write(source, ".stealthbox-qa/source-only", "must never be imported")
			write(runner, ".stealthbox-qa/reports/common.xml", "local QA proof")
			write(runner, ".stealthbox-qa/compose.env", "local QA stack state")
			reportPath := filepath.Join(runner, ".stealthbox-qa/reports/common.xml")
			reportBefore, err := os.Stat(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			sync := func() {
				t.Helper()
				if transport == "archive" {
					if err := SyncSnapshot(runner, snapshotFor(t, source), 1<<20); err != nil {
						t.Fatal(err)
					}
					return
				}
				var stderr strings.Builder
				if err := ExecuteContext(context.Background(), runnerRsyncCommand(Endpoint{Path: source}, Endpoint{Path: runner}), nil, io.Discard, &stderr); err != nil {
					t.Fatal(err, stderr.String())
				}
			}
			sync()
			if content, err := os.ReadFile(filepath.Join(runner, "stale.php")); err != nil || string(content) != "remove on next sync" {
				t.Fatal("initial source code not transferred", string(content), err)
			}
			write(source, "code.php", "second")
			write(source, ".stealthbox-qa/reports/common.xml", "new conflicting source report")
			if err := os.Remove(filepath.Join(source, "stale.php")); err != nil {
				t.Fatal(err)
			}
			for round := 0; round < 2; round++ {
				sync()
				for name, expected := range map[string]string{
					"code.php":                          "second",
					".stealthbox-qa/reports/common.xml": "local QA proof",
					".stealthbox-qa/compose.env":        "local QA stack state",
				} {
					if content, err := os.ReadFile(filepath.Join(runner, name)); err != nil || string(content) != expected {
						t.Fatal("runner content changed/lost", round, name, string(content), err)
					}
				}
				for _, name := range []string{"stale.php", ".stealthbox-qa/source-only"} {
					if _, err := os.Lstat(filepath.Join(runner, name)); !os.IsNotExist(err) {
						t.Fatal("stale code or source QA state copied", round, name, err)
					}
				}
				reportAfter, err := os.Stat(reportPath)
				if err != nil || !os.SameFile(reportBefore, reportAfter) || !reportBefore.ModTime().Equal(reportAfter.ModTime()) {
					t.Fatal("local QA report was replaced or rewritten", round, err)
				}
			}
		})
	}
}

func TestArchiveRejectsInjectedPrivateQAState(t *testing.T) {
	runner := filepath.Join(canonicalTemp(t), "runner")
	if err := validateRunnerRoot(runner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runner, "code.php"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: ".stealthbox-qa/compose.env", Typeflag: tar.TypeReg, Size: 1, Mode: 0600}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := SyncSnapshot(runner, &archive, 1<<20); err == nil {
		t.Fatal("injected QA state archive accepted")
	}
	if content, err := os.ReadFile(filepath.Join(runner, "code.php")); err != nil || string(content) != "unchanged" {
		t.Fatal("rejected archive changed runner code", err)
	}
}
