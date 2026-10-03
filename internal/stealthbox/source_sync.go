package stealthbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type SourceEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Hash   string `json:"hash,omitempty"`
	Target string `json:"target,omitempty"`
}
type SourceState struct {
	Hash            string                `json:"hash"`
	Exists          bool                  `json:"exists"`
	Entries         []SourceEntry         `json:"entries,omitempty"`
	IgnoredPaths    []string              `json:"ignored_paths,omitempty"`
	IgnoreRules     SourceIgnoreRules     `json:"-"`
	GitRepositories []SourceGitRepository `json:"git_repositories,omitempty"`
}
type SourceChange struct {
	Action string `json:"action"`
	Path   string `json:"path"`
}
type SourceSyncPlan struct {
	Version              int                   `json:"version"`
	WorkspaceID          string                `json:"workspace_id"`
	Host                 string                `json:"host"`
	VMRoot               string                `json:"vm_root"`
	LocalRoot            string                `json:"local_root"`
	RelativePath         string                `json:"relative_path"`
	Direction            string                `json:"direction"`
	Delete               bool                  `json:"delete"`
	SourceHash           string                `json:"source_hash"`
	DestinationHash      string                `json:"destination_hash"`
	Changes              []SourceChange        `json:"changes"`
	SourceExcludes       []string              `json:"source_excludes,omitempty"`
	SourceSafeLinks      bool                  `json:"source_safe_links,omitempty"`
	SourceGitIgnore      bool                  `json:"source_gitignore,omitempty"`
	SourceGit            bool                  `json:"source_git,omitempty"`
	GitBootstrap         []SourceGitRepository `json:"git_bootstrap,omitempty"`
	GitIgnoreIncludeFile string                `json:"-"`
}

func sourceRelativePath(path string) (string, error) {
	clean := filepath.Clean(path)
	if path == "" || clean == "." || filepath.IsAbs(path) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.ContainsAny(path, "\x00\n\r:") {
		return "", fmt.Errorf("source sync requires a checkout path relative to the workspace root, without ..")
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return "", fmt.Errorf("source path contains ..")
		}
	}
	if excluded(clean) {
		return "", fmt.Errorf("source checkout path is excluded from synchronization")
	}
	return clean, nil
}

func checkedSourcePath(root, relative string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return "", fmt.Errorf("configure an absolute workspace source root; run setup to expand VM ~")
	}
	relative, err := sourceRelativePath(relative)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace source root: %w", err)
	}
	path := filepath.Join(canonical, relative)
	if !within(canonical, path) {
		return "", fmt.Errorf("source path escapes workspace")
	}
	current := canonical
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("source path contains symlink: %s", relative)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("source checkout path must be a directory")
		}
	}
	return path, nil
}

func sourceStateAt(ctx context.Context, root, relative string, sourceExcludes ...string) (SourceState, error) {
	return sourceStateAtWithOptions(ctx, root, relative, sourceExcludes, false)
}

func sourceStateAtWithOptions(ctx context.Context, root, relative string, sourceExcludes []string, safeLinks bool, overrides ...SourceIgnoreRules) (SourceState, error) {
	excludes, err := checkoutSourceExcludes(filepath.Clean(relative), sourceExcludes)
	if err != nil {
		return SourceState{}, err
	}
	var rules SourceIgnoreRules
	if len(overrides) > 0 {
		rules = overrides[0]
	} else {
		files, err := sourceIgnoreFiles(ctx, root, relative, sourceExcludes)
		if err != nil {
			return SourceState{}, err
		}
		rules = SourceIgnoreRules{files}
	}
	matchers, err := newSourceIgnoreMatchers(ctx, rules)
	if err != nil {
		return SourceState{}, err
	}
	defer matchers.Close()
	ignoredPath := func(path string, isDir bool) (bool, error) {
		return matchers.Ignored(filepath.Join(relative, path), isDir)
	}
	if ignored, err := matchers.Ignored(relative, true); err != nil {
		return SourceState{}, err
	} else if ignored {
		return SourceState{}, fmt.Errorf("selected source checkout is ignored by .gitignore")
	}
	path, err := checkedSourcePath(root, relative)
	if err != nil {
		return SourceState{}, err
	}
	state := SourceState{Entries: []SourceEntry{}, IgnoreRules: rules}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		state.Hash = "missing"
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.Exists = true
	state.Entries = append(state.Entries, SourceEntry{Path: ".", Kind: "dir", Mode: uint32(info.Mode().Perm())})
	err = filepath.WalkDir(path, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if file == path {
			return nil
		}
		rel, err := filepath.Rel(path, file)
		if err != nil {
			return err
		}
		if excluded(rel) || sourcePathExcluded(rel, excludes) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		ignored, err := ignoredPath(rel, entry.IsDir())
		if err != nil {
			return err
		}
		if ignored {
			state.IgnoredPaths = append(state.IgnoredPaths, filepath.ToSlash(rel))
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.ContainsAny(rel, "\x00\n\r") {
			return fmt.Errorf("unsupported source filename")
		}
		if len(state.Entries) >= 100000 {
			return fmt.Errorf("source manifest exceeds 100000 entries")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := SourceEntry{Path: filepath.ToSlash(rel), Mode: sourceEntryMode(info.Mode())}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if !safeLinks {
				return fmt.Errorf("source sync refuses symlinks: %s", rel)
			}
			item.Kind = "symlink"
			item.Target, err = os.Readlink(file)
			if err != nil {
				return err
			}
			if err = validateSourceLink(ctx, path, file, item.Target, excludes, ignoredPath); err != nil {
				return fmt.Errorf("unsafe source symlink %s: %w", rel, err)
			}
		case info.IsDir():
			item.Kind = "dir"
		case info.Mode().IsRegular():
			item.Kind = "file"
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			item.Hash = hex.EncodeToString(h.Sum(nil))
		default:
			return fmt.Errorf("source sync refuses special files: %s", rel)
		}
		state.Entries = append(state.Entries, item)
		return nil
	})
	if err != nil {
		return state, err
	}
	b, _ := json.Marshal(struct {
		Entries []SourceEntry
		Rules   SourceIgnoreRules
	}{state.Entries, rules})
	state.GitRepositories, err = sourceGitRepositories(ctx, path, state.Entries)
	if err != nil {
		return state, err
	}
	if len(state.GitRepositories) > 0 {
		metadata, _ := json.Marshal(state.GitRepositories)
		b = append(b, metadata...)
	}
	sum := sha256.Sum256(b)
	state.Hash = hex.EncodeToString(sum[:])
	return state, nil
}

func sourceEntryMode(mode fs.FileMode) uint32 {
	// Symlink permissions differ on macOS/Linux and are not synchronized by rsync.
	// A canonical zero keeps identical links equal in manifests and change plans.
	if mode&fs.ModeSymlink != 0 {
		return 0
	}
	return uint32(mode.Perm())
}

// Resolve components without traversing a symlink before checking its target.
// This also checks intermediate aliases, not just the final EvalSymlinks result.
func validateSourceLink(ctx context.Context, root, file, target string, excludes []string, ignore ...func(string, bool) (bool, error)) error {
	checkTarget := func(parent, target string) error {
		if filepath.IsAbs(target) || target == "" || strings.ContainsAny(target, "\x00\n\r") {
			return fmt.Errorf("link target must be a relative path")
		}
		lexical := filepath.Join(parent, target)
		if !within(root, lexical) {
			return fmt.Errorf("link target escapes the selected source tree")
		}
		relative, err := filepath.Rel(root, lexical)
		if err != nil {
			return err
		}
		if excluded(relative) || sourcePathExcluded(relative, excludes) {
			return fmt.Errorf("link target is excluded from source sync")
		}
		return nil
	}
	parent := filepath.Dir(file)
	if err := checkTarget(parent, target); err != nil {
		return err
	}
	parentRelative, err := filepath.Rel(root, parent)
	if err != nil {
		return err
	}
	pending := strings.Split(parentRelative+string(filepath.Separator)+target, string(filepath.Separator))
	current := root
	links := 0
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if current == root {
				return fmt.Errorf("link target escapes the selected source tree")
			}
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, part)
		relative, err := filepath.Rel(root, next)
		if err != nil {
			return err
		}
		if excluded(relative) || sourcePathExcluded(relative, excludes) {
			return fmt.Errorf("link target traverses an excluded path")
		}
		info, err := os.Lstat(next)
		if err != nil {
			return fmt.Errorf("link target cannot be resolved: %w", err)
		}
		if len(ignore) > 0 {
			ignored, err := ignore[0](relative, info.IsDir())
			if err != nil {
				return err
			}
			if ignored {
				return fmt.Errorf("link target is excluded by .gitignore")
			}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 255 {
				return fmt.Errorf("link target is cyclic or has too many symlinks")
			}
			linkTarget, err := os.Readlink(next)
			if err != nil {
				return err
			}
			if err = checkTarget(current, linkTarget); err != nil {
				return err
			}
			pending = append(strings.Split(linkTarget, string(filepath.Separator)), pending...)
			continue
		}
		if !info.IsDir() && (!info.Mode().IsRegular() || len(pending) > 0) {
			return fmt.Errorf("link target must resolve through directories to a file or directory")
		}
		current = next
	}
	return nil
}

// SourceFingerprint is used by the deployed VM binary; no shell embeds source contents.
func SourceFingerprint(ctx context.Context, c Config, path string, rules ...SourceIgnoreRules) (SourceState, error) {
	if c.Workspace.Host != "" {
		return SourceState{}, fmt.Errorf("source fingerprint runs on the VM")
	}
	return sourceStateAtWithOptions(ctx, c.Workspace.VMRoot, path, c.Workspace.SourceExcludes, c.Workspace.SourceSafeLinks, rules...)
}

func PrepareSourceDestination(ctx context.Context, c Config, path, expected string, rules ...SourceIgnoreRules) error {
	state, err := SourceFingerprint(ctx, c, path, rules...)
	if err != nil {
		return err
	}
	if expected == "" || state.Hash != expected {
		return fmt.Errorf("source destination changed since preview; generate a new plan")
	}
	destination, err := checkedSourcePath(c.Workspace.VMRoot, path)
	if err != nil {
		return err
	}
	return os.MkdirAll(destination, 0700)
}

func remoteSourceState(ctx context.Context, c Config, path string, rules ...SourceIgnoreRules) (SourceState, error) {
	if c.Workspace.Host == "" {
		return sourceStateAtWithOptions(ctx, c.Workspace.VMRoot, path, c.Workspace.SourceExcludes, c.Workspace.SourceSafeLinks, rules...)
	}
	if c.Workspace.RemoteDir == "" {
		return SourceState{}, fmt.Errorf("run setup before source sync")
	}
	args := remoteSourceArgs(c, "fingerprint", path)
	var input io.Reader
	if len(rules) > 0 {
		args = append(args, "--source-ignore-rules-stdin")
		data, err := json.Marshal(rules[0])
		if err != nil || len(data) > sourceIgnoreLimit {
			return SourceState{}, fmt.Errorf("source .gitignore rules exceed 1 MiB")
		}
		input = bytes.NewReader(data)
	}
	var output, details bytes.Buffer
	err := ExecuteNoninteractiveContext(ctx, sshCommand(c.Workspace.Host, shellArgs(args), false), input, &output, &details)
	b := output.Bytes()
	if err != nil {
		return SourceState{}, fmt.Errorf("remote source fingerprint: %w: %s", err, details.String())
	}
	if len(b) > 32<<20 {
		return SourceState{}, fmt.Errorf("remote source manifest exceeds limit")
	}
	var state SourceState
	if err = json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("invalid remote source manifest: %w", err)
	}
	if len(rules) > 0 {
		state.IgnoreRules = rules[0]
	}
	return state, nil
}

func remoteSourceArgs(c Config, action, path string) []string {
	// Always override the deployed config, including an explicitly empty list.
	excludes, _ := canonicalSourceExcludes(c.Workspace.SourceExcludes)
	return []string{filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox"), "source", action, "--config", filepath.Join(c.Workspace.RemoteDir, "config.json"), "--path", path, "--vm-root", c.Workspace.VMRoot, "--source-excludes", JSONString(excludes), "--source-safe-links=" + strconv.FormatBool(c.Workspace.SourceSafeLinks)}
}

func sourceSyncStates(ctx context.Context, c Config, path, direction string) (SourceState, SourceState, error) {
	localFiles, err := sourceIgnoreFiles(ctx, c.Workspace.LocalRoot, path, c.Workspace.SourceExcludes)
	if err != nil {
		return SourceState{}, SourceState{}, err
	}
	remoteFiles, err := remoteSourceIgnoreFiles(ctx, c, path)
	if err != nil {
		return SourceState{}, SourceState{}, err
	}
	rules := SourceIgnoreRules{localFiles, remoteFiles}
	local, err := sourceStateAtWithOptions(ctx, c.Workspace.LocalRoot, path, c.Workspace.SourceExcludes, c.Workspace.SourceSafeLinks, rules)
	if err != nil {
		return SourceState{}, SourceState{}, err
	}
	remote, err := remoteSourceState(ctx, c, path, rules)
	if err != nil {
		return SourceState{}, SourceState{}, err
	}
	// A directory-only ignore may allow a regular file at the same path on
	// the other side. Reject that collision before rsync can replace the
	// receiver's protected directory with the sender's file.
	for _, pair := range [][2]SourceState{{local, remote}, {remote, local}} {
		ignored := make(map[string]bool, len(pair[0].IgnoredPaths))
		for _, path := range pair[0].IgnoredPaths {
			ignored[path] = true
		}
		for _, entry := range pair[1].Entries {
			if entry.Path != "." && (!filepath.IsLocal(entry.Path) || filepath.ToSlash(filepath.Clean(entry.Path)) != entry.Path || strings.ContainsAny(entry.Path, "\x00\n\r")) {
				return SourceState{}, SourceState{}, fmt.Errorf("invalid source manifest path %q", entry.Path)
			}
			for path := entry.Path; path != "."; path = filepath.ToSlash(filepath.Dir(path)) {
				if ignored[path] {
					return SourceState{}, SourceState{}, fmt.Errorf("source path %s conflicts with an ignored path on the other side; reconcile the path types or .gitignore rules first", entry.Path)
				}
			}
		}
	}
	if direction == "import" {
		return local, remote, nil
	}
	if direction == "export" {
		return remote, local, nil
	}
	return SourceState{}, SourceState{}, fmt.Errorf("source direction must be import (local → VM) or export (VM → local)")
}

func sourceChanges(source, destination SourceState, remove bool) []SourceChange {
	old := map[string]SourceEntry{}
	for _, e := range destination.Entries {
		old[e.Path] = e
	}
	changes := []SourceChange{}
	for _, e := range source.Entries {
		previous, exists := old[e.Path]
		if !exists {
			changes = append(changes, SourceChange{"add", e.Path})
		} else if previous != e {
			changes = append(changes, SourceChange{"update", e.Path})
		}
		delete(old, e.Path)
	}
	if remove {
		for path := range old {
			changes = append(changes, SourceChange{"delete", path})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

func validateSourceSyncConfig(c Config) error {
	if !WorkspaceRootEnabled(c) || c.Workspace.LocalRoot == "" {
		return fmt.Errorf("source sync needs vm_root and local_root")
	}
	if !filepath.IsAbs(c.Workspace.VMRoot) {
		return fmt.Errorf("run setup to expand the VM root")
	}
	if c.Workspace.Host == "" {
		local, err := filepath.EvalSymlinks(c.Workspace.LocalRoot)
		if err != nil {
			return err
		}
		vm, err := filepath.EvalSymlinks(c.Workspace.VMRoot)
		if err != nil {
			return err
		}
		if within(local, vm) || within(vm, local) {
			return fmt.Errorf("local and VM source roots must not overlap on the same machine")
		}
	}
	return c.Validate()
}

func PreviewSourceSync(ctx context.Context, c Config, direction, path string, remove bool, planFile string, w io.Writer) error {
	if err := validateSourceSyncConfig(c); err != nil {
		return err
	}
	if err := EnsureWorkspaceIdentity(&c); err != nil {
		return err
	}
	path, err := sourceRelativePath(path)
	if err != nil {
		return err
	}
	if planFile == "" {
		return fmt.Errorf("provide --plan with a new local plan file")
	}
	localCheckout, err := checkedSourcePath(c.Workspace.LocalRoot, path)
	if err != nil {
		return err
	}
	canonicalPlan, err := canonicalSourcePlanPath(planFile)
	if err != nil {
		return err
	}
	if within(localCheckout, canonicalPlan) {
		return fmt.Errorf("store the source plan outside the synchronized checkout")
	}
	source, destination, err := sourceSyncStates(ctx, c, path, direction)
	if err != nil {
		return err
	}
	if !source.Exists {
		return fmt.Errorf("selected source checkout does not exist")
	}
	plan := SourceSyncPlan{Version: 1, WorkspaceID: c.Workspace.ID, Host: c.Workspace.Host, VMRoot: c.Workspace.VMRoot, LocalRoot: c.Workspace.LocalRoot, RelativePath: path, Direction: direction, Delete: remove, SourceHash: source.Hash, DestinationHash: destination.Hash, Changes: sourceChanges(source, destination, remove)}
	plan.SourceExcludes, _ = canonicalSourceExcludes(c.Workspace.SourceExcludes)
	plan.SourceSafeLinks = c.Workspace.SourceSafeLinks
	plan.SourceGitIgnore = true
	plan.SourceGit = true
	plan.GitBootstrap = sourceGitBootstrap(source, destination, direction)
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return fmt.Errorf("source plan exceeds 1 MiB; select a smaller checkout")
	}
	f, err := os.OpenFile(planFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	closeErr := f.Close()
	if err != nil {
		os.Remove(planFile)
		return err
	}
	if closeErr != nil {
		os.Remove(planFile)
		return closeErr
	}
	fmt.Fprintf(w, "Preview %s %s: %d changes; delete=%t. Source files were not changed.\n", direction, path, len(plan.Changes), remove)
	for _, change := range plan.Changes {
		fmt.Fprintf(w, "%s\t%s\n", change.Action, change.Path)
	}
	for _, repo := range plan.GitBootstrap {
		fmt.Fprintf(w, "git-init\t%s (history and branches; existing VM repositories are preserved)\n", repo.Path)
	}
	fmt.Fprintf(w, "Review %s, then: stealthbox source apply --plan %s\n", planFile, Quote(planFile))
	return nil
}

func canonicalSourcePlanPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var missing []string
	for current := path; ; current = filepath.Dir(current) {
		canonical, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				canonical = filepath.Join(canonical, missing[i])
			}
			return canonical, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if current == filepath.Dir(current) {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
	}
}

func sourceSyncCommand(c Config, plan SourceSyncPlan) (Command, error) {
	excludes, err := checkoutSourceExcludes(plan.RelativePath, c.Workspace.SourceExcludes)
	if err != nil {
		return Command{}, err
	}
	local, err := checkedSourcePath(c.Workspace.LocalRoot, plan.RelativePath)
	if err != nil {
		return Command{}, err
	}
	vm := filepath.Join(c.Workspace.VMRoot, plan.RelativePath)
	if c.Workspace.Host != "" && !remotePathRE.MatchString(vm) {
		return Command{}, fmt.Errorf("remote source sync paths must use safe path characters")
	}
	if c.Workspace.Host == "" {
		vm, err = checkedSourcePath(c.Workspace.VMRoot, plan.RelativePath)
		if err != nil {
			return Command{}, err
		}
	}
	source, destination := Endpoint{Path: local}, Endpoint{Host: c.Workspace.Host, Path: vm}
	if plan.Direction == "export" {
		source, destination = destination, source
	}
	command := runnerRsyncCommand(source, destination)
	args := []string{"--omit-dir-times", "--itemize-changes"}
	for _, exclude := range excludes {
		// Anchored literal filters match a file or directory and protect it from --delete.
		args = append(args, "--exclude=/"+exclude)
	}
	if plan.SourceGitIgnore {
		// Only the current manifests may participate in this transfer. This also
		// protects newly created ignored files between fingerprinting and rsync.
		if plan.GitIgnoreIncludeFile == "" {
			return Command{}, fmt.Errorf("source .gitignore transfer requires current manifests")
		}
		args = append(args, "--include-from="+plan.GitIgnoreIncludeFile, "--exclude=*")
	}
	for _, arg := range command.Args {
		if arg == "--no-links" && c.Workspace.SourceSafeLinks {
			args = append(args, "--links")
			continue
		}
		if arg == "--delete-delay" && !plan.Delete {
			continue
		}
		args = append(args, arg)
	}
	command.Args = args
	return command, nil
}

func ApplySourceSync(ctx context.Context, c Config, planFile string, w io.Writer) error {
	if err := validateSourceSyncConfig(c); err != nil {
		return err
	}
	if err := EnsureWorkspaceIdentity(&c); err != nil {
		return err
	}
	f, err := os.Open(planFile)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	f.Close()
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return fmt.Errorf("source plan exceeds limit")
	}
	var plan SourceSyncPlan
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&plan); err != nil {
		return err
	}
	if plan.Version != 1 || plan.WorkspaceID != c.Workspace.ID || plan.Host != c.Workspace.Host || plan.VMRoot != c.Workspace.VMRoot || plan.LocalRoot != c.Workspace.LocalRoot {
		return fmt.Errorf("source plan belongs to a different workspace configuration")
	}
	if !plan.SourceGitIgnore {
		return fmt.Errorf("source .gitignore policy changed; generate a new plan")
	}
	if !plan.SourceGit {
		return fmt.Errorf("source Git policy changed; generate a new plan")
	}
	currentExcludes, _ := canonicalSourceExcludes(c.Workspace.SourceExcludes)
	planExcludes, err := canonicalSourceExcludes(plan.SourceExcludes)
	if err != nil {
		return err
	}
	if !slices.Equal(currentExcludes, planExcludes) {
		return fmt.Errorf("source exclusions changed since preview; generate a new plan")
	}
	if plan.SourceSafeLinks != c.Workspace.SourceSafeLinks {
		return fmt.Errorf("source safe link mode changed since preview; generate a new plan")
	}
	path, err := sourceRelativePath(plan.RelativePath)
	if err != nil {
		return err
	}
	if path != plan.RelativePath {
		return fmt.Errorf("source plan path must be canonical")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	lockDir := filepath.Join(cache, "stealthbox", "source-locks")
	if err = os.MkdirAll(lockDir, 0700); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(c.Workspace.ID + ":" + path))
	lockFile := filepath.Join(lockDir, fmt.Sprintf("%x.lock", hash[:]))
	lock, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("source apply lock exists; another apply may be running: %w", err)
	}
	lock.Close()
	defer os.Remove(lockFile)
	source, destination, err := sourceSyncStates(ctx, c, path, plan.Direction)
	if err != nil {
		return err
	}
	if !source.Exists || source.Hash != plan.SourceHash || destination.Hash != plan.DestinationHash {
		return fmt.Errorf("source or destination changed since preview; nothing applied; generate a new plan")
	}
	if JSONString(sourceChanges(source, destination, plan.Delete)) != JSONString(plan.Changes) {
		return fmt.Errorf("source plan change list does not match its fingerprints")
	}
	if JSONString(sourceGitBootstrap(source, destination, plan.Direction)) != JSONString(plan.GitBootstrap) {
		return fmt.Errorf("source Git bootstrap list does not match its fingerprints")
	}
	bundles, cleanup, err := prepareSourceGitBundles(ctx, c, path, plan.GitBootstrap)
	if err != nil {
		return err
	}
	defer cleanup()
	plan.GitIgnoreIncludeFile, err = sourceIgnoreIncludeFile(source, destination)
	if err != nil {
		return err
	}
	defer os.Remove(plan.GitIgnoreIncludeFile)
	command, err := sourceSyncCommand(c, plan)
	if err != nil {
		return err
	}
	if plan.Direction == "export" {
		path, err := checkedSourcePath(c.Workspace.LocalRoot, path)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(path, 0700); err != nil {
			return err
		}
	} else if c.Workspace.Host == "" {
		path, err := checkedSourcePath(c.Workspace.VMRoot, path)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(path, 0700); err != nil {
			return err
		}
	} else {
		args := append(remoteSourceArgs(c, "prepare", path), "--expected-hash", destination.Hash)
		args = append(args, "--source-ignore-rules-stdin")
		if err = ExecuteContext(ctx, sshCommand(c.Workspace.Host, shellArgs(args), false), strings.NewReader(JSONString(source.IgnoreRules)), io.Discard, w); err != nil {
			return err
		}
	}
	if err = ExecuteContext(ctx, command, nil, w, w); err != nil {
		return err
	}
	if err = applySourceGitBundles(ctx, c, path, plan.GitBootstrap, bundles, w); err != nil {
		return fmt.Errorf("source files transferred, but Git bootstrap failed: %w", err)
	}
	fmt.Fprintf(w, "Applied %s %s. Keep source edits paused during an apply.\n", plan.Direction, path)
	return nil
}
