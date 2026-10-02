package main

import "testing"

func TestInstalledModuleVersion(t *testing.T) {
	for _, test := range []struct {
		override, module string
		modified         bool
		want             string
	}{
		{"dev", "v0.4.1", false, "v0.4.1"},
		{"v0.4.1", "(devel)", false, "v0.4.1"},
		{"dev", "v0.4.1", true, "dev"},
		{"dev", "v0.4.1-0.20261002000000-abcdef123456", false, "dev"},
		{"dev", "(devel)", false, "dev"},
	} {
		if got := resolvedVersion(test.override, test.module, test.modified); got != test.want {
			t.Fatalf("%+v: got %q", test, got)
		}
	}
}
