package stealthbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ResolvedProject identifies a checkout, never just its basename. Git worktrees
// and nested repositories receive separate runner directories.
type ResolvedProject struct {
	ID           string
	DisplayName  string
	RelativePath string
	SourcePath   string
	RelativeCWD  string
	RunnerPath   string
	StaticName   string
	Project      Project
}

func WorkspaceRootEnabled(c Config) bool { return c.Workspace.VMRoot != "" }

func EnsureWorkspaceIdentity(c *Config) error {
	if !WorkspaceRootEnabled(*c) {
		return nil
	}
	if c.Workspace.ID == "" {
		sum := sha256.Sum256([]byte(c.Workspace.Host + "\x00" + filepath.Clean(c.Workspace.VMRoot)))
		c.Workspace.ID = fmt.Sprintf("workspace-%x", sum[:12])
	}
	return validateWorkspaceRoots(*c)
}

func validateWorkspaceRoots(c Config) error {
	if c.Workspace.SyncTransport != "" && c.Workspace.SyncTransport != "auto" && c.Workspace.SyncTransport != "rsync" && c.Workspace.SyncTransport != "archive" {
		return fmt.Errorf("workspace sync_transport must be auto, rsync or archive")
	}
	if !WorkspaceRootEnabled(c) {
		return nil
	}
	for label, path := range map[string]string{"VM root": c.Workspace.VMRoot, "local root": c.Workspace.LocalRoot, "runner root": c.Workspace.RunnerRoot} {
		if path == "" {
			continue
		}
		if label == "VM root" && (path == "~" || strings.HasPrefix(path, "~/")) && !strings.ContainsAny(path, "\n\r\x00") {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" || strings.ContainsAny(path, "\n\r\x00") {
			return fmt.Errorf("%s must be absolute and not filesystem root", label)
		}
	}
	if c.Workspace.RunnerRoot == "" {
		return fmt.Errorf("workspace mode needs a separate local runner_root")
	}
	if c.Workspace.ID != "" && !nameRE.MatchString(c.Workspace.ID) {
		return fmt.Errorf("invalid workspace ID")
	}
	if c.Workspace.LocalRoot != "" && pathsOverlap(c.Workspace.LocalRoot, c.Workspace.RunnerRoot) {
		return fmt.Errorf("local source and runner roots must not overlap")
	}
	for _, p := range c.Projects {
		if dst, ok := p.Runners["mac"]; ok && pathsOverlap(c.Workspace.RunnerRoot, dst.Path) {
			return fmt.Errorf("workspace runner root overlaps a configured project runner")
		}
	}
	return nil
}

func normalizeRunner(runner string) string {
	if runner == "local" {
		return "mac"
	}
	return runner
}

func relativeCommandDirectory(cwd string) (string, error) {
	if cwd == "" || cwd == "." {
		return "", nil
	}
	if !filepath.IsLocal(cwd) || strings.ContainsAny(cwd, "\n\r\x00") {
		return "", fmt.Errorf("cwd must be relative to the selected checkout")
	}
	return filepath.Clean(cwd), nil
}

// ResolveProject accepts a legacy configured name or a path inside VMRoot. A
// checkout path is required for workspace calls; the workspace root is not an
// implicit project. An empty selector on the VM uses the current directory.
func ResolveProject(ctx context.Context, c Config, selector, cwd string) (ResolvedProject, error) {
	relativeCWD, err := relativeCommandDirectory(cwd)
	if err != nil {
		return ResolvedProject{}, err
	}
	if p, ok := c.Projects[selector]; ok {
		sourcePath := p.Source.Path
		if p.Source.Host == "" {
			if physical, err := filepath.EvalSymlinks(sourcePath); err == nil {
				sourcePath = physical
			}
		}
		return ResolvedProject{ID: selector, DisplayName: selector, SourcePath: sourcePath, RelativeCWD: relativeCWD, RunnerPath: p.Runners["mac"].Path, StaticName: selector, Project: p}, nil
	}
	if !WorkspaceRootEnabled(c) {
		return ResolvedProject{}, fmt.Errorf("unknown project %q", selector)
	}
	if err = EnsureWorkspaceIdentity(&c); err != nil {
		return ResolvedProject{}, err
	}
	if !filepath.IsAbs(c.Workspace.VMRoot) {
		return ResolvedProject{}, fmt.Errorf("run setup to expand the VM workspace root before selecting projects")
	}
	root := filepath.Clean(c.Workspace.VMRoot)
	selected := selector
	if selected == "" {
		if c.Workspace.Host != "" {
			return ResolvedProject{}, fmt.Errorf("specify a checkout path under the VM workspace root")
		}
		selected, err = os.Getwd()
		if err != nil {
			return ResolvedProject{}, err
		}
	}
	if strings.ContainsAny(selected, "\n\r\x00") {
		return ResolvedProject{}, fmt.Errorf("invalid checkout path")
	}
	if !filepath.IsAbs(selected) {
		if !filepath.IsLocal(selected) {
			return ResolvedProject{}, fmt.Errorf("checkout path must stay inside the workspace root")
		}
		selected = filepath.Join(root, selected)
	}
	if !within(root, selected) {
		return ResolvedProject{}, fmt.Errorf("checkout path is outside the workspace root")
	}
	var canonicalRoot, checkout, start string
	if c.Workspace.Host != "" {
		canonicalRoot, checkout, start, err = resolveRemoteCheckout(ctx, c.Workspace.Host, root, selected)
	} else {
		canonicalRoot, checkout, start, err = resolveLocalCheckout(root, selected)
	}
	if err != nil {
		return ResolvedProject{}, err
	}
	rel, err := filepath.Rel(canonicalRoot, checkout)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return ResolvedProject{}, fmt.Errorf("select a checkout below the workspace root, not the whole workspace")
	}
	if excluded(rel) {
		return ResolvedProject{}, fmt.Errorf("checkout is inside an excluded dependency, secret or agent-state directory")
	}
	if relativeCWD == "" {
		relativeCWD, err = filepath.Rel(checkout, start)
		if err != nil || !within(checkout, start) {
			return ResolvedProject{}, fmt.Errorf("selected directory is outside its checkout")
		}
		if relativeCWD == "." {
			relativeCWD = ""
		}
	}
	sum := sha256.Sum256([]byte(c.Workspace.ID + "\x00" + filepath.ToSlash(rel)))
	id := fmt.Sprintf("project-%x", sum[:12])
	runnerPath := filepath.Join(c.Workspace.RunnerRoot, id, "tree")
	source := Endpoint{Host: c.Workspace.Host, Path: checkout}
	local := Endpoint{Path: runnerPath, Bridge: c.Workspace.Host == ""}
	p := Project{Source: source, Runners: map[string]Endpoint{"vm": source, "mac": local, "local": local}}
	return ResolvedProject{ID: id, DisplayName: filepath.ToSlash(rel), RelativePath: filepath.ToSlash(rel), SourcePath: checkout, RelativeCWD: relativeCWD, RunnerPath: runnerPath, Project: p}, nil
}

func resolveLocalCheckout(root, selected string) (canonicalRoot, checkout, start string, err error) {
	canonicalRoot, err = filepath.EvalSymlinks(root)
	if err != nil {
		return
	}
	start, err = filepath.EvalSymlinks(selected)
	if err != nil {
		return
	}
	if !within(canonicalRoot, start) {
		err = fmt.Errorf("checkout symlink escapes the workspace root")
		return
	}
	info, e := os.Stat(start)
	if e != nil || !info.IsDir() {
		err = fmt.Errorf("checkout path must be an existing directory")
		return
	}
	checkout = start
	for p := start; within(canonicalRoot, p); p = filepath.Dir(p) {
		marker, e := os.Lstat(filepath.Join(p, ".git"))
		if e == nil && (marker.IsDir() || marker.Mode().IsRegular()) {
			checkout = p
			return
		}
		if p == canonicalRoot {
			break
		}
	}
	relative, relErr := filepath.Rel(canonicalRoot, start)
	if relErr != nil {
		err = relErr
		return
	}
	if relative != "." {
		checkout = filepath.Join(canonicalRoot, strings.Split(relative, string(filepath.Separator))[0])
	}
	return
}

func resolveRemoteCheckout(ctx context.Context, host, root, selected string) (canonicalRoot, checkout, start string, err error) {
	// cd -P resolves parent symlinks on the source host before rsync sees a path.
	// Only quoted configured paths are interpolated; all output is checked again.
	script := "set -eu; cd -P " + Quote(root) + "; root=$(pwd -P); cd -P " + Quote(selected) + "; start=$(pwd -P); case \"$start\" in \"$root\"|\"$root\"/*) ;; *) exit 41;; esac; checkout=; p=$start; while :; do if [ -d \"$p/.git\" ] || [ -f \"$p/.git\" ]; then checkout=$p; break; fi; [ \"$p\" = \"$root\" ] && break; p=${p%/*}; [ -n \"$p\" ] || p=/; done; if [ -z \"$checkout\" ]; then if [ \"$start\" = \"$root\" ]; then checkout=$root; else relative=${start#\"$root\"/}; first=${relative%%/*}; checkout=$root/$first; fi; fi; printf '%s\\n' \"$root\" \"$checkout\" \"$start\""
	data, e := Output(ctx, At(Endpoint{Host: host}, script, false))
	if e != nil {
		err = fmt.Errorf("resolve VM checkout: %w", e)
		return
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		err = fmt.Errorf("unexpected VM checkout response; remove shell startup output")
		return
	}
	canonicalRoot, checkout, start = lines[0], lines[1], lines[2]
	if !filepath.IsAbs(canonicalRoot) || !within(canonicalRoot, checkout) || !within(checkout, start) {
		err = fmt.Errorf("VM checkout is outside the workspace root")
	}
	return
}

// WorkspaceProjects lists Git checkouts and worktrees without following directory
// symlinks. The list is discovery only; every execution resolves containment again.
func WorkspaceProjects(ctx context.Context, c Config) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if !WorkspaceRootEnabled(c) {
		return Names(c), nil
	}
	if c.Workspace.Host != "" {
		script := "cd -P " + Quote(c.Workspace.VMRoot) + " && find . \\( -name '.env*' -o -name vendor -o -name node_modules -o -name .claude -o -name .codex -o -name .agents -o -name .opencode -o -name .kimi-code -o -name .serena \\) -prune -o -name .git -prune -print"
		data, err := noninteractiveOutput(ctx, At(Endpoint{Host: c.Workspace.Host}, script, false))
		if err != nil {
			return nil, err
		}
		var result []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			rel := strings.TrimPrefix(strings.TrimSuffix(line, "/.git"), "./")
			if filepath.IsLocal(rel) && rel != "." {
				result = append(result, rel)
			}
		}
		return sortedUniquePaths(result), nil
	}
	root, err := filepath.EvalSymlinks(c.Workspace.VMRoot)
	if err != nil {
		return nil, err
	}
	var result []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Name() == ".git" {
			rel, e := filepath.Rel(root, filepath.Dir(path))
			if e != nil {
				return e
			}
			if rel != "." {
				result = append(result, filepath.ToSlash(rel))
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		if path != root && d.IsDir() && excluded(d.Name()) {
			return filepath.SkipDir
		}
		return nil
	})
	return sortedUniquePaths(result), err
}

// Resolve the existing prefix so lexical aliases cannot conceal source/runner
// overlap. Missing leaf directories are normal before a first runner pull.
func canonicalConfiguredPath(path string) string {
	path = filepath.Clean(path)
	var missing []string
	for current := path; ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved
		}
		if current == filepath.Dir(current) {
			return path
		}
		missing = append(missing, filepath.Base(current))
	}
}
func pathsOverlap(a, b string) bool {
	a = canonicalConfiguredPath(a)
	b = canonicalConfiguredPath(b)
	return within(a, b) || within(b, a)
}

// WorkspaceCheckoutPath distinguishes an explicit path from a legacy project
// alias even when a checkout and configured project have the same name.
func WorkspaceCheckoutPath(c Config, path string) (string, error) {
	if !WorkspaceRootEnabled(c) {
		return "", fmt.Errorf("workspace path selection requires vm_root")
	}
	if !filepath.IsAbs(c.Workspace.VMRoot) {
		return "", fmt.Errorf("run setup to expand the VM workspace root")
	}
	if path == "" || strings.ContainsAny(path, "\n\r\x00") {
		return "", fmt.Errorf("specify a checkout path")
	}
	if !filepath.IsAbs(path) {
		if !filepath.IsLocal(path) {
			return "", fmt.Errorf("checkout path must stay inside workspace")
		}
		path = filepath.Join(c.Workspace.VMRoot, path)
	}
	if !within(filepath.Clean(c.Workspace.VMRoot), filepath.Clean(path)) {
		return "", fmt.Errorf("checkout path is outside the workspace root")
	}
	return filepath.Clean(path), nil
}

func executionDirectory(ctx context.Context, endpoint Endpoint, cwd string) (string, error) {
	if _, err := relativeCommandDirectory(cwd); err != nil {
		return "", err
	}
	if endpoint.Host != "" {
		script := "set -eu; cd -P " + Quote(endpoint.Path) + "; root=$(pwd -P); cd -P " + Quote(filepath.Join(endpoint.Path, cwd)) + "; current=$(pwd -P); case \"$current\" in \"$root\"|\"$root\"/*) ;; *) exit 41;; esac; printf '%s\\n' \"$current\""
		data, err := noninteractiveOutput(ctx, At(endpoint, script, false))
		if err != nil {
			return "", fmt.Errorf("command cwd is unavailable or outside its checkout: %w", err)
		}
		path := strings.TrimSuffix(string(data), "\n")
		if !filepath.IsAbs(path) || strings.ContainsRune(path, '\n') {
			return "", fmt.Errorf("invalid VM command directory response")
		}
		return path, nil
	}
	root, err := filepath.EvalSymlinks(endpoint.Path)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(endpoint.Path, cwd))
	if err != nil {
		return "", err
	}
	if !within(root, path) {
		return "", fmt.Errorf("command cwd symlink escapes its checkout")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("command cwd is not an existing directory")
	}
	return path, nil
}

func sortedUniquePaths(paths []string) []string {
	sort.Strings(paths)
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if len(result) == 0 || result[len(result)-1] != path {
			result = append(result, path)
		}
	}
	return result
}
