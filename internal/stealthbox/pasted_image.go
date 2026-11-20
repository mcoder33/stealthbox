package stealthbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	readPastedImageTool        = "read_pasted_image"
	pastedImageEndpoint        = "/pasted-image"
	pastedImagePNG             = "image/png"
	pastedImageJPEG            = "image/jpeg"
	pastedImageAccept          = pastedImagePNG + ", " + pastedImageJPEG
	pastedImageTransferTimeout = 90 * time.Second
	pastedImageMaxBytes        = 8 << 20
	pastedImageMaxPath         = 1024
)

type pastedImage struct {
	mimeType string
	data     []byte
}

func pastedImageToolDefinition() map[string]any {
	return map[string]any{
		"name":        readPastedImageTool,
		"description": "View a terminal clipboard image by the exact absolute Mac pasted-image-*.png path supplied by the user. Requires the connected Mac bridge.",
		"annotations": map[string]bool{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true},
		"inputSchema": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"path": map[string]any{"type": "string", "minLength": 1, "maxLength": pastedImageMaxPath}},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
}

func callPastedImageTool(ctx context.Context, c Config, arguments json.RawMessage) (any, error) {
	var args map[string]json.RawMessage
	var path string
	if json.Unmarshal(arguments, &args) != nil || len(args) != 1 ||
		json.Unmarshal(args["path"], &path) != nil || path == "" || len(path) > pastedImageMaxPath {
		return nil, fmt.Errorf("read_pasted_image requires only the exact Mac image path")
	}
	image, err := fetchPastedImage(ctx, c, path)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"content": []map[string]any{{"type": "image", "mimeType": image.mimeType, "data": image.data}},
		"isError": false,
	}, nil
}

func fetchPastedImage(ctx context.Context, c Config, path string) (pastedImage, error) {
	if !c.Bridge.Enabled {
		return pastedImage{}, fmt.Errorf("Mac bridge is disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, pastedImageTransferTimeout)
	defer cancel()
	query := url.Values{"path": {path}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+pastedImageEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return pastedImage{}, err
	}
	req.Header.Set("Accept", pastedImageAccept)
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	client := c.bridgeClient()
	client.Transport.(*http.Transport).ResponseHeaderTimeout = 5 * time.Second
	res, err := client.Do(req)
	if err != nil {
		return pastedImage{}, pastedImageTransportError(err, "could not reach the bridge")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return pastedImage{}, fmt.Errorf("Mac pasted image: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(message)))
	}
	mime := res.Header.Get("Content-Type")
	if mime != pastedImagePNG && mime != pastedImageJPEG {
		return pastedImage{}, fmt.Errorf("Mac bridge returned an unexpected image type")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, pastedImageMaxBytes+1))
	if err != nil {
		return pastedImage{}, pastedImageTransportError(err, "bridge connection ended during transfer")
	}
	if len(data) > pastedImageMaxBytes {
		return pastedImage{}, fmt.Errorf("Mac bridge response exceeds the 8 MiB transfer limit")
	}
	if len(data) == 0 {
		return pastedImage{}, fmt.Errorf("Mac bridge returned an empty pasted image")
	}
	return pastedImage{mimeType: mime, data: data}, nil
}

func pastedImageTransportError(err error, stage string) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("Mac pasted image transfer timed out; check the bridge connection and try again")
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("Mac pasted image transfer was canceled")
	default:
		return fmt.Errorf("Mac pasted image: %s", stage)
	}
}
