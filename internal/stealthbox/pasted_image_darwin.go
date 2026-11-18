package stealthbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/sys/unix"
)

const (
	pastedImageMaxPixels = 16 << 20
	// A 3 MiB image stays below 5 MB even after MCP base64 encoding.
	pastedImageMaxCompactBytes = 3 << 20
)

func servePastedImage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	paths := query["path"]
	if len(query) != 1 || len(paths) != 1 {
		http.Error(w, "exactly one pasted-image path is required", http.StatusBadRequest)
		return
	}
	image, err := readPastedImage(r.Context(), paths[0], r.Header.Get("Accept") == pastedImageAccept)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", image.mimeType)
	w.Header().Set("Content-Length", fmt.Sprint(len(image.data)))
	_, _ = w.Write(image.data)
}

func readPastedImage(ctx context.Context, path string, compact bool) (pastedImage, error) {
	path, err := macPastedImagePath(ctx, path)
	if err != nil {
		return pastedImage{}, err
	}
	data, err := readPastedImageFile(path)
	if err != nil {
		return pastedImage{}, err
	}
	decoded, err := decodePastedPNG(data)
	if err != nil {
		return pastedImage{}, err
	}
	if compact && (len(data) > pastedImageMaxCompactBytes || max(decoded.Bounds().Dx(), decoded.Bounds().Dy()) > 8000) {
		return compactPastedImage(decoded)
	}
	return pastedImage{mimeType: pastedImagePNG, data: data}, nil
}

func compactPastedImage(source image.Image) (pastedImage, error) {
	for _, encoding := range []struct{ edge, quality int }{{2048, 92}, {1600, 86}} {
		w, h := source.Bounds().Dx(), source.Bounds().Dy()
		longEdge := max(w, h)
		if longEdge > encoding.edge {
			w = max(1, (w*encoding.edge+longEdge/2)/longEdge)
			h = max(1, (h*encoding.edge+longEdge/2)/longEdge)
		}
		scaled := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(scaled, scaled.Bounds(), image.White, image.Point{}, draw.Src)
		draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), source, source.Bounds(), draw.Over, nil)
		var out bytes.Buffer
		if err := jpeg.Encode(&out, scaled, &jpeg.Options{Quality: encoding.quality}); err != nil {
			return pastedImage{}, fmt.Errorf("could not encode pasted image for transfer")
		}
		if out.Len() <= pastedImageMaxCompactBytes {
			return pastedImage{mimeType: pastedImageJPEG, data: out.Bytes()}, nil
		}
	}
	return pastedImage{}, fmt.Errorf("pasted image is too large after resizing")
}

func macPastedImagePath(ctx context.Context, path string) (string, error) {
	if len(path) > pastedImageMaxPath || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		strings.ContainsRune(path, 0) || !isPastedImageName(filepath.Base(path)) {
		return "", fmt.Errorf("expected an absolute path to a pasted-image-*.png file")
	}
	// Use the OS account's temporary directory, regardless of TMPDIR in either process.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		return "", fmt.Errorf("macOS user temporary directory is unavailable")
	}
	root := privateDarwinPath(filepath.Clean(strings.TrimSpace(string(out))))
	if !strings.HasPrefix(root, "/private/var/folders/") || filepath.Base(root) != "T" {
		return "", fmt.Errorf("macOS user temporary directory is unavailable")
	}
	path = privateDarwinPath(path)
	if !strings.HasPrefix(path, root+"/") {
		return "", fmt.Errorf("pasted image must be inside the macOS user temporary directory")
	}
	return path, nil
}

func readPastedImageFile(path string) ([]byte, error) {
	// Open the exact path once, with kernel-enforced no-symlink resolution for every
	// component. Validate and read the same descriptor, even if the path is replaced.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW_ANY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, fmt.Errorf("pasted image no longer exists; paste or upload it again")
	}
	if err != nil {
		return nil, fmt.Errorf("pasted image is unavailable or its path contains a link")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()

	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("pasted image must be a regular file owned by the Mac user with one link")
	}
	if stat.Size > pastedImageMaxBytes {
		return nil, fmt.Errorf("pasted image exceeds 8 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, pastedImageMaxBytes+1))
	var after unix.Stat_t
	if err != nil || int64(len(data)) != stat.Size || unix.Fstat(fd, &after) != nil ||
		after.Nlink != 1 || after.Size != stat.Size || after.Mtim != stat.Mtim || after.Ctim != stat.Ctim {
		return nil, fmt.Errorf("pasted image could not be read or changed during the read")
	}
	return data, nil
}

func isPastedImageName(name string) bool {
	stem, ok := strings.CutPrefix(name, "pasted-image-")
	id, png := strings.CutSuffix(stem, ".png")
	return ok && png && id != ""
}

func privateDarwinPath(path string) string {
	if strings.HasPrefix(path, "/var/") {
		return "/private" + path
	}
	return path
}

func decodePastedPNG(data []byte) (image.Image, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("pasted image is not a valid PNG")
	}
	if cfg.Width > pastedImageMaxPixels/cfg.Height {
		return nil, fmt.Errorf("pasted image exceeds 16 megapixels")
	}
	reader := bytes.NewReader(data)
	decoded, err := png.Decode(reader)
	if err != nil || reader.Len() != 0 {
		return nil, fmt.Errorf("pasted image is not a valid PNG")
	}
	return decoded, nil
}
