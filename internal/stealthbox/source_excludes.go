package stealthbox

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Source excludes are literal paths relative to both workspace source roots.
func canonicalSourceExcludes(excludes []string) ([]string, error) {
	canonical := append([]string{}, excludes...)
	for _, exclude := range canonical {
		if exclude == "" || exclude == "." || path.IsAbs(exclude) || path.Clean(exclude) != exclude || strings.ContainsAny(exclude, "\x00\n\r:\\*?[]") {
			return nil, fmt.Errorf("workspace.source_excludes requires canonical relative literal paths: %q", exclude)
		}
		for _, part := range strings.Split(exclude, "/") {
			if part == ".." {
				return nil, fmt.Errorf("workspace.source_excludes must not contain ..: %q", exclude)
			}
		}
	}
	slices.Sort(canonical)
	return slices.Compact(canonical), nil
}

func sourcePathExcluded(relative string, excludes []string) bool {
	relative = filepath.ToSlash(relative)
	for _, exclude := range excludes {
		if relative == exclude || strings.HasPrefix(relative, exclude+"/") {
			return true
		}
	}
	return false
}

func checkoutSourceExcludes(relative string, excludes []string) ([]string, error) {
	canonical, err := canonicalSourceExcludes(excludes)
	if err != nil {
		return nil, err
	}
	relative = filepath.ToSlash(relative)
	if sourcePathExcluded(relative, canonical) {
		return nil, fmt.Errorf("selected source checkout is excluded by workspace.source_excludes")
	}
	selected := []string{}
	for _, exclude := range canonical {
		if strings.HasPrefix(exclude, relative+"/") {
			selected = append(selected, strings.TrimPrefix(exclude, relative+"/"))
		}
	}
	return selected, nil
}
