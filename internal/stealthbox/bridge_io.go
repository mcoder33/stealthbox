package stealthbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

const bridgeIOIdleTimeout = 5 * time.Second

type bridgeResponse struct {
	writer  http.ResponseWriter
	control *http.ResponseController
	until   time.Time
}

func newBridgeResponse(w http.ResponseWriter, deadline time.Time) *bridgeResponse {
	return &bridgeResponse{writer: w, control: http.NewResponseController(w), until: deadline}
}

func (r *bridgeResponse) Write(p []byte) (int, error) {
	_ = r.control.SetWriteDeadline(bridgeIODeadline(r.until))
	n, err := r.writer.Write(p)
	if err == nil {
		_ = r.control.SetWriteDeadline(time.Time{})
	}
	return n, err
}

func (r *bridgeResponse) flush() error {
	_ = r.control.SetWriteDeadline(bridgeIODeadline(r.until))
	err := r.control.Flush()
	if err == nil || errors.Is(err, http.ErrNotSupported) {
		_ = r.control.SetWriteDeadline(time.Time{})
		return nil
	}
	return err
}

type bridgeArchiveReader struct {
	ctx     context.Context
	control *http.ResponseController
	body    io.Reader
}

func (r bridgeArchiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	deadline, _ := r.ctx.Deadline()
	_ = r.control.SetReadDeadline(bridgeIODeadline(deadline))
	return r.body.Read(p)
}

func bridgeIODeadline(deadline time.Time) time.Time {
	idle := time.Now().Add(bridgeIOIdleTimeout)
	if deadline.Before(idle) {
		return deadline
	}
	return idle
}
