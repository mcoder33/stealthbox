package stealthbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type cancelArchiveCopy struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelArchiveCopy) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if w.Len() > 512 {
		w.cancel()
	}
	return n, err
}

func TestSnapshotContextCancelsDuringFileCopy(t *testing.T) {
	source := t.TempDir()
	file := bytes.Repeat([]byte("snapshot test data\n"), 1<<14)
	if err := os.WriteFile(filepath.Join(source, "input"), file, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancelArchiveCopy{cancel: cancel}
	if err := snapshotContext(ctx, source, writer, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot was not canceled during file copy: %v", err)
	}
	if writer.Len() <= 512 || writer.Len() >= len(file) {
		t.Fatalf("expected a partially copied archive payload, wrote %d bytes", writer.Len())
	}
}

type cancelArchiveAtEOF struct {
	*bytes.Buffer
	cancel context.CancelFunc
}

func (r cancelArchiveAtEOF) Read(p []byte) (int, error) {
	n, err := r.Buffer.Read(p)
	if r.Len() == 0 {
		r.cancel()
	}
	return n, err
}

func TestCanceledArchiveIsNotSwappedIntoRunner(t *testing.T) {
	source, runner := t.TempDir(), filepath.Join(canonicalTemp(t), "runner")
	if err := validateRunnerRoot(runner); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{source, runner} {
		if err := os.WriteFile(filepath.Join(root, "file"), []byte(root), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snapshot := cancelArchiveAtEOF{snapshotFor(t, source), cancel}
	if err := syncSnapshot(ctx, runner, snapshot, 1<<20, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot sync was not canceled before swap: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(runner, "file")); err != nil || string(contents) != runner {
		t.Fatalf("runner changed after cancellation: %q, %v", contents, err)
	}
}
