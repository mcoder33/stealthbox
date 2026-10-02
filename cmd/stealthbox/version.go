package main

import (
	"regexp"
	"runtime/debug"
)

var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// go install module@version has module build metadata but no release ldflags.
// Recognize stable, clean module versions so VM downloads use the same release.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	modified := false
	for _, setting := range info.Settings {
		modified = modified || setting.Key == "vcs.modified" && setting.Value == "true"
	}
	return resolvedVersion(version, info.Main.Version, modified)
}

func resolvedVersion(override, module string, modified bool) string {
	if override != "dev" {
		return override
	}
	if !modified && releaseVersion.MatchString(module) {
		return module
	}
	return "dev"
}
