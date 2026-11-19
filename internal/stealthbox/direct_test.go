//go:build !windows

package stealthbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDirectSSHFirstHop(t *testing.T) {
	for _, tc := range []struct{ config, name, ip string }{
		{"hostname 192.0.2.10\n", "work", "192.0.2.10"},
		{"hostname 192.0.2.10\nproxyjump user@[192.0.2.11]\n", "192.0.2.11", "192.0.2.11"},
		{"hostname 192.0.2.10\nproxycommand proxy %h\n", "", ""},
		{"hostname 192.0.2.10\nproxyjump gateway\n", "", ""},
		{"hostname 192.0.2.10\nproxyjump a,b\n", "", ""},
	} {
		name, ip, err := directFirstHop("work", tc.config)
		if name != tc.name || ip != tc.ip || (err != nil) != (tc.name == "") {
			t.Fatalf("%q => %q %q %v", tc.config, name, ip, err)
		}
	}
}

func TestDirectBridgeIdentity(t *testing.T) {
	c := Config{}
	t.Setenv("STEALTHBOX_SSH_CONFIG", "")
	normal := configHash(c)
	t.Setenv("STEALTHBOX_SSH_CONFIG", "/tmp/test-direct.conf")
	if configHash(c) == normal {
		t.Fatal("would reuse daemon with old SSH transport")
	}
}

func TestDirectSSHConfigKeepsTargetAndBindsJump(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("validate the macOS OpenSSH configuration")
	}
	dir := t.TempDir()
	base := "Host work\n HostName 198.51.100.10\n ProxyJump user@192.0.2.4\n User target\n ServerAliveInterval 20\n ControlPath /tmp/vpn-master\n"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "direct")
	if err := os.WriteFile(config, []byte(directSSHConfig("work", "192.0.2.4", "192.0.2.4", "en0", "test", dir)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host string
		want []string
	}{
		{"work", []string{"hostname 198.51.100.10", "user target", "serveraliveinterval 20", "proxyjump user@", "controlpath " + dir + "/sbd-test-"}},
		{"192.0.2.4", []string{"proxycommand /usr/bin/nc -4 -G 8 -b en0 192.0.2.4 %p"}},
	} {
		out, err := exec.Command("ssh", "-F", config, "-G", tc.host).CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		for _, v := range tc.want {
			if !strings.Contains(string(out), v) {
				t.Fatalf("missing SSH setting %q", v)
			}
		}
	}
}

func TestScopedRouteLookup(t *testing.T) {
	for _, tc := range []struct {
		output         string
		found, wantErr bool
	}{
		{"route: writing to routing socket: not in table\n", false, false},
		{"   route to: 192.0.2.10\n  interface: en0\n", true, false},
		{"route: permission denied", false, true},
		{"interface: utun5", false, true},
	} {
		found, err := scopedRouteFound(tc.output, "en0")
		if found != tc.found || (err != nil) != tc.wantErr {
			t.Fatalf("lookup %q returned %v, %v", tc.output, found, err)
		}
	}
}

func TestPhysicalDefaultInVPNRoutingTable(t *testing.T) {
	const routes = "Internet:\nDestination Gateway Flags Netif Expire\ndefault link#24 UCSg utun5\ndefault 192.168.31.1 UGScIg en0\n"
	device, gw, err := physicalTableDefault(routes)
	if err != nil || device != "en0" || gw != "192.168.31.1" {
		t.Fatalf("%q %q %v", device, gw, err)
	}
	for _, table := range []string{"default link#24 UCSg utun5", routes + "default 10.0.0.1 UGScIg en2\n"} {
		if _, _, err := physicalTableDefault(table); err == nil {
			t.Fatal("expected unavailable or ambiguous default")
		}
	}
}
