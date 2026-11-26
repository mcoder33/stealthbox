package stealthbox

import (
	"context"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandaloneSerenaClient(t *testing.T) {
	ctx := context.Background()
	upstream := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{Instructions: "Choose a worktree"})
	mcp.AddTool(upstream, &mcp.Tool{Name: "lookup"}, func(_ context.Context, _ *mcp.CallToolRequest, arg struct {
		Worktree string `json:"worktree"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: arg.Worktree}}, IsError: arg.Worktree == ""}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return upstream }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only" {
			http.Error(w, "missing bearer", 401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- serveSerenaClient(ctx, httpServer.URL, "test-only", serverSide) }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "consumer", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.InitializeResult().Instructions != "Choose a worktree" {
		t.Fatal("lost instructions")
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 1 || listed.Tools[0].Name != "lookup" {
		t.Fatalf("tools: %v, %v", listed, err)
	}
	for _, root := range []string{"tree-a", "tree-b", ""} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "lookup", Arguments: map[string]string{"worktree": root}})
		if err != nil || result.IsError != (root == "") || result.Content[0].(*mcp.TextContent).Text != root {
			t.Fatalf("call: %v, %v", result, err)
		}
	}
	session.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneRequiresLocalCatalog(t *testing.T) {
	workspace := WorkspaceConfig{VMRoot: t.TempDir(), Serena: &SerenaManifest{}}
	missing := filepath.Join(t.TempDir(), "missing.json")
	for _, w := range []WorkspaceConfig{
		{VMRoot: workspace.VMRoot},
		{Serena: workspace.Serena},
		{VMRoot: workspace.VMRoot, Serena: workspace.Serena, Host: "remote"},
	} {
		err := StandaloneSerena(context.Background(), Config{Workspace: w}, missing)
		if err == nil || !strings.Contains(err.Error(), "local workspace profile") {
			t.Fatalf("unexpected profile error: %v", err)
		}
	}
	if err := StandaloneSerena(context.Background(), Config{Workspace: workspace}, missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing configuration: %v", err)
	}
}
