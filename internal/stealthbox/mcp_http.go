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

type mcpHTTPServer struct {
	config    Config
	key       []byte
	host      string
	timeout   time.Duration
	reading   chan struct{}
	executing chan struct{}

	mu     sync.Mutex
	active map[mcpCallKey]context.CancelCauseFunc
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
		reading:   make(chan struct{}, mcpHTTPMaxReads),
		executing: make(chan struct{}, mcpHTTPMaxCalls),
		active:    make(map[mcpCallKey]context.CancelCauseFunc),
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
	address, _, _ := net.SplitHostPort(r.RemoteAddr)
	origin := r.Header.Values("Origin")
	if !net.ParseIP(address).IsLoopback() || r.Host != s.host ||
		(len(origin) != 0 && (len(origin) != 1 || origin[0] != "http://"+s.host)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/health" {
		s.health(w, r)
		return
	}
	authorization := r.Header.Values("Authorization")
	if len(authorization) != 1 {
		mcpHTTPUnauthorized(w)
		return
	}
	client, ok := s.authenticate(authorization[0])
	if !ok {
		mcpHTTPUnauthorized(w)
		return
	}
	switch r.URL.Path {
	case mcpHTTPPath:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.post(w, r, client)
	default:
		http.NotFound(w, r)
	}
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

func mcpHTTPUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (s *mcpHTTPServer) post(w http.ResponseWriter, r *http.Request, client mcpHTTPClient) {
	protocolHeader := r.Header.Values(mcpProtocolHeader)
	if len(protocolHeader) > 1 || (len(protocolHeader) == 1 && !mcpHTTPSupports(protocolHeader[0])) {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	sessionHeader := r.Header.Values(mcpSessionHeader)
	if len(sessionHeader) > 1 {
		http.Error(w, "multiple MCP session IDs", http.StatusBadRequest)
		return
	}
	session := r.Header.Get(mcpSessionHeader)
	var protocol string
	if len(sessionHeader) == 1 {
		protocol = s.sessionProtocol(client, session)
		if protocol == "" {
			http.Error(w, "MCP session expired or unknown", http.StatusNotFound)
			return
		}
		if len(protocolHeader) == 1 && protocolHeader[0] != protocol {
			http.Error(w, "protocol version differs from MCP session", http.StatusBadRequest)
			return
		}
	}
	req, status, err := s.readRequest(w, r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	id := mcpID(req.ID)
	if req.Method == "initialize" && session == "" && id != "" {
		protocol := mcpProtocol(req.Params, mcpHTTPProtocol)
		w.Header().Set(mcpSessionHeader, s.mintSession(client, protocol))
		writeMCPHTTPResponse(w, dispatchMCP(r.Context(), s.config, client.MCPScope, req, protocol, 0))
		return
	}
	if session == "" || req.Method == "initialize" {
		http.Error(w, "initialize a new MCP session first", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		s.notify(session, req)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if id == "" {
		http.Error(w, "invalid MCP request ID", http.StatusBadRequest)
		return
	}
	if req.Method != "tools/call" {
		writeMCPHTTPResponse(w, dispatchMCP(r.Context(), s.config, client.MCPScope, req, protocol, 0))
		return
	}
	s.callTool(w, r.Context(), client.MCPScope, mcpCallKey{session: session, request: id}, req)
}

func mcpHTTPSupports(protocol string) bool {
	switch protocol {
	case "2025-03-26", "2025-06-18", mcpHTTPProtocol:
		return true
	default:
		return false
	}
}

func (s *mcpHTTPServer) readRequest(w http.ResponseWriter, r *http.Request) (mcpRequest, int, error) {
	var req mcpRequest
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "application/json" {
		return req, http.StatusUnsupportedMediaType, errors.New("MCP POST requires application/json")
	}
	if r.ContentLength > mcpHTTPMaxBody {
		return req, http.StatusRequestEntityTooLarge, errors.New("MCP request body exceeds its size limit")
	}
	select {
	case s.reading <- struct{}{}:
		defer func() { <-s.reading }()
	default:
		return req, http.StatusServiceUnavailable, errors.New("MCP HTTP request reader is busy")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpHTTPMaxBody))
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return req, http.StatusRequestEntityTooLarge, errors.New("MCP request body exceeds its size limit")
	}
	if err != nil {
		return req, http.StatusBadRequest, errors.New("could not read MCP request body")
	}
	if !utf8.Valid(data) || json.Unmarshal(data, &req) != nil || req.JSONRPC != "2.0" || req.Method == "" {
		return req, http.StatusBadRequest, errors.New("invalid MCP JSON-RPC message")
	}
	return req, 0, nil
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

func (s *mcpHTTPServer) callTool(w http.ResponseWriter, ctx context.Context, scope MCPScope, key mcpCallKey, req mcpRequest) {
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
	response := dispatchMCP(ctx, s.config, scope, req, "", mcpHTTPBridgeTimeout(s.config))
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
