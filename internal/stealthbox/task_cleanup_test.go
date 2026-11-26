package stealthbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSuccessfulCommandStopsBackgroundWorker(t *testing.T) {
	c := bridgeConfig(t)
	marker := filepath.Join(c.Projects["test"].Runners["vm"].Path, "after")
	script := fmt.Sprintf("(sleep 0.4; echo survived > %s) </dev/null >/dev/null 2>&1 &", Quote(marker))
	if err := Run(context.Background(), c, "test", "vm", []string{"sh", "-c", script}, false, "", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	time.Sleep(650 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("background worker wrote after successful reply: %v", err)
	}
}
