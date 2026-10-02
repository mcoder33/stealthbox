package main

import (
	"bytes"
	"testing"
)

func TestEmbeddedPayloads(t *testing.T) {
	for _, tc := range []struct {
		arch    string
		machine byte
	}{{"x86_64", 62}, {"aarch64\n", 183}} {
		payload, err := linuxPayload(tc.arch)
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) < 20 || !bytes.Equal(payload[:4], []byte{0x7f, 'E', 'L', 'F'}) || payload[18] != tc.machine {
			t.Fatalf("wrong embedded Linux executable for %s", tc.arch)
		}
	}
	if _, err := linuxPayload("unknown"); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
}

func TestDistributionSelection(t *testing.T) {
	t.Setenv("STEALTHBOX_WSL_DISTRO", "Ubuntu Custom")
	p := wslPrefix()
	if len(p) != 3 || p[0] != "--distribution" || p[1] != "Ubuntu Custom" || p[2] != "--exec" {
		t.Fatalf("distribution name was not preserved: %q", p)
	}
}
