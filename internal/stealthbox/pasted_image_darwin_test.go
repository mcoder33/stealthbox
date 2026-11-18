package stealthbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMCPPastedImageFromMac(t *testing.T) {
	dir := pastedImageTestDir(t)
	path := filepath.Join(dir, "pasted-image-20261118-161344-synthetic.png")
	data := writePastedImageTestPNG(t, path)

	c := bridgeConfig(t)
	c.Bridge.AllowExec = false
	c.Bridge.Socket = filepath.Join(dir, "img.sock")
	listener, err := net.Listen("unix", c.Bridge.Socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: BridgeHandler(c)}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	t.Setenv("TMPDIR", canonicalTemp(t))
	t.Setenv("STEALTHBOX_SCOPE", "workspace")
	t.Setenv("STEALTHBOX_PROJECT", "unrelated")
	c.Workspace.VMRoot = "/checkout"
	for _, call := range []struct {
		name    string
		args    map[string]any
		wantErr bool
	}{
		{"var alias", map[string]any{"path": strings.TrimPrefix(path, "/private")}, false},
		{"private alias", map[string]any{"path": path}, false},
		{"extra execution arguments", map[string]any{"path": path, "argv": []string{"true"}}, true},
		{"missing path", map[string]any{}, true},
		{"unavailable", map[string]any{"path": filepath.Join(dir, "pasted-image-missing.png")}, true},
	} {
		t.Run(call.name, func(t *testing.T) {
			params, err := json.Marshal(map[string]any{"name": "read_pasted_image", "arguments": call.args})
			if err != nil {
				t.Fatal(err)
			}
			in := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n" +
				"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n" +
				"{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":" + string(params) + "}\n"
			var out bytes.Buffer
			if err := ServeMCP(context.Background(), c, strings.NewReader(in), &out); err != nil {
				t.Fatal(err)
			}
			responses := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(responses) != 3 || !strings.Contains(responses[0], "read_pasted_image") ||
				!strings.Contains(responses[1], `"name":"read_pasted_image"`) ||
				!strings.Contains(responses[1], `"readOnlyHint":true`) ||
				strings.Contains(responses[1], `"name":"mac_exec"`) {
				t.Fatal(out.String())
			}
			var response struct {
				Result struct {
					IsError bool
					Content []struct {
						Type, MimeType string
						Data           []byte
					}
				}
			}
			if err := json.Unmarshal([]byte(responses[2]), &response); err != nil {
				t.Fatal(err)
			}
			result := response.Result
			if result.IsError != call.wantErr || len(result.Content) != 1 {
				t.Fatal(responses[2])
			}
			item := result.Content[0]
			if call.wantErr {
				if item.Type != "text" || len(item.Data) != 0 {
					t.Fatal(responses[2])
				}
			} else if item.Type != "image" || item.MimeType != "image/png" || !bytes.Equal(item.Data, data) {
				t.Fatal(responses[2])
			}
		})
	}
}

func TestBridgePastedImageConfinement(t *testing.T) {
	c := bridgeConfig(t)
	dir := pastedImageTestDir(t)
	outside := filepath.Join("/private/tmp", "stealthbox-pasted-image-test")
	outside, err := os.MkdirTemp(filepath.Dir(outside), filepath.Base(outside))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	external := filepath.Join(outside, "pasted-image-outside.png")
	data := writePastedImageTestPNG(t, external)
	inside := filepath.Join(dir, "pasted-image-valid.png")
	writePastedImageTestPNG(t, inside)

	link := filepath.Join(dir, "pasted-image-link.png")
	nested := filepath.Join(dir, "linked-directory")
	hardLink := filepath.Join(dir, "pasted-image-hard-link.png")
	bad := filepath.Join(dir, "pasted-image-invalid.png")
	oversized := filepath.Join(dir, "pasted-image-large.png")
	trailing := filepath.Join(dir, "pasted-image-trailing.png")
	fifo := filepath.Join(dir, "pasted-image-fifo.png")
	directory := filepath.Join(dir, "pasted-image-directory.png")
	tooManyPixels := filepath.Join(dir, "pasted-image-too-many-pixels.png")
	hugePNGHeader := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(hugePNGHeader[16:20], 5000)
	binary.BigEndian.PutUint32(hugePNGHeader[20:24], 5000)
	binary.BigEndian.PutUint32(hugePNGHeader[29:33], crc32.ChecksumIEEE(hugePNGHeader[12:29]))
	for _, err := range []error{
		os.Symlink(external, link),
		os.Symlink(outside, nested),
		os.Link(external, hardLink),
		os.WriteFile(bad, data[:len(data)-6], 0600),
		os.WriteFile(trailing, append(data, "trailer"...), 0600),
		os.WriteFile(oversized, data, 0600),
		os.Truncate(oversized, (8<<20)+1),
		unix.Mkfifo(fifo, 0600),
		os.Mkdir(directory, 0700),
		os.WriteFile(tooManyPixels, hugePNGHeader, 0600),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	h := BridgeHandler(c)
	for _, tc := range []struct {
		name, path, token string
		want              int
	}{
		{"unauthorized", inside, "invalid", 401},
		{"outside user temp", external, c.Bridge.Token, 400},
		{"not a pasted image", filepath.Join(dir, "other.png"), c.Bridge.Token, 400},
		{"dot dot", dir + "/../" + filepath.Base(dir) + "/" + filepath.Base(inside), c.Bridge.Token, 400},
		{"symlink file", link, c.Bridge.Token, 400},
		{"symlink directory", filepath.Join(nested, filepath.Base(external)), c.Bridge.Token, 400},
		{"hard link", hardLink, c.Bridge.Token, 400},
		{"truncated PNG", bad, c.Bridge.Token, 400},
		{"trailing data", trailing, c.Bridge.Token, 400},
		{"large PNG", oversized, c.Bridge.Token, 400},
		{"too many pixels", tooManyPixels, c.Bridge.Token, 400},
		{"named pipe", fifo, c.Bridge.Token, 400},
		{"directory", directory, c.Bridge.Token, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{"path": {tc.path}}
			req := httptest.NewRequest("GET", "/pasted-image?"+q.Encode(), nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", out.Code, tc.want, out.Body)
			}
			if tc.path == tooManyPixels && !strings.Contains(out.Body.String(), "exceeds 16 megapixels") {
				t.Fatal(out.Body.String())
			}
			if out.Header().Get("Content-Type") == "image/png" {
				t.Fatal("error returned an image")
			}
		})
	}
}

func pastedImageTestDir(t *testing.T) string {
	t.Helper()
	root, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(strings.TrimSpace(string(root)), "sbimg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return "/private" + strings.TrimPrefix(dir, "/private")
}

func writePastedImageTestPNG(t *testing.T, path string) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
