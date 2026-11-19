//go:build !windows

package stealthbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

func sshSetting(output, key string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == key {
			return fields[1]
		}
	}
	return ""
}

// Limit this mode to one numeric IPv4 endpoint, optionally reached via one jump.
func directFirstHop(alias, config string) (string, string, error) {
	name, ip := alias, sshSetting(config, "hostname")
	if proxy := sshSetting(config, "proxycommand"); proxy != "" && proxy != "none" {
		return "", "", fmt.Errorf("direct mode does not support ProxyCommand")
	}
	if jump := sshSetting(config, "proxyjump"); jump != "" && jump != "none" {
		if strings.Contains(jump, ",") {
			return "", "", fmt.Errorf("direct mode supports one ProxyJump")
		}
		if _, host, found := strings.Cut(jump, "@"); found {
			jump = host
		}
		ip = strings.TrimSuffix(strings.TrimPrefix(jump, "["), "]")
		name = ip
	}
	if net.ParseIP(ip).To4() == nil || strings.Contains(ip, ":") {
		return "", "", fmt.Errorf("direct mode needs a numeric IPv4 first hop (without a jump port override)")
	}
	return name, ip, nil
}

func configureDirectSSH(ctx context.Context, alias string, log io.Writer) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("--direct requires macOS")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(alias) || os.Getenv("STEALTHBOX_SSH_CONFIG") != "" {
		return fmt.Errorf("--direct requires a workspace SSH alias from the standard OpenSSH configuration")
	}
	settings := func(host string) (string, error) {
		data, e := exec.CommandContext(ctx, "/usr/bin/ssh", "-G", host).Output()
		return string(data), e
	}
	cfg, err := settings(alias)
	if err != nil {
		return err
	}
	name, ip, err := directFirstHop(alias, cfg)
	if err != nil {
		return err
	}
	if name != alias {
		cfg, err = settings(name)
		if err != nil {
			return err
		}
		n, actual, e := directFirstHop(name, cfg)
		if e != nil || n != name || actual != ip || sshSetting(cfg, "proxyjump") != "" {
			return fmt.Errorf("jump host must resolve directly to %s", ip)
		}
	}
	port := sshSetting(cfg, "port")
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("invalid SSH port")
	}
	out, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return err
	}
	device, gateway := sshSetting(string(out), "interface:"), sshSetting(string(out), "gateway:")
	if !regexp.MustCompile(`^en[0-9]+$`).MatchString(device) || net.ParseIP(gateway).To4() == nil {
		return fmt.Errorf("no physical IPv4 default gateway found; configure a VPN exclusion for %s/32", ip)
	}
	probe := func() error {
		return exec.CommandContext(ctx, "/usr/bin/nc", "-4", "-z", "-G", "5", "-b", device, ip, port).Run()
	}
	if err = probe(); err != nil {
		// Only provision a missing scoped route. A TCP failure on an existing route
		// may be a firewall or an unreachable server, neither is fixed by replacing it.
		if _, e := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "-ifscope", device, ip).Output(); e == nil {
			return fmt.Errorf("SSH gateway %s:%s is unreachable on %s; check VPN split tunneling", ip, port, device)
		}
		fmt.Fprintf(log, "Direct SSH needs a scoped host route to %s via %s (%s). To remove: sudo /sbin/route -n delete -host -ifscope %s %s %s\n", ip, gateway, device, device, ip, gateway)
		cmd := exec.CommandContext(ctx, "/usr/bin/sudo", "/sbin/route", "-n", "add", "-host", "-ifscope", device, ip, gateway)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, log, log
		if err = cmd.Run(); err != nil {
			return fmt.Errorf("could not add scoped route: %w", err)
		}
		if err = probe(); err != nil {
			// We just created this route, not an existing user's exception.
			cleanup := exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "/sbin/route", "-n", "delete", "-host", "-ifscope", device, ip, gateway)
			cleanup.Stderr = log
			if e := cleanup.Run(); e != nil {
				return fmt.Errorf("bound TCP probe failed; removing new route also failed: %w", e)
			}
			return fmt.Errorf("bound TCP probe failed on %s; route removed", device)
		}
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, ".ssh")
	if strings.ContainsAny(dir, " \t\r\n%'\"\\") {
		return fmt.Errorf("--direct requires an SSH directory without spaces or quoting characters")
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(alias+" "+name+" "+ip+" "+device+" "+gateway)))[:16]
	path := filepath.Join(dir, "stealthbox-direct-"+key+".conf")
	conf := directSSHConfig(alias, name, ip, device, key, dir)
	if err = writePrivate(path, []byte(conf)); err != nil {
		return err
	}
	fmt.Fprintf(log, "Direct SSH: %s:%s via %s.\n", ip, port, device)
	return os.Setenv("STEALTHBOX_SSH_CONFIG", path)
}

func directSSHConfig(alias, name, ip, device, key, dir string) string {
	// Separate masters prevent attachment to a connection established via VPN.
	conf := fmt.Sprintf("Host %s\n    ProxyCommand /usr/bin/nc -4 -G 8 -b %s %s %%p\n", name, device, ip)
	if name != alias {
		conf += "    ControlMaster no\n    ControlPath none\n"
	}
	conf += fmt.Sprintf("Match originalhost %s sessiontype none\n    ControlMaster no\n    ControlPath none\nHost %s\n    ControlPath %s/sbd-%s-%%C\nHost *\n    Include %s/config\nHost *\n    Include /etc/ssh/ssh_config\n", alias, alias, dir, key, dir)
	return conf
}
