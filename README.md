# Stealth Box

Work in your usual terminal and tmux on a remote VM. Choose where project
commands run: the VM or your Mac. Written in Go, with no Go dependencies.

This is an initial CLI, not a macOS GUI or an automatic VM provisioner.
SSH, tmux, rsync and Docker remain normal system tools.

## Install

Go 1.24 or newer:

```sh
go install github.com/mcoder33/stealthbox/cmd/stealthbox@latest
# Or from this checkout:
go build -o bin/stealthbox ./cmd/stealthbox
```

## First connection

Configure an SSH alias such as `dev-vm` in `~/.ssh/config`. Use your local
ssh-agent/keychain. Stealth Box does not store SSH passwords or private keys.
Verify the host key and test `ssh dev-vm` before using the launcher.

```sh
stealthbox init
# Edit ~/.config/stealthbox/config.json with your own paths and aliases.
stealthbox doctor --project example --runner vm
stealthbox open --project example --session tmux --runner vm -- codex
```

`open` uses the **current terminal**, opening an SSH session in place. Omit
`-- codex` to start a shell. `--session shell` bypasses tmux. Separate default
tmux sessions are used for each project and runner. An existing tmux session
keeps its original process and environment when reattached.

Example configuration on the Mac:

```json
{
  "projects": {
    "example": {
      "source": {
        "host": "dev-vm",
        "path": "/home/developer/projects/example"
      },
      "runners": {
        "vm": {
          "host": "dev-vm",
          "path": "/home/developer/projects/example"
        },
        "mac": {
          "path": "/Users/developer/.local/share/stealthbox/runners/example"
        }
      }
    }
  }
}
```

All paths must be absolute. `host` is an SSH config alias; omitting it means
this machine. Paths must refer to the machine selected by `host`.

## Run commands

From the Mac:

```sh
stealthbox run --project example --runner vm -- docker compose run --rm test
stealthbox run --project example --runner mac -- docker compose run --rm test
# Review every planned command without connecting or writing:
stealthbox run --project example --runner mac --dry-run -- docker compose run --rm test
```

To run the same commands **inside Codex/tmux on the VM**, install Stealth Box
there too and create its configuration. On the VM, `source` and the `vm`
runner omit `host`; the `mac` runner uses an SSH alias reaching your Mac.
For example:

```json
{
  "projects": {
    "example": {
      "source": {"path": "/home/developer/projects/example"},
      "runners": {
        "vm": {"path": "/home/developer/projects/example"},
        "mac": {
          "host": "mac-runner",
          "path": "/Users/developer/.local/share/stealthbox/runners/example"
        }
      }
    }
  }
}
```

The VM needs an authenticated route to `mac-runner`, for example private
network access and macOS Remote Login. This version does not configure the
network or enable Remote Login. Do not expose Docker's API to the Internet.

`open` sets `STEALTHBOX_PROJECT` and `STEALTHBOX_RUNNER` for the new session,
so the VM can use:

```sh
stealthbox run -- docker compose run --rm test
```

Configure a project test script or agent instructions to call this command.
Arbitrary Docker commands are not intercepted. `STEALTHBOX_CONFIG` selects
a config path on the machine where the CLI runs; it is not forwarded by SSH.

## File synchronization and limits

- Code on the source is authoritative, including uncommitted files.
- A different runner gets an rsync copy before execution. Its path must be
  a **dedicated disposable directory**, initially empty. The CLI marks it
  with `.stealthbox-runner` and refuses an unmarked nonempty directory.
- Deleted source files are removed from the runner. Do not edit the runner
  copy or run concurrent synchronizations into the same directory.
- `.git`, `.env`, `.env.*`, `vendor`, `node_modules` and `.serena` are excluded.
  Provision runner secrets/dependencies separately. These exclusions are not
  comprehensive secret detection: review your project's files before sync.
- Excluded runner files survive synchronization. Other generated files in
  the runner directory may be removed on the next sync. Prefer Docker volumes
  for persistent data and collect artifacts before the next run.
- SSH and rsync must work without interactive password prompts. The bundled macOS
  openrsync is supported. Remote paths may contain only letters, digits, slash,
  dot, underscore and hyphen; local paths may include spaces.
- Synchronization supports local-to-remote, remote-to-local and local-to-local;
  two remote endpoints require running the CLI on one of those machines.
- Containers, volumes and databases stay on their runner. No state migration
  or automatic fallback. Mac ARM and VM AMD64 images may behave differently.
- tmux survives SSH disconnection, not a stopped/preempted VM. Restart the VM
  and resume Codex separately; Stealth Box does not restart VMs yet.
- Command output is streamed to the terminal and nonzero exit codes propagate.
  Artifact download, GUI terminal launching and automatic tunnels are future work.

## Development

```sh
go test -race ./...
go vet ./...
go build ./cmd/stealthbox
```

MIT licensed.
