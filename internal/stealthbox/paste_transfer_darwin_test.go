package stealthbox

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestPastedImageNegotiatesLargeScreenshot(t *testing.T) {
	const originalBytes = 5306822
	const width, height = 3856, 1962
	source := image.NewRGBA(image.Rect(0, 0, width, height))
	paint := image.NewUniform(color.RGBA{R: 139, G: 46, B: 229, A: 255})
	draw.Draw(source, source.Bounds(), paint, image.Point{}, draw.Src)
	var b bytes.Buffer
	if err := png.Encode(&b, source); err != nil {
		t.Fatal(err)
	}
	data := b.Bytes()
	const iendBytes = 12
	// Pad a valid private PNG chunk so the test exercises the failing transfer size.
	padding := make([]byte, originalBytes-len(data))
	binary.BigEndian.PutUint32(padding[:4], uint32(len(padding)-12))
	copy(padding[4:8], "sbIg")
	binary.BigEndian.PutUint32(padding[len(padding)-4:], crc32.ChecksumIEEE(padding[4:len(padding)-4]))
	fixture := bytes.Join([][]byte{data[:len(data)-iendBytes], padding, data[len(data)-iendBytes:]}, nil)
	if len(fixture) != originalBytes {
		t.Fatal(len(fixture))
	}
	path := filepath.Join(pastedImageTestDir(t), "pasted-image-large.png")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}

	c := bridgeConfig(t)
	for _, accept := range []string{"", "image/png, image/jpeg"} {
		name := "legacy PNG client"
		if accept != "" {
			name = "bounded client"
		}
		t.Run(name, func(t *testing.T) {
			query := url.Values{"path": {path}}
			req := httptest.NewRequest(http.MethodGet, "/pasted-image?"+query.Encode(), nil)
			req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
			req.Header.Set("Accept", accept)
			out := httptest.NewRecorder()
			BridgeHandler(c).ServeHTTP(out, req)
			if out.Code != http.StatusOK {
				t.Fatalf("status %d: %s", out.Code, out.Body)
			}
			mime := out.Header().Get("Content-Type")
			if accept == "" {
				if mime != "image/png" || !bytes.Equal(out.Body.Bytes(), fixture) {
					t.Fatalf("legacy client got %s, %d bytes", mime, out.Body.Len())
				}
				return
			}
			if mime != "image/jpeg" || out.Body.Len() >= 3<<20 {
				t.Fatalf("bounded client got %s, %d bytes", mime, out.Body.Len())
			}
			result, err := jpeg.Decode(out.Body)
			if err != nil {
				t.Fatal(err)
			}
			if bounds := result.Bounds(); bounds.Dx() != 2048 || bounds.Dy() != (height*2048+width/2)/width {
				t.Fatalf("unexpected resized dimensions: %s", bounds)
			}
			center := color.NRGBAModel.Convert(result.At(1024, 512)).(color.NRGBA)
			if center.R < 135 || center.R > 143 || center.G < 42 || center.G > 50 || center.B < 225 || center.B > 233 {
				t.Fatalf("unexpected JPEG color: %#v", center)
			}
		})
	}
}
