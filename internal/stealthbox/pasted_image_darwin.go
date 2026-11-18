package stealthbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const pastedImageMaxPixels = 16 << 20

func servePastedImage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	paths := query["path"]
	if len(query) != 1 || len(paths) != 1 {
		http.Error(w, "exactly one pasted-image path is required", http.StatusBadRequest)
		return
	}
	data, err := readPastedImage(r.Context(), paths[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", pastedImageMIME)
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}

func readPastedImage(ctx context.Context, path string) ([]byte, error) {
	path, err := macPastedImagePath(ctx, path)
	if err != nil {
		return nil, err
	}
	data, err := readPastedImageFile(path)
	if err != nil {
		return nil, err
	}
	if err := validatePastedPNG(data); err != nil {
		return nil, err
	}
	return data, nil
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

func validatePastedPNG(data []byte) error {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("pasted image is not a valid PNG")
	}
	if cfg.Width > pastedImageMaxPixels/cfg.Height {
		return fmt.Errorf("pasted image exceeds 16 megapixels")
	}
	reader := bytes.NewReader(data)
	if _, err := png.Decode(reader); err != nil || reader.Len() != 0 {
		return fmt.Errorf("pasted image is not a valid PNG")
	}
	return nil
}
