package stealthbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
)

const bridgeArchiveOverhead = 64 << 20

var errBridgeArchiveStagingFull = errors.New("queued archive storage is full")

type queuedBridgeArchive struct {
	file  *os.File
	queue *runnerQueue
	size  int64
}

func archiveRequestLimit(maxBytes int64) int64 {
	return min(maxBytes, math.MaxInt64-bridgeArchiveOverhead) + bridgeArchiveOverhead
}

func bridgeArchiveRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, limit int64) io.Reader {
	return bridgeArchiveReader{
		ctx:     ctx,
		control: http.NewResponseController(w),
		body:    http.MaxBytesReader(w, r.Body, limit),
	}
}

// Read the entire HTTP body while waiting. For HTTP/1, reaching the body EOF
// lets net/http detect peer closure even though the runner is still busy.
func stageQueuedBridgeArchive(ctx context.Context, w http.ResponseWriter, r *http.Request, queue *runnerQueue, maxBytes int64) (*queuedBridgeArchive, error) {
	limit := archiveRequestLimit(maxBytes)
	size := r.ContentLength
	if size < 0 {
		size = limit
	}
	if size > limit {
		return nil, &http.MaxBytesError{Limit: limit}
	}
	if !queue.reserveArchive(size, limit) {
		return nil, errBridgeArchiveStagingFull
	}
	file, err := os.CreateTemp("", "stealthbox-queued-archive-*.tar")
	if err != nil {
		queue.releaseArchive(size)
		return nil, fmt.Errorf("stage queued archive: %w", err)
	}
	staged := &queuedBridgeArchive{file: file, queue: queue, size: size}
	received := false
	defer func() {
		if !received {
			staged.close()
		}
	}()
	// Unlink now so a stopped bridge cannot leave a staged archive behind.
	if err := os.Remove(file.Name()); err != nil {
		return nil, fmt.Errorf("unlink staged archive: %w", err)
	}
	if err := receiveQueuedBridgeArchive(ctx, w, r, staged); err != nil {
		return nil, err
	}
	received = true
	return staged, nil
}

func receiveQueuedBridgeArchive(ctx context.Context, w http.ResponseWriter, r *http.Request, archive *queuedBridgeArchive) error {
	if _, err := io.Copy(archive.file, bridgeArchiveRequest(ctx, w, r, archive.size)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("receive queued archive: %w", err)
	}
	_, err := archive.file.Seek(0, io.SeekStart)
	return err
}

func (a *queuedBridgeArchive) close() {
	a.file.Close()
	a.queue.releaseArchive(a.size)
}
