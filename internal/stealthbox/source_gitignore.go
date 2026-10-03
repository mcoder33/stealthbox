package stealthbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Paths are workspace-relative so selecting a parent folder or a checkout uses
// the same .gitignore domains. Each side keeps its own rule precedence.
type SourceIgnoreFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type SourceIgnoreRules [][]SourceIgnoreFile

const sourceIgnoreLimit = 1 << 20

type sourceIgnoreMatcher struct {
	dir      string
	cmd      *exec.Cmd
	input    io.WriteCloser
	output   *bufio.Reader
	stderr   bytes.Buffer
	worktree string
	shadow   bool
}

func ignoreGitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	// Only the transferred .gitignore files define this policy. A user's global
	// excludes, index, fsmonitor and repository environment must not affect it.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_FLUSH=1")
	return cmd
}

func newSourceIgnoreMatcher(ctx context.Context, files []SourceIgnoreFile) (*sourceIgnoreMatcher, error) {
	m := &sourceIgnoreMatcher{}
	if len(files) == 0 {
		return m, nil
	}
	data, err := json.Marshal(files)
	if err != nil || len(data) > sourceIgnoreLimit {
		return nil, fmt.Errorf("source .gitignore rules exceed 1 MiB; select a smaller checkout")
	}
	m.dir, err = os.MkdirTemp("", "stealthbox-gitignore-")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			m.Close()
		}
	}()
	worktree := filepath.Join(m.dir, "tree")
	m.shadow = true
	if err = os.Mkdir(worktree, 0700); err != nil {
		return nil, err
	}
	for _, file := range files {
		if !filepath.IsLocal(file.Path) || filepath.ToSlash(filepath.Clean(file.Path)) != file.Path || filepath.Base(file.Path) != ".gitignore" || strings.ContainsAny(file.Path, "\x00\n\r\\") {
			return nil, fmt.Errorf("invalid source .gitignore path %q", file.Path)
		}
		path := filepath.Join(worktree, filepath.FromSlash(file.Path))
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if err = os.WriteFile(path, []byte(file.Content), 0600); err != nil {
			return nil, err
		}
	}
	if err = m.start(ctx, worktree); err != nil {
		return nil, err
	}
	ok = true
	return m, nil
}

func (m *sourceIgnoreMatcher) start(ctx context.Context, worktree string) error {
	m.worktree = worktree
	gitdir := filepath.Join(m.dir, "git")
	if out, err := ignoreGitCommand(ctx, "init", "--quiet", "--bare", "--template=", gitdir).CombinedOutput(); err != nil {
		return fmt.Errorf("initialize .gitignore matcher: %w: %s", err, out)
	}
	m.cmd = ignoreGitCommand(ctx, "--git-dir="+gitdir, "--work-tree="+worktree,
		"-c", "core.bare=false", "-c", "core.excludesFile="+os.DevNull, "-c", "core.fsmonitor=false",
		"check-ignore", "--no-index", "--stdin", "-z", "--verbose", "--non-matching")
	m.cmd.Dir = worktree
	m.cmd.Stderr = &m.stderr
	var err error
	m.input, err = m.cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := m.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	m.output = bufio.NewReader(output)
	if err = m.cmd.Start(); err != nil {
		return err
	}
	return nil
}

func (m *sourceIgnoreMatcher) Close() {
	if m.input != nil {
		m.input.Close()
	}
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Wait()
	}
	if m.dir != "" {
		os.RemoveAll(m.dir)
	}
}

func (m *sourceIgnoreMatcher) Ignored(path string, isDir bool) (bool, error) {
	if m.input == nil {
		return false, nil
	}
	path = filepath.ToSlash(path)
	if !filepath.IsLocal(path) || strings.ContainsAny(path, "\x00\n\r") {
		return false, fmt.Errorf("invalid path for source .gitignore matching")
	}
	if isDir && m.shadow {
		// Git determines directory-only matches with lstat. A trailing slash
		// would incorrectly let a rule like dir/* match the directory itself.
		if err := os.MkdirAll(filepath.Join(m.worktree, path), 0700); err != nil {
			return false, err
		}
	}
	if _, err := io.WriteString(m.input, path+"\x00"); err != nil {
		return false, fmt.Errorf("query source .gitignore: %w", err)
	}
	var fields [4]string
	for i := range fields {
		field, err := m.output.ReadString(0)
		if err != nil {
			return false, fmt.Errorf("read source .gitignore match: %w", err)
		}
		fields[i] = strings.TrimSuffix(field, "\x00")
	}
	if fields[3] != path {
		return false, fmt.Errorf("unexpected source .gitignore match path")
	}
	return fields[2] != "" && !strings.HasPrefix(fields[2], "!"), nil
}

type sourceIgnoreMatchers []*sourceIgnoreMatcher

func newSourceIgnoreMatchers(ctx context.Context, rules SourceIgnoreRules) (sourceIgnoreMatchers, error) {
	var matchers sourceIgnoreMatchers
	for _, files := range rules {
		m, err := newSourceIgnoreMatcher(ctx, files)
		if err != nil {
			matchers.Close()
			return nil, err
		}
		matchers = append(matchers, m)
	}
	return matchers, nil
}

func (ms sourceIgnoreMatchers) Close() {
	for _, m := range ms {
		m.Close()
	}
}

func (ms sourceIgnoreMatchers) Ignored(path string, isDir bool) (bool, error) {
	for _, m := range ms {
		ignored, err := m.Ignored(path, isDir)
		if err != nil || ignored {
			return ignored, err
		}
	}
	return false, nil
}

func sourceIgnoreFiles(ctx context.Context, root, relative string, excludes []string) ([]SourceIgnoreFile, error) {
	selected, err := checkedSourcePath(root, relative)
	if err != nil {
		return nil, err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if _, err = checkoutSourceExcludes(relative, excludes); err != nil {
		return nil, err
	}
	var files []SourceIgnoreFile
	matcher := &sourceIgnoreMatcher{}
	matcher.dir, err = os.MkdirTemp("", "stealthbox-gitignore-scan-")
	if err != nil {
		return nil, err
	}
	defer matcher.Close()
	if err = matcher.start(ctx, canonicalRoot); err != nil {
		return nil, err
	}
	size := 0
	load := func(dir string) error {
		path := filepath.Join(dir, ".gitignore")
		info, err := os.Lstat(path)
		if os.IsNotExist(err) || err == nil && !info.Mode().IsRegular() {
			return nil // Git does not follow symlinks to .gitignore.
		}
		if err != nil {
			return err
		}
		if info.Size() > sourceIgnoreLimit {
			return fmt.Errorf("source .gitignore exceeds 1 MiB")
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(canonicalRoot, path)
		if err != nil {
			return err
		}
		files = append(files, SourceIgnoreFile{Path: filepath.ToSlash(rel), Content: string(content)})
		size += len(content) + len(rel)
		if size > sourceIgnoreLimit {
			return fmt.Errorf("source .gitignore rules exceed 1 MiB; select a smaller checkout")
		}
		return nil
	}
	// Include ancestor rules even when only a nested checkout is selected.
	dir := canonicalRoot
	for _, part := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		if err = load(dir); err != nil {
			return nil, err
		}
		dir = filepath.Join(dir, part)
	}
	if _, err = os.Lstat(selected); os.IsNotExist(err) {
		return files, nil
	} else if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(selected, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(canonicalRoot, file)
		if err != nil {
			return err
		}
		if excluded(rel) || sourcePathExcluded(rel, excludes) {
			return filepath.SkipDir
		}
		ignored, err := matcher.Ignored(rel, true)
		if err != nil {
			return err
		}
		if ignored {
			return filepath.SkipDir
		}
		return load(file)
	})
	return files, err
}

// SourceIgnoreFiles is the read-only VM half of source import/export.
func SourceIgnoreFiles(ctx context.Context, c Config, path string) ([]SourceIgnoreFile, error) {
	if c.Workspace.Host != "" {
		return nil, fmt.Errorf("source ignore scan runs on the VM")
	}
	return sourceIgnoreFiles(ctx, c.Workspace.VMRoot, path, c.Workspace.SourceExcludes)
}

func remoteSourceIgnoreFiles(ctx context.Context, c Config, path string) ([]SourceIgnoreFile, error) {
	if c.Workspace.Host == "" {
		return SourceIgnoreFiles(ctx, c, path)
	}
	if c.Workspace.RemoteDir == "" {
		return nil, fmt.Errorf("run setup before source sync")
	}
	out, err := Output(ctx, sshCommand(c.Workspace.Host, shellArgs(remoteSourceArgs(c, "ignore", path)), false))
	if err != nil {
		return nil, fmt.Errorf("remote .gitignore scan (update the VM with setup): %w", err)
	}
	if len(out) > sourceIgnoreLimit {
		return nil, fmt.Errorf("remote source .gitignore rules exceed 1 MiB")
	}
	var files []SourceIgnoreFile
	if err = json.Unmarshal(out, &files); err != nil {
		return nil, fmt.Errorf("invalid remote source .gitignore rules: %w", err)
	}
	return files, nil
}

func sourceIgnoreIncludeFile(source, destination SourceState) (string, error) {
	file, err := os.CreateTemp("", "stealthbox-source-includes-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(file.Name())
		}
	}()
	escape := strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[", "]", "\\]")
	var includes []string
	for _, state := range []SourceState{source, destination} {
		for _, entry := range state.Entries {
			if entry.Path == "." {
				continue
			}
			if !filepath.IsLocal(entry.Path) || filepath.ToSlash(filepath.Clean(entry.Path)) != entry.Path || strings.ContainsAny(entry.Path, "\x00\n\r") {
				return "", fmt.Errorf("invalid source manifest path %q", entry.Path)
			}
			literal := "/" + escape.Replace(entry.Path)
			if entry.Kind == "dir" {
				literal += "/"
			}
			includes = append(includes, literal)
		}
	}
	slices.Sort(includes)
	writer := bufio.NewWriter(file)
	for _, include := range slices.Compact(includes) {
		if _, err = io.WriteString(writer, include+"\n"); err != nil {
			return "", err
		}
	}
	if err = writer.Flush(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	ok = true
	return file.Name(), nil
}
