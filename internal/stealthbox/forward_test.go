package stealthbox

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplacementForwardLeavesOccupiedSocketAndServesBridge(t *testing.T) {
	c := reconnectFixture(t)
	c.Bridge.Socket = c.Bridge.RemoteSocket
	old, err := net.Listen("unix", c.Bridge.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close() // accepts connections in the kernel but never answers
	name := "sb-0123456789abcdef"
	path := filepath.Join(filepath.Dir(c.Bridge.Socket), name)
	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+c.Bridge.Token {
			http.Error(w, "denied", 401)
			return
		}
		json.NewEncoder(w).Encode(BridgeState{OK: true})
	})}
	go server.Serve(live)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- HoldBridgeForward(ctx, c, name) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(3 * time.Second); ; {
		if active, err := os.Readlink(c.Bridge.Socket + ".active"); err == nil && active == name {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("forward was not published")
		}
		time.Sleep(10 * time.Millisecond)
	}
	check, end := context.WithTimeout(ctx, time.Second)
	defer end()
	if err := BridgeHealth(check, c); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(c.Bridge.Socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("lost previous socket")
	}
}
