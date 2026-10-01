package stealthbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
)

func serveFile(w http.ResponseWriter, r *http.Request, c Config, locks map[string]*sync.Mutex) {
	name := r.URL.Query().Get("project")
	rel := r.URL.Query().Get("path")
	p, ok := c.Projects[name]
	if !ok {
		http.Error(w, "unknown project", 403)
		return
	}
	if !filepath.IsLocal(rel) || rel == "." {
		http.Error(w, "path must be relative to runner directory", 400)
		return
	}
	dst, ok := p.Runners["mac"]
	if !ok || dst.Host != "" || dst.Bridge {
		http.Error(w, "Mac runner unavailable", 400)
		return
	}
	lock := locks[name]
	if !lock.TryLock() {
		http.Error(w, "runner is busy", 409)
		return
	}
	defer lock.Unlock()
	root, err := os.OpenRoot(dst.Path)
	if err != nil {
		http.Error(w, "runner directory unavailable", 404)
		return
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		http.Error(w, "artifact unavailable or outside runner root", 404)
		return
	}
	defer f.Close()
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() {
		http.Error(w, "artifact must be a regular file", 400)
		return
	}
	max, _ := limits(c)
	if s.Size() > max {
		http.Error(w, "artifact exceeds transfer limit", 413)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(s.Size()))
	_, _ = io.CopyN(w, f, s.Size())
}
func Fetch(ctx context.Context, c Config, project, rel, output string) error {
	if !c.Bridge.Enabled {
		return fmt.Errorf("Mac bridge is disabled")
	}
	if !filepath.IsLocal(rel) || rel == "." {
		return fmt.Errorf("artifact path must be relative")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://unix/file?"+url.Values{"project": {project}, "path": {rel}}.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Bridge.Token)
	res, err := bridgeHTTP(c.Bridge.Socket).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("fetch failed: HTTP %d", res.StatusCode)
	}
	max, _ := limits(c)
	if res.ContentLength > max {
		return fmt.Errorf("artifact exceeds limit")
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(output)
		}
	}()
	n, err := io.Copy(file, io.LimitReader(res.Body, max+1))
	if err != nil {
		return err
	}
	if n > max {
		return fmt.Errorf("artifact exceeds limit")
	}
	if err = file.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
