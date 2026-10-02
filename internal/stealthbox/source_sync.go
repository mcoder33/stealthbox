package stealthbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type SourceEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Mode uint32 `json:"mode"`
	Hash string `json:"hash,omitempty"`
}
type SourceState struct {
	Hash    string        `json:"hash"`
	Exists  bool          `json:"exists"`
	Entries []SourceEntry `json:"entries,omitempty"`
}
type SourceChange struct {
	Action string `json:"action"`
	Path   string `json:"path"`
}
type SourceSyncPlan struct {
	Version         int            `json:"version"`
	WorkspaceID     string         `json:"workspace_id"`
	Host            string         `json:"host"`
	VMRoot          string         `json:"vm_root"`
	LocalRoot       string         `json:"local_root"`
	RelativePath    string         `json:"relative_path"`
	Direction       string         `json:"direction"`
	Delete          bool           `json:"delete"`
	SourceHash      string         `json:"source_hash"`
	DestinationHash string         `json:"destination_hash"`
	Changes         []SourceChange `json:"changes"`
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

func sourceStateAt(ctx context.Context, root, relative string) (SourceState, error) {
	path, err := checkedSourcePath(root, relative)
	if err != nil {
		return SourceState{}, err
	}
	state := SourceState{Entries: []SourceEntry{}}
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
		if excluded(rel) {
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
		item := SourceEntry{Path: filepath.ToSlash(rel), Mode: uint32(info.Mode().Perm())}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("source sync refuses symlinks: %s", rel)
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
	b, _ := json.Marshal(state.Entries)
	sum := sha256.Sum256(b)
	state.Hash = hex.EncodeToString(sum[:])
	return state, nil
}

// SourceFingerprint is used by the deployed VM binary; no shell embeds source contents.
func SourceFingerprint(ctx context.Context, c Config, path string) (SourceState, error) {
	if c.Workspace.Host != "" {
		return SourceState{}, fmt.Errorf("source fingerprint runs on the VM")
	}
	return sourceStateAt(ctx, c.Workspace.VMRoot, path)
}

func PrepareSourceDestination(ctx context.Context, c Config, path, expected string) error {
	state, err := SourceFingerprint(ctx, c, path)
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

func remoteSourceState(ctx context.Context, c Config, path string) (SourceState, error) {
	if c.Workspace.Host == "" {
		return sourceStateAt(ctx, c.Workspace.VMRoot, path)
	}
	if c.Workspace.RemoteDir == "" {
		return SourceState{}, fmt.Errorf("run setup before source sync")
	}
	args := []string{filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox"), "source", "fingerprint", "--config", filepath.Join(c.Workspace.RemoteDir, "config.json"), "--path", path}
	b, err := Output(ctx, sshCommand(c.Workspace.Host, shellArgs(args), false))
	if err != nil {
		return SourceState{}, fmt.Errorf("remote source fingerprint: %w", err)
	}
	if len(b) > 32<<20 {
		return SourceState{}, fmt.Errorf("remote source manifest exceeds limit")
	}
	var state SourceState
	if err = json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("invalid remote source manifest: %w", err)
	}
	return state, nil
}

func sourceSyncStates(ctx context.Context, c Config, path, direction string) (SourceState, SourceState, error) {
	local, err := sourceStateAt(ctx, c.Workspace.LocalRoot, path)
	if err != nil {
		return SourceState{}, SourceState{}, err
	}
	remote, err := remoteSourceState(ctx, c, path)
	if err != nil {
		return SourceState{}, SourceState{}, err
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
	for _, arg := range command.Args {
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
		args := []string{filepath.Join(c.Workspace.RemoteDir, "bin", "stealthbox"), "source", "prepare", "--config", filepath.Join(c.Workspace.RemoteDir, "config.json"), "--path", path, "--expected-hash", destination.Hash}
		if err = ExecuteContext(ctx, sshCommand(c.Workspace.Host, shellArgs(args), false), nil, io.Discard, w); err != nil {
			return err
		}
	}
	if err = ExecuteContext(ctx, command, nil, w, w); err != nil {
		return err
	}
	fmt.Fprintf(w, "Applied %s %s. Keep source edits paused during an apply.\n", plan.Direction, path)
	return nil
}
