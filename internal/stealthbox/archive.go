package stealthbox

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func excluded(path string) bool {
	for _, p := range strings.Split(filepath.ToSlash(path), "/") {
		if p == ".git" || p == ".env" || strings.HasPrefix(p, ".env.") || p == "node_modules" || p == "vendor" || p == ".serena" || p == ".stealthbox-runner" {
			return true
		}
	}
	return false
}

// Snapshot never dereferences symlinks or includes the usual secret/dependency directories.
func Snapshot(root string, w io.Writer, max int64) error {
	tw := tar.NewWriter(w)
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		if excluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		total += info.Size()
		if total > max {
			return fmt.Errorf("snapshot exceeds %d bytes", max)
		}
		h, e := tar.FileInfoHeader(info, "")
		if e != nil {
			return e
		}
		h.Name = filepath.ToSlash(rel)
		if e = tw.WriteHeader(h); e != nil {
			return e
		}
		if info.IsDir() {
			return nil
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		_, e = io.CopyN(tw, f, info.Size())
		f.Close()
		return e
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// A stage directory receives only regular files/directories. Existing checkout is untouched until validation succeeds.
func extractArchive(r io.Reader, stage string, max int64) error {
	tr := tar.NewReader(r)
	var total int64
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		if !filepath.IsLocal(name) || name == "." || excluded(name) || strings.ContainsRune(name, 0) {
			return fmt.Errorf("unsafe archive entry %q", h.Name)
		}
		name = filepath.Clean(name)
		if seen[name] {
			return fmt.Errorf("duplicate archive entry %q", name)
		}
		seen[name] = true
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive links and special files are unsupported")
		}
		if h.Size < 0 || h.Size > max-total {
			return fmt.Errorf("archive exceeds limit")
		}
		total += h.Size
		path := filepath.Join(stage, name)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			if err = os.MkdirAll(path, 0700); err != nil {
				return err
			}
			continue
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode)&0777)
		if err != nil {
			return err
		}
		_, err = io.CopyN(f, tr, h.Size)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
}
func validateRunnerRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return fmt.Errorf("invalid runner root")
	}
	// Disallow symlink components, including parents of a not-yet-existing directory.
	for p := filepath.Clean(root); ; p = filepath.Dir(p) {
		s, e := os.Lstat(p)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil && s.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runner path contains symlink: %s", p)
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	marker, err := os.Lstat(filepath.Join(root, ".stealthbox-runner"))
	if err == nil && marker.Mode().IsRegular() {
		return nil
	}
	if len(entries) > 0 {
		return fmt.Errorf("refusing unmarked nonempty runner directory %s", root)
	}
	return os.WriteFile(filepath.Join(root, ".stealthbox-runner"), nil, 0600)
}

// SyncSnapshot preserves excluded paths, removes stale code, and never follows destination symlinks.
func SyncSnapshot(root string, r io.Reader, max int64) error {
	if err := validateRunnerRoot(root); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(root), ".stealthbox-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = extractArchive(r, stage, max); err != nil {
		return err
	}
	// Move only excluded entries to the new snapshot. Everything else is disposable.
	type moved struct{ old, new string }
	var moves []moved
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		if !excluded(rel) {
			return nil
		}
		to := filepath.Join(stage, rel)
		if e = os.MkdirAll(filepath.Dir(to), 0700); e != nil {
			return e
		}
		moves = append(moves, moved{path, to})
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return err
	}
	restore := func() {
		for i := len(moves) - 1; i >= 0; i-- {
			if _, e := os.Lstat(moves[i].new); e == nil {
				_ = os.Rename(moves[i].new, moves[i].old)
			}
		}
	}
	for _, m := range moves {
		if err = os.Rename(m.old, m.new); err != nil {
			restore()
			return err
		}
	}
	backup := root + ".stealthbox-old"
	if _, e := os.Lstat(backup); !os.IsNotExist(e) {
		restore()
		return fmt.Errorf("backup path already exists: %s", backup)
	}
	if err = os.Rename(root, backup); err != nil {
		restore()
		return err
	}
	if err = os.Rename(stage, root); err != nil {
		_ = os.Rename(backup, root)
		restore()
		return err
	}
	return os.RemoveAll(backup)
}
