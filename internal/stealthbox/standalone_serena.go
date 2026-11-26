package stealthbox

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// StandaloneSerena connects local stdio clients to the same per-profile service
// as managed agents. Each tool call selects its checkout under the workspace.
func StandaloneSerena(ctx context.Context, c Config, configPath string) error {
	if c.Workspace.Serena == nil || !WorkspaceRootEnabled(c) || c.Workspace.Host != "" {
		return errors.New("serena-stdio needs a local workspace profile with a Serena catalog")
	}
	url, token, err := ensureManagedMCP(ctx, c, configPath, MCPScope{Workspace: true, Runner: "vm"})
	if err != nil {
		return err
	}
	return serveSerenaClient(ctx, strings.TrimSuffix(url, "/mcp")+"/serena", token, &mcp.StdioTransport{})
}

func serveSerenaClient(ctx context.Context, url, token string, downstream mcp.Transport) error {
	impl := &mcp.Implementation{Name: "serena", Version: BuildVersion}
	localHTTP := managedMCPControlClient()
	localHTTP.Transport = &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}), Base: localHTTP.Transport}
	session, err := mcp.NewClient(impl, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: localHTTP, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return err
	}
	defer session.Close()

	server := mcp.NewServer(impl, &mcp.ServerOptions{Instructions: session.InitializeResult().Instructions})
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return session.CallTool(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments, Meta: req.Params.Meta})
		})
	}
	return server.Run(ctx, downstream)
}
