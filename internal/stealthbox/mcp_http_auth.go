package stealthbox

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	mcpBearerDomain        = "stealthbox:mcp:client"
	mcpSessionDomain       = "stealthbox:mcp:session:"
	mcpHealthDomain        = "stealthbox:mcp:health"
	mcpRetireDomain        = "stealthbox:mcp:retire-idle"
	mcpHTTPChallengeHeader = "X-Stealthbox-MCP-Challenge"
	mcpHTTPProofHeader     = "X-Stealthbox-MCP-Proof"
	mcpTokenMaxBytes       = 4096
)

type mcpHTTPClient struct {
	MCPScope
	ID string `json:"id"`
}

// MCPHTTPBearer mints a credential for one client of a local shared service.
// The service key is private to the launcher and service.
func MCPHTTPBearer(key []byte, scope MCPScope) (string, error) {
	if len(key) != sha256.Size {
		return "", fmt.Errorf("MCP HTTP needs a 32-byte service key")
	}
	if !scope.validHTTPBinding() {
		return "", fmt.Errorf("MCP HTTP needs an explicit project or workspace binding and runner")
	}
	client := mcpHTTPClient{MCPScope: scope, ID: rand.Text()}
	claim, err := json.Marshal(client)
	if err != nil {
		return "", err
	}
	token := signMCPHTTP(key, mcpBearerDomain, claim)
	if len(token) > mcpTokenMaxBytes {
		return "", fmt.Errorf("MCP HTTP credential exceeds the size limit")
	}
	return token, nil
}

func (scope MCPScope) validHTTPBinding() bool {
	if scope.Workspace {
		runner := normalizeRunner(scope.Runner)
		return scope.Project == "" && (runner == "vm" || runner == "mac")
	}
	return nameRE.MatchString(scope.Project) && nameRE.MatchString(scope.Runner)
}

func mcpAuthorizationToken(authorization []string) string {
	if len(authorization) != 1 {
		return ""
	}
	scheme, token, _ := strings.Cut(authorization[0], " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return token
}

func (s *mcpHTTPServer) authenticate(authorization []string) (mcpHTTPClient, bool) {
	var client mcpHTTPClient
	token := mcpAuthorizationToken(authorization)
	payload, ok := verifyMCPHTTP(s.key, mcpBearerDomain, token)
	if !ok || json.Unmarshal(payload, &client) != nil || client.ID == "" || !client.validHTTPBinding() {
		return client, false
	}
	if client.Workspace {
		return client, WorkspaceRootEnabled(s.config)
	}
	project, ok := s.config.Projects[client.Project]
	if !ok {
		return client, false
	}
	_, ok = project.Runners[normalizeRunner(client.Runner)]
	return client, ok
}

func mcpHTTPHealthProof(key []byte, challenge string) string {
	return hex.EncodeToString(mcpHTTPMAC(key, mcpHealthDomain, challenge))
}

func (s *mcpHTTPServer) mintSession(client mcpHTTPClient, protocol string) string {
	payload := []byte(protocol + "/" + rand.Text())
	return signMCPHTTP(s.key, mcpSessionDomain+client.ID, payload)
}

func (s *mcpHTTPServer) sessionProtocol(client mcpHTTPClient, session string) string {
	payload, ok := verifyMCPHTTP(s.key, mcpSessionDomain+client.ID, session)
	if !ok {
		return ""
	}
	protocol, _, _ := strings.Cut(string(payload), "/")
	return protocol
}

func signMCPHTTP(key []byte, domain string, payload []byte) string {
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := mcpHTTPMAC(key, domain, encoded)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func verifyMCPHTTP(key []byte, domain, token string) ([]byte, bool) {
	if len(token) > mcpTokenMaxBytes {
		return nil, false
	}
	payload, signature, ok := strings.Cut(token, ".")
	if !ok {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || !hmac.Equal(decoded, mcpHTTPMAC(key, domain, payload)) {
		return nil, false
	}
	claim, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	return claim, err == nil
}

func mcpHTTPMAC(key []byte, domain, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}
