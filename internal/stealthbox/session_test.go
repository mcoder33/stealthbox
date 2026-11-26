package stealthbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionScopeUsesPhysicalVMRoots(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	subdir := filepath.Join(workspace, "component")
	sibling := filepath.Join(dir, "workspace-other")
	project := filepath.Join(dir, "legacy", "project")
	foreign := filepath.Join(dir, "foreign")
	for _, path := range []string{subdir, sibling, project, foreign} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(subdir, "regular.txt")
	if err := os.WriteFile(file, []byte("regular file"), 0600); err != nil {
		t.Fatal(err)
	}
	workAlias := filepath.Join(dir, "alias")
	escape := filepath.Join(workspace, "escape")
	for alias, target := range map[string]string{workAlias: workspace, escape: sibling} {
		if err := os.Symlink(target, alias); err != nil {
			t.Fatal(err)
		}
	}
	config := Config{
		Workspace: WorkspaceConfig{VMRoot: workAlias},
		Projects: map[string]Project{
			"legacy":  {Source: Endpoint{Path: project}},
			"foreign": {Source: Endpoint{Path: foreign, Host: "another-vm"}},
		},
	}
	for _, test := range []struct {
		name, cwd string
		want      bool
	}{
		{"workspace root", workspace, true},
		{"workspace child", subdir, true},
		{"workspace alias", filepath.Join(workAlias, "component"), true},
		{"legacy source", project, true},
		{"remote source", foreign, false},
		{"sibling prefix", sibling, false},
		{"symlink escape", escape, false},
		{"missing", filepath.Join(workspace, "missing"), false},
		{"regular file", file, false},
		{"relative", "component", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionInScope(config, test.cwd); got != test.want {
				t.Errorf("sessionInScope(%q) = %v, want %v", test.cwd, got, test.want)
			}
		})
	}
}

func TestSessionScopeRequiresAnAbsoluteConfiguredRoot(t *testing.T) {
	cwd := t.TempDir()
	for _, root := range []string{"", "~/Projects"} {
		config := Config{Workspace: WorkspaceConfig{VMRoot: root}}
		if sessionInScope(config, cwd) {
			t.Errorf("session unexpectedly in scope with root %q", root)
		}
	}
}
