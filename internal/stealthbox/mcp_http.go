package stealthbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	mcpHTTPPath          = "/mcp"
	mcpHTTPRetirePath    = "/retire-idle"
	mcpSessionHeader     = "MCP-Session-Id"
	mcpProtocolHeader    = "MCP-Protocol-Version"
	mcpHTTPProtocol      = "2025-11-25"
	mcpHTTPMaxBody       = 2 << 20
	mcpHTTPMaxResponse   = 16 << 20
	mcpHTTPMaxReads      = 16
	mcpHTTPMaxCalls      = 32
	mcpHTTPReadTimeout   = 10 * time.Second
	mcpHTTPWriteTimeout  = 5 * time.Second
	mcpHTTPShutdownGrace = 5 * time.Second
)

var errMCPCancelledByClient = errors.New("MCP request cancelled by client")

type mcpCallKey struct {
	session string
	request string
}

type mcpHTTPSession struct {
	id       string
	protocol string
}

type mcpHTTPServer struct {
	serena    *serenaPool
	config    Config
	key       []byte
	host      string
	timeout   time.Duration
	stop      context.CancelFunc
	reading   chan struct{}
	executing chan struct{}

	mu      sync.Mutex
	retired bool
	active  map[mcpCallKey]context.CancelCauseFunc
}

// ServeMCPHTTP serves the MCP endpoint on an already-bound loopback TCP listener.
// It owns the listener and cancels admitted calls when ctx is cancelled.
func ServeMCPHTTP(ctx context.Context, c Config, listener net.Listener, key []byte) error {
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		return fmt.Errorf("MCP HTTP listener must bind to a loopback IP address")
	}
	if len(key) != sha256.Size {
		return fmt.Errorf("MCP HTTP needs a 32-byte service key")
	}
	serviceCtx, stop := context.WithCancel(ctx)
	defer stop()
	timeout := mcpHTTPCallTimeout(c)
	handler := &mcpHTTPServer{
		config:    c,
		key:       bytes.Clone(key),
		host:      addr.String(),
		timeout:   timeout,
		stop:      stop,
		reading:   make(chan struct{}, mcpHTTPMaxReads),
		executing: make(chan struct{}, mcpHTTPMaxCalls),
		active:    make(map[mcpCallKey]context.CancelCauseFunc),
	}
	if c.Workspace.Serena != nil {
		pool, err := newSerenaPool(c.Workspace.Serena, serenaIdleTimeout)
		if err != nil {
			return err
		}
		handler.serena = pool
		defer handler.serena.close()
	}
	server := &http.Server{
		Handler:           handler,
		BaseContext:       func(net.Listener) context.Context { return serviceCtx },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       mcpHTTPReadTimeout,
		WriteTimeout:      mcpHTTPReadTimeout + timeout + mcpHTTPWriteTimeout,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	shutdownResult := make(chan error, 1)
	go func() {
		<-serviceCtx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), mcpHTTPShutdownGrace)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			_ = server.Close()
		}
		shutdownResult <- err
	}()
	serveErr := server.Serve(listener)
	stop()
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, <-shutdownResult)
}

func mcpHTTPCallTimeout(c Config) time.Duration {
	_, operation := limits(c)
	if !c.Bridge.Enabled {
		return operation
	}
	return max(mcpHTTPBridgeTimeout(c), pastedImageTransferTimeout)
}

func mcpHTTPBridgeTimeout(c Config) time.Duration {
	// The Mac bridge accounts separately for preparation, queue wait and execution.
	_, operation := limits(c)
	return 3 * operation
}

func (s *mcpHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.fromLoopback(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/health":
		s.health(w, r)
		return
	case mcpHTTPRetirePath:
		s.retireIdle(w, r)
		return
	}
	client, ok := s.authenticate(r.Header.Values("Authorization"))
	if !ok {
		mcpHTTPUnauthorized(w)
		return
	}
	if r.URL.Path == "/serena" && s.serena != nil {
		client.serena = true
		client.ID += "/serena"
	} else if r.URL.Path != mcpHTTPPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.post(w, r, client)
}

func (s *mcpHTTPServer) fromLoopback(r *http.Request) bool {
	address, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !net.ParseIP(address).IsLoopback() || r.Host != s.host {
		return false
	}
	origin := r.Header.Values("Origin")
	return len(origin) == 0 || (len(origin) == 1 && origin[0] == "http://"+s.host)
}

func (s *mcpHTTPServer) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	challenge := r.Header.Values(mcpHTTPChallengeHeader)
	if len(challenge) != 1 || len(challenge[0]) < 16 || len(challenge[0]) > 128 {
		http.Error(w, "invalid MCP health challenge", http.StatusBadRequest)
		return
	}
	w.Header().Set(mcpHTTPProofHeader, mcpHTTPHealthProof(s.key, challenge[0]))
	w.WriteHeader(http.StatusNoContent)
}

func (s *mcpHTTPServer) retireIdle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := mcpAuthorizationToken(r.Header.Values("Authorization"))
	if _, ok := verifyMCPHTTP(s.key, mcpRetireDomain, token); !ok {
		mcpHTTPUnauthorized(w)
		return
	}
	s.mu.Lock()
	if len(s.active) != 0 {
		s.mu.Unlock()
		http.Error(w, "MCP service is busy", http.StatusConflict)
		return
	}
	s.retired = true
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	s.stop()
}

func mcpHTTPUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (s *mcpHTTPServer) post(w http.ResponseWriter, r *http.Request, client mcpHTTPClient) {
	session, ok := s.requestSession(w, r.Header, client)
	if !ok {
		return
	}
	req, ok := s.readRequest(w, r)
	if !ok {
		return
	}
	s.dispatch(w, r.Context(), client, session, req)
}

func (s *mcpHTTPServer) requestSession(w http.ResponseWriter, header http.Header, client mcpHTTPClient) (mcpHTTPSession, bool) {
	var session mcpHTTPSession
	protocolHeader := header.Values(mcpProtocolHeader)
	if len(protocolHeader) > 1 || (len(protocolHeader) == 1 && !mcpHTTPSupports(protocolHeader[0])) {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return session, false
	}
	sessionHeader := header.Values(mcpSessionHeader)
	if len(sessionHeader) == 0 {
		return session, true
	}
	if len(sessionHeader) > 1 {
		http.Error(w, "multiple MCP session IDs", http.StatusBadRequest)
		return session, false
	}
	session.id = sessionHeader[0]
	session.protocol = s.sessionProtocol(client, session.id)
	if session.protocol == "" {
		http.Error(w, "MCP session expired or unknown", http.StatusNotFound)
		return session, false
	}
	if len(protocolHeader) == 1 && protocolHeader[0] != session.protocol {
		http.Error(w, "protocol version differs from MCP session", http.StatusBadRequest)
		return session, false
	}
	return session, true
}

func (s *mcpHTTPServer) dispatch(w http.ResponseWriter, ctx context.Context, client mcpHTTPClient, session mcpHTTPSession, req mcpRequest) {
	id := mcpID(req.ID)
	if session.id == "" {
		s.initialize(w, ctx, client, req, id)
		return
	}
	if req.Method == "initialize" {
		http.Error(w, "initialize a new MCP session first", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		s.notify(session.id, req)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if id == "" {
		http.Error(w, "invalid MCP request ID", http.StatusBadRequest)
		return
	}
	if client.serena && req.Method == "tools/list" {
		writeMCPHTTPResponse(w, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: s.serena.inventory(client.Workspace)})
		return
	}
	if req.Method != "tools/call" {
		writeMCPHTTPResponse(w, dispatchMCP(ctx, s.config, client.MCPScope, req, session.protocol, 0))
		return
	}
	s.callTool(w, ctx, client, mcpCallKey{session: session.id, request: id}, req)
}

func (s *mcpHTTPServer) initialize(w http.ResponseWriter, ctx context.Context, client mcpHTTPClient, req mcpRequest, id string) {
	if req.Method != "initialize" || id == "" {
		http.Error(w, "initialize a new MCP session first", http.StatusBadRequest)
		return
	}
	protocol := mcpProtocol(req.Params, mcpHTTPProtocol)
	w.Header().Set(mcpSessionHeader, s.mintSession(client, protocol))
	response := dispatchMCP(ctx, s.config, client.MCPScope, req, protocol, 0)
	if client.serena {
		result := response.Result.(map[string]any)
		result["serverInfo"] = map[string]string{"name": "serena", "version": BuildVersion}
		result["instructions"] = "Serena is pinned to each VM checkout. In workspace scope pass worktree on each tool call. Use initial_instructions for the selected worktree."
	}
	writeMCPHTTPResponse(w, response)
}

func mcpHTTPSupports(protocol string) bool {
	switch protocol {
	case "2025-03-26", "2025-06-18", mcpHTTPProtocol:
		return true
	default:
		return false
	}
}

func (s *mcpHTTPServer) readRequest(w http.ResponseWriter, r *http.Request) (mcpRequest, bool) {
	var req mcpRequest
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "application/json" {
		http.Error(w, "MCP POST requires application/json", http.StatusUnsupportedMediaType)
		return req, false
	}
	if r.ContentLength > mcpHTTPMaxBody {
		http.Error(w, "MCP request body exceeds its size limit", http.StatusRequestEntityTooLarge)
		return req, false
	}
	select {
	case s.reading <- struct{}{}:
		defer func() { <-s.reading }()
	default:
		http.Error(w, "MCP HTTP request reader is busy", http.StatusServiceUnavailable)
		return req, false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpHTTPMaxBody))
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		http.Error(w, "MCP request body exceeds its size limit", http.StatusRequestEntityTooLarge)
		return req, false
	}
	if err != nil {
		http.Error(w, "could not read MCP request body", http.StatusBadRequest)
		return req, false
	}
	return decodeMCPHTTPRequest(w, data)
}

func decodeMCPHTTPRequest(w http.ResponseWriter, data []byte) (mcpRequest, bool) {
	var req mcpRequest
	if !utf8.Valid(data) || json.Unmarshal(data, &req) != nil || req.JSONRPC != "2.0" || req.Method == "" {
		http.Error(w, "invalid MCP JSON-RPC message", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func mcpID(id json.RawMessage) string {
	if len(id) == 0 || len(id) > 256 {
		return ""
	}
	var text string
	if id[0] == '"' && json.Unmarshal(id, &text) == nil {
		return "s:" + text
	}
	// JSON parsing already checked the grammar. MCP numeric IDs are integers.
	if strings.Trim(string(id), "-0123456789") == "" {
		return "n:" + string(id)
	}
	return ""
}

func (s *mcpHTTPServer) notify(session string, req mcpRequest) {
	if req.Method != "notifications/cancelled" {
		return
	}
	var params struct {
		ID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(req.Params, &params) != nil {
		return
	}
	key := mcpCallKey{session: session, request: mcpID(params.ID)}
	s.mu.Lock()
	cancel := s.active[key]
	s.mu.Unlock()
	if cancel != nil {
		cancel(errMCPCancelledByClient)
	}
}

func (s *mcpHTTPServer) callTool(w http.ResponseWriter, ctx context.Context, client mcpHTTPClient, key mcpCallKey, req mcpRequest) {
	select {
	case s.executing <- struct{}{}:
		defer func() { <-s.executing }()
	default:
		http.Error(w, "MCP HTTP tool capacity exhausted", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s.mu.Lock()
	if s.retired {
		s.mu.Unlock()
		http.Error(w, "MCP service retired", http.StatusServiceUnavailable)
		return
	}
	if _, running := s.active[key]; running {
		s.mu.Unlock()
		http.Error(w, "duplicate in-flight MCP request ID", http.StatusBadRequest)
		return
	}
	s.active[key] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, key)
		s.mu.Unlock()
	}()
	ctx, finish := context.WithTimeout(ctx, s.timeout)
	defer finish()
	if ctx.Err() != nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var response mcpResponse
	if client.serena {
		result, err := s.serena.call(ctx, s.config, client.MCPScope, req.Params)
		response = mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
		if err != nil {
			response.Result = map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": err.Error()}}}
		}
	} else {
		response = dispatchMCP(ctx, s.config, client.MCPScope, req, "", mcpHTTPBridgeTimeout(s.config))
	}
	if errors.Is(context.Cause(ctx), errMCPCancelledByClient) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeMCPHTTPResponse(w, response)
}

func writeMCPHTTPResponse(w http.ResponseWriter, response mcpResponse) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(mcpHTTPWriteTimeout))
	data, err := json.Marshal(response)
	if err != nil || len(data) > mcpHTTPMaxResponse {
		data, _ = json.Marshal(mcpError(response.ID, -32603, "MCP response exceeds its size limit"))
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
