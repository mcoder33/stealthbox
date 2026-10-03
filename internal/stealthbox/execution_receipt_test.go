//go:build !windows

package stealthbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func receiptFixture(t *testing.T) Config {
	t.Helper()
	c := rootConfig(t)
	c.Bridge.AllowExec = true
	root, err := os.MkdirTemp("/tmp", "sb-receipt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	c.Bridge.Socket = filepath.Join(root, "receipt.sock")
	c.Workspace.SyncTransport = "archive"
	for _, path := range []string{"a/api", "b/api"} {
		checkout := createCheckout(t, c, path, path)
		if err := os.Mkdir(filepath.Join(checkout, "tests"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- ServeBridge(ctx, c, ready) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("private receipt bridge did not start")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return c
}

func decodeReceipt(t *testing.T, text string) map[string]string {
	t.Helper()
	const prefix = "[execution] "
	if strings.Count(text, prefix) != 1 {
		t.Fatalf("want one execution receipt, got %q", text)
	}
	line := strings.SplitN(strings.SplitN(text, prefix, 2)[1], "\n", 2)[0]
	var receipt map[string]string
	if err := json.Unmarshal([]byte(line), &receipt); err != nil {
		t.Fatal("invalid receipt", err, text)
	}
	return receipt
}

func TestSixMixedRunsHaveActualReceiptsAndUnchangedStdout(t *testing.T) {
	c := receiptFixture(t)
	for _, path := range []string{"a/api", "b/api"} {
		for _, execution := range []struct {
			runner, sync string
			snapshot     bool
		}{{"vm", "not-needed", true}, {"local", "completed", true}, {"mac", "not-requested", false}} {
			var stdout, stderr bytes.Buffer
			if err := Run(context.Background(), c, path+"/tests", execution.runner, []string{"printf", "payload"}, execution.snapshot, "", &stdout, &stderr); err != nil {
				t.Fatal(err, stderr.String())
			}
			if stdout.String() != "payload" {
				t.Fatal("receipt contaminated stdout", stdout.String())
			}
			receipt := decodeReceipt(t, stderr.String())
			resolved, err := ResolveProject(context.Background(), c, path, "")
			if err != nil {
				t.Fatal(err)
			}
			runner := normalizeRunner(execution.runner)
			wantCWD := filepath.Join(resolved.Project.Runners[runner].Path, "tests")
			if receipt["checkout"] != resolved.SourcePath || receipt["runner"] != runner || receipt["cwd"] != wantCWD || receipt["sync"] != execution.sync {
				t.Fatal("receipt describes requested selector instead of actual execution", receipt, path, execution, wantCWD)
			}
			if execution.sync == "completed" && receipt["sync_transport"] != "archive" {
				t.Fatal("successful archive transfer not identified", receipt)
			}
		}
	}
}

func TestMCPMixedRunsIncludeExecutionReceipts(t *testing.T) {
	c := receiptFixture(t)
	t.Setenv("STEALTHBOX_SCOPE", "workspace")
	t.Setenv("STEALTHBOX_PROJECT", "")
	var input strings.Builder
	for index, path := range []string{"a/api", "b/api"} {
		for offset, tool := range []string{"vm_exec", "runner_run", "mac_exec"} {
			request := map[string]any{"jsonrpc": "2.0", "id": index*3 + offset + 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": map[string]any{"path": path + "/tests", "runner": "local", "argv": []string{"printf", "payload"}}}}
			data, _ := json.Marshal(request)
			fmt.Fprintln(&input, string(data))
		}
	}
	var output bytes.Buffer
	if err := ServeMCP(context.Background(), c, strings.NewReader(input.String()), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 6 {
		t.Fatal("missing MCP results", output.String())
	}
	for index, line := range lines {
		var response struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
				IsError bool                    `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &response); err != nil || response.Result.IsError || len(response.Result.Content) != 1 {
			t.Fatal("MCP run failed", line, err)
		}
		text := response.Result.Content[0].Text
		receipt := decodeReceipt(t, text)
		path := "a/api"
		if index >= 3 {
			path = "b/api"
		}
		if !strings.Contains(text, "payload") || receipt["checkout"] != filepath.Join(c.Workspace.VMRoot, path) || !strings.HasSuffix(receipt["cwd"], "/tests") {
			t.Fatal("MCP result cannot be attributed", text)
		}
	}
}

func TestBridgeDoesNotClaimSuccessfulSyncAfterTransferFailure(t *testing.T) {
	c := bridgeConfig(t)
	request := RunRequest{Project: "test", Sync: true, Transport: "archive", Args: []string{"printf", "must-not-run"}}
	metadata, _ := json.Marshal(request)
	r := httptest.NewRequest("POST", "/run", strings.NewReader("invalid archive"))
	r.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	r.Header.Set("X-Stealthbox-Request", base64.RawURLEncoding.EncodeToString(metadata))
	w := httptest.NewRecorder()
	BridgeHandler(c).ServeHTTP(w, r)
	if w.Code == 200 || strings.Contains(w.Body.String(), "[execution]") || strings.Contains(w.Body.String(), "must-not-run") {
		t.Fatal("failed transfer received a successful execution receipt", w.Code, w.Body.String())
	}
}

func TestExecutionReceiptUsesPhysicalCheckoutAndCWD(t *testing.T) {
	c, _ := DefaultConfig()
	checkout := canonicalTemp(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(checkout, alias); err != nil {
		t.Fatal(err)
	}
	c.Projects = map[string]Project{"fixture": {Source: Endpoint{Path: alias}, Runners: map[string]Endpoint{"vm": {Path: alias}}}}
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), c, "fixture", "vm", []string{"pwd", "-P"}, false, "", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	receipt := decodeReceipt(t, stderr.String())
	if stdout.String() != checkout+"\n" || receipt["checkout"] != checkout || receipt["cwd"] != checkout {
		t.Fatal("receipt did not resolve physical execution paths", receipt, stdout.String(), checkout)
	}
}
