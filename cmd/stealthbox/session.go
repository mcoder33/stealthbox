//go:build !windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mcoder33/stealthbox/internal/stealthbox"
)

const sessionUsage = `  stealthbox session list [--timeout 30s] [--config PATH]
  stealthbox session ask AGENT:ID [--timeout 5m] [--config PATH] < MESSAGE
`

func runSessionCommand(ctx context.Context, args []string, configDefault string, onVM bool) error {
	cmd, config, err := parseSessionCommand(args, configDefault)
	if errors.Is(err, flag.ErrHelp) {
		_, err := fmt.Fprint(os.Stdout, sessionUsage)
		return err
	}
	if err != nil {
		return err
	}
	c, err := stealthbox.Load(config)
	if os.IsNotExist(err) {
		return fmt.Errorf("configuration not found: %s; run stealthbox setup first", config)
	}
	if err != nil {
		return err
	}
	if onVM {
		return cmd.RunOnVM(ctx, c, os.Stdin, os.Stdout, os.Stderr)
	}
	return cmd.RunOverSSH(ctx, c, os.Stdin, os.Stdout, os.Stderr)
}

func parseSessionCommand(args []string, configDefault string) (stealthbox.SessionCommand, string, error) {
	var cmd stealthbox.SessionCommand
	if len(args) == 0 {
		return cmd, "", flag.ErrHelp
	}
	action, flags := args[0], args[1:]
	switch action {
	case "--help", "-h":
		return cmd, "", flag.ErrHelp
	case "list":
		cmd.Timeout = 30 * time.Second
	case "ask":
		if len(flags) == 0 || flags[0] == "" {
			return cmd, "", fmt.Errorf("session ask requires AGENT:ID and a message on stdin")
		}
		switch flags[0] {
		case "--help", "-h":
			return cmd, "", flag.ErrHelp
		}
		cmd.Target, flags = flags[0], flags[1:]
		cmd.Timeout = 5 * time.Minute
	default:
		return cmd, "", fmt.Errorf("unknown session action %q; use session list or session ask", action)
	}

	f := flag.NewFlagSet("session "+action, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	config := f.String("config", configDefault, "configuration path")
	f.DurationVar(&cmd.Timeout, "timeout", cmd.Timeout, "maximum time to wait for the VM command")
	if err := f.Parse(flags); err != nil {
		return cmd, "", err
	}
	if f.NArg() != 0 || cmd.Timeout <= 0 {
		return cmd, "", fmt.Errorf("session %s accepts only flags after its arguments and requires a positive --timeout", action)
	}
	return cmd, *config, nil
}
