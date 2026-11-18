package stealthbox

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchPastedImageResponse(t *testing.T) {
	c := bridgeConfig(t)
	dir, err := os.MkdirTemp("/tmp", "sb-paste-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c.Bridge.Socket = filepath.Join(dir, "p.sock")
	listener, err := net.Listen("unix", c.Bridge.Socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != pastedImageAccept || r.Header.Get("Authorization") != "Bearer "+c.Bridge.Token {
			http.Error(w, "missing negotiation or authorization", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		switch r.URL.Query().Get("path") {
		case "complete":
			_, _ = io.WriteString(w, "complete")
		case "truncated":
			w.Header().Set("Content-Length", "12")
			_, _ = io.WriteString(w, "short")
		case "stalled":
			w.Header().Set("Content-Length", "12")
			_, _ = io.WriteString(w, "short")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	})}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	t.Run("negotiated JPEG", func(t *testing.T) {
		image, err := fetchPastedImage(context.Background(), c, "complete")
		if err != nil || image.mimeType != "image/jpeg" || string(image.data) != "complete" {
			t.Fatalf("unexpected image response: %v, %s, %d bytes", err, image.mimeType, len(image.data))
		}
	})
	t.Run("truncated transfer", func(t *testing.T) {
		image, err := fetchPastedImage(context.Background(), c, "truncated")
		if err == nil || !strings.Contains(err.Error(), "connection ended during transfer") || len(image.data) != 0 {
			t.Fatalf("expected a truncated transfer error, got %v", err)
		}
	})
	t.Run("transfer timed out", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		image, err := fetchPastedImage(ctx, c, "stalled")
		if err == nil || !strings.Contains(err.Error(), "transfer timed out") || len(image.data) != 0 {
			t.Fatalf("expected a timeout error, got %v", err)
		}
	})
}
