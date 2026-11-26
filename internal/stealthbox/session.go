package stealthbox

import (
	"os"
	"path/filepath"
	"slices"
)

// VMSession identifies a conversation in the selected VM profile.
type VMSession struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	CWD   string `json:"cwd"`
	Agent string `json:"agent"`
}

func sessionInScope(c Config, cwd string) bool {
	if !filepath.IsAbs(cwd) {
		return false
	}
	path, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if sessionWithinRoot(c.Workspace.VMRoot, path) {
		return true
	}
	for _, p := range c.Projects {
		if p.Source.Host == "" && sessionWithinRoot(p.Source.Path, path) {
			return true
		}
	}
	return false
}

func sessionWithinRoot(root, path string) bool {
	if !filepath.IsAbs(root) {
		return false
	}
	physical, err := filepath.EvalSymlinks(root)
	return err == nil && physical != string(filepath.Separator) && within(physical, path)
}

func sessionAgentArgs(c Config, agent string) []string {
	if argv, ok := c.Agents[agent]; ok {
		return slices.Clone(argv)
	}
	return []string{agent}
}
