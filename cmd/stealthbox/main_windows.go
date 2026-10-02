package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The release bundles both Linux architectures, so no download is needed.
//
//go:embed _payload/linux-amd64
var linuxAMD64 []byte

//go:embed _payload/linux-arm64
var linuxARM64 []byte

var version = "dev"

func main() {
	version = buildVersion()
	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		fmt.Println("stealthbox", version)
		return
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Println("Stealth Box for Windows (WSL 2 launcher)\nRequires a configured WSL Linux distribution with OpenSSH and rsync.\nRun without arguments to prepare and resume remote tmux; use 'settings' for the TUI.\nAll paths and SSH settings are inside WSL.\nSet STEALTHBOX_WSL_DISTRO to select a distribution.\nFor full CLI help: stealthbox.exe help --wsl")
		return
	}
	if len(args) == 2 && args[0] == "help" && args[1] == "--wsl" {
		args = []string{"help"}
	}
	if err := launchWSL(args); err != nil {
		fmt.Fprintln(os.Stderr, "stealthbox:", err)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}

func wslPrefix() []string {
	if distro := os.Getenv("STEALTHBOX_WSL_DISTRO"); distro != "" {
		return []string{"--distribution", distro, "--exec"}
	}
	return []string{"--exec"}
}

func linuxPayload(arch string) ([]byte, error) {
	switch strings.TrimSpace(arch) {
	case "x86_64", "amd64":
		return linuxAMD64, nil
	case "aarch64", "arm64":
		return linuxARM64, nil
	default:
		return nil, fmt.Errorf("unsupported WSL architecture %q", arch)
	}
}

// POSIX positional arguments preserve spaces, quotes and shell characters.
const installScript = `set -eu
umask 077
dir="$HOME/.cache/stealthbox/bin"
mkdir -p "$dir"
tmp=$(mktemp "$dir/.install.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM
cat > "$tmp"
chmod 700 "$tmp"
mv -f "$tmp" "$dir/stealthbox"
`
const launchScript = `exec "$HOME/.cache/stealthbox/bin/stealthbox" "$@"`

func launchWSL(args []string) error {
	wsl, err := exec.LookPath("wsl.exe")
	if err != nil {
		return fmt.Errorf("WSL 2 is required: run 'wsl --install -d Ubuntu', finish Linux user setup, then retry")
	}
	prefix := wslPrefix()
	archCmd := exec.Command(wsl, append(append([]string{}, prefix...), "uname", "-m")...)
	arch, err := archCmd.Output()
	if err != nil {
		return fmt.Errorf("cannot start WSL Linux: finish 'wsl --install -d Ubuntu' and Linux user setup; %w", err)
	}
	payload, err := linuxPayload(string(arch))
	if err != nil {
		return err
	}
	install := exec.Command(wsl, append(append([]string{}, prefix...), "sh", "-c", installScript)...)
	install.Stdin = bytes.NewReader(payload)
	install.Stdout, install.Stderr = os.Stdout, os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("install embedded Linux binary: %w", err)
	}
	argv := append(append([]string{}, prefix...), "sh", "-c", launchScript, "stealthbox")
	argv = append(argv, args...)
	cmd := exec.Command(wsl, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
