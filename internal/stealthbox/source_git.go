package stealthbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Only portable repository data crosses the source boundary. Hooks, local
// credentials, the index, reflogs and stash stay on their original machine.
type SourceGitReference struct {
	Name     string `json:"name"`
	OID      string `json:"oid"`
	Symbolic string `json:"symbolic,omitempty"`
	Remote   string `json:"remote,omitempty"`
	Merge    string `json:"merge,omitempty"`
}
type SourceGitRemote struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	PushURL string `json:"push_url,omitempty"`
}
type SourceGitRepository struct {
	Path         string               `json:"path"`
	Head         string               `json:"head,omitempty"`
	Branch       string               `json:"branch,omitempty"`
	ObjectFormat string               `json:"object_format"`
	References   []SourceGitReference `json:"references,omitempty"`
	Remotes      []SourceGitRemote    `json:"remotes,omitempty"`
	UserName     string               `json:"user_name,omitempty"`
	UserEmail    string               `json:"user_email,omitempty"`
}

func sourceGitCommand(ctx context.Context, directory string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false", "-c", "gc.auto=0", "-C", directory}, args...)...)
	// A managed panel can carry Git helper/config variables for a different
	// checkout. Source inspection must use the actual repository instead.
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env, "GIT_TERMINAL_PROMPT=0")
	return command
}

func sourceGitValue(ctx context.Context, directory string, args ...string) string {
	out, err := sourceGitCommand(ctx, directory, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var gitSCPURL = regexp.MustCompile(`^([a-zA-Z0-9_.-]+@)?(\[[^]]+\]|[^/@:\s]+):[^\x00\n\r]+$`)

func portableGitURL(raw string) string {
	if gitSCPURL.MatchString(raw) && !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		return u.String()
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

func sourceGitRepository(ctx context.Context, directory, path string) (SourceGitRepository, error) {
	r := SourceGitRepository{Path: path, ObjectFormat: sourceGitValue(ctx, directory, "rev-parse", "--show-object-format")}
	if r.ObjectFormat != "sha1" && r.ObjectFormat != "sha256" {
		return r, fmt.Errorf("invalid Git checkout: %s", path)
	}
	r.Head = sourceGitValue(ctx, directory, "rev-parse", "--verify", "HEAD")
	r.Branch = sourceGitValue(ctx, directory, "symbolic-ref", "-q", "HEAD")
	r.UserName = sourceGitValue(ctx, directory, "config", "user.name")
	r.UserEmail = sourceGitValue(ctx, directory, "config", "user.email")
	out, err := sourceGitCommand(ctx, directory, "for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)", "refs/heads", "refs/tags", "refs/remotes").Output()
	if err != nil {
		return r, fmt.Errorf("cannot inspect Git references: %s", path)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			return r, fmt.Errorf("invalid Git reference metadata")
		}
		ref := SourceGitReference{Name: parts[0], OID: parts[1], Symbolic: parts[2]}
		if branch, ok := strings.CutPrefix(ref.Name, "refs/heads/"); ok {
			ref.Remote = sourceGitValue(ctx, directory, "config", "branch."+branch+".remote")
			ref.Merge = sourceGitValue(ctx, directory, "config", "branch."+branch+".merge")
		}
		r.References = append(r.References, ref)
	}
	for _, name := range strings.Fields(sourceGitValue(ctx, directory, "remote")) {
		r.Remotes = append(r.Remotes, SourceGitRemote{Name: name, URL: portableGitURL(sourceGitValue(ctx, directory, "config", "remote."+name+".url")), PushURL: portableGitURL(sourceGitValue(ctx, directory, "config", "remote."+name+".pushurl"))})
	}
	return r, ctx.Err()
}

func sourceGitRepositories(ctx context.Context, checkout string, entries []SourceEntry) ([]SourceGitRepository, error) {
	var repos []SourceGitRepository
	for _, entry := range entries {
		if entry.Kind != "dir" {
			continue
		}
		directory := filepath.Join(checkout, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(filepath.Join(directory, ".git"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return nil, fmt.Errorf("unsupported .git entry: %s", entry.Path)
		}
		// An excluded .git directory can also be a placeholder in a source
		// export. Only a real Git metadata directory (or worktree file) is
		// bootstrapped; malformed real repositories still fail inspection.
		if info.IsDir() {
			if _, e := os.Stat(filepath.Join(directory, ".git", "HEAD")); os.IsNotExist(e) {
				continue
			}
		}
		r, err := sourceGitRepository(ctx, directory, entry.Path)
		if err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, nil
}

func sourceGitBootstrap(source, destination SourceState, direction string) []SourceGitRepository {
	var result []SourceGitRepository
	if direction != "import" {
		return result
	}
	for _, r := range source.GitRepositories {
		if !slices.ContainsFunc(destination.GitRepositories, func(other SourceGitRepository) bool { return r.Path == other.Path }) {
			result = append(result, r)
		}
	}
	return result
}

func prepareSourceGitBundles(ctx context.Context, c Config, relative string, repos []SourceGitRepository) ([]string, func(), error) {
	cleanup := func() {}
	if len(repos) == 0 {
		return nil, cleanup, nil
	}
	root, err := checkedSourcePath(c.Workspace.LocalRoot, relative)
	if err != nil {
		return nil, cleanup, err
	}
	temporary, err := os.MkdirTemp("", "stealthbox-git-bundles-")
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { os.RemoveAll(temporary) }
	bundles := make([]string, len(repos))
	for i, r := range repos {
		directory := filepath.Join(root, filepath.FromSlash(r.Path))
		if r.Head != "" {
			bundles[i] = filepath.Join(temporary, fmt.Sprintf("%d.bundle", i))
			args := []string{"bundle", "create", bundles[i]}
			for _, ref := range r.References {
				args = append(args, ref.Name)
			}
			args = append(args, "HEAD")
			if err = sourceGitCommand(ctx, directory, args...).Run(); err != nil {
				cleanup()
				return nil, func() {}, fmt.Errorf("cannot prepare Git history: %s", r.Path)
			}
		}
		current, e := sourceGitRepository(ctx, directory, r.Path)
		if e != nil || JSONString(current) != JSONString(r) {
			cleanup()
			return nil, func() {}, fmt.Errorf("Git repository changed since preview: %s; generate a new plan", r.Path)
		}
	}
	return bundles, cleanup, nil
}

func applySourceGitBundles(ctx context.Context, c Config, relative string, repos []SourceGitRepository, bundles []string, w io.Writer) error {
	for i, r := range repos {
		var input io.Reader = strings.NewReader(JSONString(r) + "\n")
		var file *os.File
		if bundles[i] != "" {
			var err error
			file, err = os.Open(bundles[i])
			if err != nil {
				return err
			}
			input = io.MultiReader(input, file)
		}
		path := filepath.ToSlash(filepath.Join(relative, r.Path))
		var err error
		if c.Workspace.Host == "" {
			err = BootstrapSourceGit(ctx, c, path, input)
		} else {
			err = ExecuteContext(ctx, sshCommand(c.Workspace.Host, shellArgs(remoteSourceArgs(c, "git-bootstrap", path)), false), input, io.Discard, w)
		}
		if file != nil {
			file.Close()
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "Initialized Git history: %s\n", path)
	}
	return nil
}

var sourceGitOID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func validateSourceGitRepository(ctx context.Context, r SourceGitRepository, directory string) error {
	if r.ObjectFormat != "sha1" && r.ObjectFormat != "sha256" {
		return fmt.Errorf("invalid Git object format")
	}
	validOID := func(s string) bool {
		return sourceGitOID.MatchString(s) && ((r.ObjectFormat == "sha1" && len(s) == 40) || (r.ObjectFormat == "sha256" && len(s) == 64))
	}
	validRef := func(s string) bool {
		if !strings.HasPrefix(s, "refs/heads/") && !strings.HasPrefix(s, "refs/tags/") && !strings.HasPrefix(s, "refs/remotes/") {
			return false
		}
		return sourceGitCommand(ctx, directory, "check-ref-format", s).Run() == nil
	}
	if r.Head != "" && !validOID(r.Head) {
		return fmt.Errorf("invalid Git HEAD")
	}
	if r.Branch != "" && (!strings.HasPrefix(r.Branch, "refs/heads/") || !validRef(r.Branch)) {
		return fmt.Errorf("invalid Git branch")
	}
	for _, ref := range r.References {
		if !validRef(ref.Name) || !validOID(ref.OID) || (ref.Symbolic != "" && !validRef(ref.Symbolic)) || (ref.Merge != "" && !validRef(ref.Merge)) || strings.ContainsAny(ref.Remote, "\x00\n\r") {
			return fmt.Errorf("invalid Git reference")
		}
	}
	for _, remote := range r.Remotes {
		if remote.Name == "" || strings.HasPrefix(remote.Name, "-") || strings.ContainsAny(remote.Name, "\x00\n\r /\\") || strings.ContainsAny(remote.URL+remote.PushURL, "\x00\n\r") || portableGitURL(remote.URL) != remote.URL || portableGitURL(remote.PushURL) != remote.PushURL {
			return fmt.Errorf("invalid Git remote")
		}
	}
	if strings.ContainsAny(r.UserName+r.UserEmail, "\x00\n\r") {
		return fmt.Errorf("invalid Git author")
	}
	return nil
}

// BootstrapSourceGit receives a metadata header and optional bundle. Existing
// VM repositories are never overwritten, including when another process races
// the final installation. The working tree is not checked out or reset.
func BootstrapSourceGit(ctx context.Context, c Config, relative string, input io.Reader) error {
	directory, err := checkedSourcePath(c.Workspace.VMRoot, relative)
	if err != nil {
		return err
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return fmt.Errorf("Git destination directory does not exist")
	}
	if _, err = os.Lstat(filepath.Join(directory, ".git")); !os.IsNotExist(err) {
		return fmt.Errorf("Git destination already exists or cannot be inspected")
	}
	reader := bufio.NewReaderSize(input, (1<<20)+1)
	header, err := reader.ReadSlice('\n')
	if err != nil || len(header) > 1<<20 {
		return fmt.Errorf("invalid or oversized Git metadata")
	}
	var r SourceGitRepository
	decoder := json.NewDecoder(strings.NewReader(string(header)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&r); err != nil {
		return fmt.Errorf("invalid Git metadata")
	}
	if err = validateSourceGitRepository(ctx, r, directory); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(directory, ".stealthbox-git-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	metadata := filepath.Join(temporary, "repository")
	if err = sourceGitCommand(ctx, directory, "init", "--bare", "--template=", "--object-format="+r.ObjectFormat, metadata).Run(); err != nil {
		return fmt.Errorf("cannot initialize Git metadata")
	}
	git := func(args ...string) *exec.Cmd {
		return sourceGitCommand(ctx, directory, append([]string{"--git-dir=" + metadata, "--work-tree=" + directory}, args...)...)
	}
	limit := c.Bridge.MaxBytes
	if limit <= 0 {
		limit = 1 << 30
	}
	if r.Head != "" {
		bundle := filepath.Join(temporary, "history.bundle")
		f, e := os.OpenFile(bundle, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		n, copyErr := io.Copy(f, io.LimitReader(reader, limit+1))
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n > limit {
			return fmt.Errorf("Git bundle exceeds transfer limit")
		}
		if err = git("bundle", "verify", bundle).Run(); err != nil {
			return fmt.Errorf("invalid Git bundle")
		}
		if err = git("bundle", "unbundle", bundle).Run(); err != nil {
			return fmt.Errorf("cannot import Git objects")
		}
		if err = git("cat-file", "-e", r.Head+"^{commit}").Run(); err != nil {
			return fmt.Errorf("Git HEAD object is missing")
		}
	}
	config := func(key, value string) error { return git("config", "--local", key, value).Run() }
	if err = config("core.bare", "false"); err != nil {
		return err
	}
	for _, ref := range r.References {
		if ref.Symbolic != "" {
			err = git("symbolic-ref", ref.Name, ref.Symbolic).Run()
		} else {
			err = git("update-ref", ref.Name, ref.OID).Run()
		}
		if err != nil {
			return fmt.Errorf("cannot restore Git references")
		}
		if branch, ok := strings.CutPrefix(ref.Name, "refs/heads/"); ok {
			if ref.Remote != "" {
				if err = config("branch."+branch+".remote", ref.Remote); err != nil {
					return err
				}
			}
			if ref.Merge != "" {
				if err = config("branch."+branch+".merge", ref.Merge); err != nil {
					return err
				}
			}
		}
	}
	if r.Branch != "" {
		err = git("symbolic-ref", "HEAD", r.Branch).Run()
	} else if r.Head != "" {
		err = git("update-ref", "--no-deref", "HEAD", r.Head).Run()
	}
	if err != nil {
		return fmt.Errorf("cannot restore Git HEAD")
	}
	for _, remote := range r.Remotes {
		if err = config("remote."+remote.Name+".url", remote.URL); err != nil {
			return err
		}
		if err = config("remote."+remote.Name+".fetch", "+refs/heads/*:refs/remotes/"+remote.Name+"/*"); err != nil {
			return err
		}
		if remote.PushURL != "" {
			if err = config("remote."+remote.Name+".pushurl", remote.PushURL); err != nil {
				return err
			}
		}
	}
	if r.UserName != "" {
		if err = config("user.name", r.UserName); err != nil {
			return err
		}
	}
	if r.UserEmail != "" {
		if err = config("user.email", r.UserEmail); err != nil {
			return err
		}
	}
	if r.Head != "" {
		if err = git("read-tree", r.Head).Run(); err != nil {
			return fmt.Errorf("cannot initialize Git index")
		}
	}
	if err = installGitDirectory(metadata, filepath.Join(directory, ".git")); err != nil {
		return fmt.Errorf("cannot install Git metadata; destination may have appeared: %w", err)
	}
	return nil
}
