package stealthbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	readPastedImageTool = "read_pasted_image"
	pastedImageEndpoint = "/pasted-image"
	pastedImageMIME     = "image/png"
	pastedImageMaxBytes = 8 << 20
	pastedImageMaxPath  = 1024
)

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
	data, err := fetchPastedImage(ctx, c, path)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"content": []map[string]any{{"type": "image", "mimeType": pastedImageMIME, "data": data}},
		"isError": false,
	}, nil
}

func fetchPastedImage(ctx context.Context, c Config, path string) ([]byte, error) {
	if !c.Bridge.Enabled {
		return nil, fmt.Errorf("Mac bridge is disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	query := url.Values{"path": {path}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+pastedImageEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	res, err := bridgeHTTP(c.Bridge.Socket).Do(req)
	if err != nil {
		return nil, fmt.Errorf("Mac bridge is disconnected or unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return nil, fmt.Errorf("Mac pasted image: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(message)))
	}
	if res.Header.Get("Content-Type") != pastedImageMIME {
		return nil, fmt.Errorf("Mac bridge returned an unexpected image type")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, pastedImageMaxBytes+1))
	if err != nil || len(data) == 0 || len(data) > pastedImageMaxBytes {
		return nil, fmt.Errorf("Mac bridge returned an incomplete or oversized pasted image")
	}
	return data, nil
}
