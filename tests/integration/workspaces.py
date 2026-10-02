#!/usr/bin/env python3
"""Workspace integration: real SSH/rsync, isolated tmux, MCP and local Docker.

No application code, cloud credentials, or actual LLM requests are used. All
keys, processes, checkouts and Compose networks are isolated and cleaned up.
"""
import fcntl
import hashlib
import json
import os
import pathlib
import pty
import select
import shlex
import signal
import struct
import subprocess
import tempfile
import termios
import time

ROOT = pathlib.Path(__file__).resolve().parents[2]
IMAGE = 'stealthbox-integration:local'
class CommandFailed(RuntimeError):
    pass


VM_ROOT = '/home/developer/Projects'
REMOTE_BIN = '/home/developer/.stealthbox/bin/stealthbox'
REMOTE_CONFIG = '/home/developer/.stealthbox/config.json'


def call(argv, **kwargs):
    kwargs.setdefault('timeout', 180)
    try:
        result = subprocess.run(argv, check=True, text=True, capture_output=True, **kwargs)
    except subprocess.CalledProcessError as error:
        raise CommandFailed(f'command failed: {shlex.join(argv)}\n{error.stderr}') from error
    return result.stdout


def wait_for(predicate, timeout=60):
    deadline = time.monotonic() + timeout
    error = None
    while time.monotonic() < deadline:
        try:
            result = predicate()
            if result:
                return result
        except (OSError, subprocess.SubprocessError, CommandFailed) as exc:
            error = exc
        time.sleep(.15)
    raise AssertionError(f'timed out: {error}')


def main():
    print('Building isolated SSH + rsync workspace fixture', flush=True)
    call(['docker', 'build', '-t', IMAGE, str(ROOT / 'tests/integration')], timeout=300)
    arch = call(['docker', 'image', 'inspect', IMAGE, '--format', '{{.Architecture}}']).strip()
    with tempfile.TemporaryDirectory(prefix='sb-ws-') as temporary:
        temporary = pathlib.Path(os.path.realpath(temporary))
        binary = temporary / 'stealthbox'
        linux_binary = temporary / 'stealthbox-linux'
        key = temporary / 'key'
        known = temporary / 'known_hosts'
        ssh_config = temporary / 'ssh config with spaces'
        config = temporary / 'config.json'
        runner_root = temporary / 'runners'
        local_root = temporary / 'Projects'
        call(['go', 'build', '-o', str(binary), './cmd/stealthbox'], cwd=ROOT)
        call(['go', 'build', '-o', str(linux_binary), './cmd/stealthbox'], cwd=ROOT,
             env=dict(os.environ, GOOS='linux', GOARCH=arch, CGO_ENABLED='0'))
        call(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(key)])
        container = call(['docker', 'run', '-d', '--rm', '-p', '127.0.0.1::22', '-v',
                          f'{key}.pub:/test-key.pub:ro', IMAGE]).strip()
        process = None
        master = None
        env = dict(os.environ, STEALTHBOX_SSH_CONFIG=str(ssh_config), TERM='xterm-256color')
        env.pop('TMUX', None)
        env.pop('STEALTHBOX_PROJECT', None)
        env.pop('STEALTHBOX_SCOPE', None)
        networks = []
        try:
            port = call(['docker', 'port', container, '22/tcp']).strip().rsplit(':', 1)[1]
            host_key = wait_for(lambda: call(['docker', 'exec', container, 'cat',
                                            '/etc/ssh/ssh_host_ed25519_key.pub']).strip())
            known.write_text(f'[127.0.0.1]:{port} {host_key}\n')
            ssh_config.write_text(f'Host sb-workspace\n  HostName 127.0.0.1\n  User developer\n'
                                  f'  Port {port}\n  IdentityFile {key}\n  UserKnownHostsFile {known}\n'
                                  '  StrictHostKeyChecking yes\n  IdentitiesOnly yes\n  BatchMode yes\n')
            ssh = ['ssh', '-F', str(ssh_config), 'sb-workspace']
            remote = lambda *argv: call(ssh + [shlex.join(argv)])
            wait_for(lambda: remote('true') == '')
            remote('mkdir', '-p', f'{VM_ROOT}/a/api/tests', f'{VM_ROOT}/b/api',
                   '/home/developer/outside', f'{VM_ROOT}/plain/src/tests',
                   f'{VM_ROOT}/.codex/credentials/.git', f'{VM_ROOT}/.env-private/repo/.git')
            for relative, code in [('a/api', 'aaaaa'), ('b/api', 'bbbbb')]:
                root = f'{VM_ROOT}/{relative}'
                remote('git', '-C', root, 'init', '--quiet')
                remote('sh', '-c', f"printf '{code}\\n' > {shlex.quote(root + '/code.txt')}")
            remote('git', '-C', f'{VM_ROOT}/a/api', 'add', 'code.txt')
            remote('git', '-C', f'{VM_ROOT}/a/api', '-c', 'user.name=Fixture', '-c',
                   'user.email=fixture@example.invalid', 'commit', '--quiet', '-m', 'fixture')
            remote('git', '-C', f'{VM_ROOT}/a/api', 'worktree', 'add', '--quiet', '--detach',
                   f'{VM_ROOT}/worktree-detached', 'HEAD')
            remote('git', '-C', f'{VM_ROOT}/a/api', 'worktree', 'add', '--quiet', '-b',
                   'fixture-branch', f'{VM_ROOT}/worktree-branch', 'HEAD')
            remote('sh', '-c', f"printf stale > {VM_ROOT}/a/api/remove-me; "
                   f"printf secret > {VM_ROOT}/a/api/.envrc; "
                   f"mkdir -p {VM_ROOT}/a/api/.codex {VM_ROOT}/a/api/vendor/library/.git; "
                   f"printf secret > {VM_ROOT}/a/api/.codex/auth.json; "
                   f"ln -s /home/developer/outside {VM_ROOT}/escape; "
                   f"ln -s /home/developer/outside {VM_ROOT}/a/api/escape")
            compose = ('services:\n  smoke:\n    image: alpine:3.21\n'
                       '    volumes: ["./:/src:ro"]\n'
                       '    command: ["sh", "-c", "cat /src/code.txt; printf docker-ok"]\n')
            call(ssh + [f'cat > {VM_ROOT}/a/api/compose.yaml'], input=compose)
            mock = ('#!/bin/sh\n'
                    'printf "%s|%s|%s\\n" "$STEALTHBOX_SCOPE" "$STEALTHBOX_PROJECT" "$PWD" '
                    '>> /home/developer/agent-launches\n'
                    'printf "%s\\n" "$@" > /home/developer/agent-arguments\n')
            call(ssh + ['cat > /home/developer/mock-codex; chmod 700 /home/developer/mock-codex'], input=mock)
            call([str(binary), 'init', '--config', str(config), '--host', 'sb-workspace',
                  '--enable-mac', '--allow-mac-exec', '--vm-root', '~/Projects', '--local-root',
                  str(local_root), '--runner-root', str(runner_root), '--shell-integration'], env=env)
            settings = json.loads(config.read_text())
            settings['bridge']['socket'] = str(temporary / 'mac.sock')
            settings['workspace']['id'] = 'workspace-e2e-' + container[:12]
            settings['agents']['codex'] = ['/home/developer/mock-codex']
            config.write_text(json.dumps(settings))
            os.chmod(config, 0o600)
            identity = settings['workspace']['id']
            ids = {}
            for relative in ['a/api', 'b/api', 'worktree-detached', 'worktree-branch']:
                digest = hashlib.sha256((identity + '\0' + relative).encode()).hexdigest()[:24]
                ids[relative] = 'project-' + digest
                networks.append(ids[relative] + '_default')
            first_runner = runner_root / ids['a/api'] / 'tree'
            first_runner.mkdir(parents=True)
            (first_runner / '.stealthbox-runner').touch()
            vendor = first_runner / 'vendor'
            vendor.mkdir()
            for number in range(1000):
                (vendor / f'dependency-{number}').write_bytes(b'cached-' * 150)
            sentinel = vendor / 'dependency-0'
            original_cache = (sentinel.stat().st_ino, sentinel.stat().st_mtime_ns)
            local_source = local_root / 'a/api'
            local_source.mkdir(parents=True)
            (local_source / 'code.txt').write_text('local-source-must-stay')
            master, slave = pty.openpty()
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 32, 120, 0, 0))
            process = subprocess.Popen([str(binary), 'connect', '--config', str(config),
                                        '--runner', 'local', '--binary', str(linux_binary)],
                                       stdin=slave, stdout=slave, stderr=slave, env=env,
                                       start_new_session=True)
            os.close(slave)

            def drain():
                chunks = []
                while select.select([master], [], [], .02)[0]:
                    try:
                        chunk = os.read(master, 65536)
                        if not chunk:
                            break
                        chunks.append(chunk)
                    except OSError:
                        break
                return b''.join(chunks)

            def ready():
                if process.poll() is not None:
                    raise AssertionError('root connect failed: ' + drain().decode(errors='replace'))
                drain()
                return remote(REMOTE_BIN, 'bridge-health', '--config', REMOTE_CONFIG) == ''

            wait_for(ready, 90)
            deployed = json.loads(remote('cat', REMOTE_CONFIG))
            assert deployed['workspace']['sync_transport'] == 'rsync', deployed
            assert deployed['workspace'].get('host', '') == '', deployed
            assert deployed['workspace']['vm_root'] == VM_ROOT, deployed
            print('PASS root deployment, remote HOME expansion, reverse bridge + true rsync mode', flush=True)
            output = remote(REMOTE_BIN, 'run', '--config', REMOTE_CONFIG, '--path', 'a/api/tests',
                            '--runner', 'local', '--', 'docker', 'compose', '-f', '../compose.yaml',
                            'run', '--rm', 'smoke')
            assert 'aaaaa' in output and 'docker-ok' in output, output
            assert (first_runner / 'code.txt').read_text() == 'aaaaa\n'
            assert not (first_runner / '.envrc').exists()
            assert not (first_runner / '.codex').exists()
            assert (sentinel.stat().st_ino, sentinel.stat().st_mtime_ns) == original_cache
            assert (local_source / 'code.txt').read_text() == 'local-source-must-stay'
            print('PASS checkout subdirectory → network rsync → actual local Docker Compose; 1000 cached dependencies preserved', flush=True)
            remote('sh', '-c', f"cp -p {VM_ROOT}/a/api/code.txt /home/developer/time-reference; "
                   f"printf 'ccccc\\n' > {VM_ROOT}/a/api/code.txt; "
                   f"touch -r /home/developer/time-reference {VM_ROOT}/a/api/code.txt; "
                   f"rm {VM_ROOT}/a/api/remove-me")
            output = remote(REMOTE_BIN, 'run', '--config', REMOTE_CONFIG, '--path', 'a/api',
                            '--runner', 'local', '--', 'cat', 'code.txt')
            assert output == 'ccccc\n', output
            assert not (first_runner / 'remove-me').exists()
            assert (sentinel.stat().st_ino, sentinel.stat().st_mtime_ns) == original_cache
            print('PASS same-size/preserved-mtime edit transferred; stale code deleted; dependency cache retained', flush=True)
            messages = []
            for number, relative in enumerate(['a/api', 'b/api', 'a/api'], 1):
                messages.append({'jsonrpc': '2.0', 'id': number, 'method': 'tools/call',
                                 'params': {'name': 'runner_run', 'arguments': {
                                     'path': relative, 'runner': 'local', 'argv': ['cat', 'code.txt']}}})
            messages.append({'jsonrpc': '2.0', 'id': 4, 'method': 'tools/call',
                             'params': {'name': 'projects_list', 'arguments': {}}})
            output = call(ssh + [f'STEALTHBOX_SCOPE=workspace {REMOTE_BIN} mcp --config {REMOTE_CONFIG}'],
                          input=''.join(json.dumps(message) + '\n' for message in messages))
            responses = [json.loads(line) for line in output.splitlines()]
            assert all(not response['result']['isError'] for response in responses), responses
            expected = ['ccccc', 'bbbbb', 'ccccc']
            for response, text in zip(responses[:3], expected):
                assert text in response['result']['content'][0]['text'], response
            discovered = json.loads(responses[3]['result']['content'][0]['text'].split('\n[exit status]')[0])
            assert discovered['paths'] == ['a/api', 'b/api', 'worktree-branch', 'worktree-detached'], discovered
            second_runner = runner_root / ids['b/api'] / 'tree'
            assert second_runner != first_runner and (second_runner / 'code.txt').read_text() == 'bbbbb\n'
            print('PASS one workspace MCP agent A → B → A, same-basename checkout isolation + dependency-pruned discovery', flush=True)
            ambiguous = {'jsonrpc': '2.0', 'id': 5, 'method': 'tools/call', 'params': {
                'name': 'vm_exec', 'arguments': {'project': 'other', 'path': 'a/api',
                                               'argv': ['touch', '/home/developer/ambiguous-marker']}}}
            output = call(ssh + [f'STEALTHBOX_SCOPE=workspace {REMOTE_BIN} mcp --config {REMOTE_CONFIG}'],
                          input=json.dumps(ambiguous) + '\n')
            assert json.loads(output)['result']['isError'], output
            remote('test', '!', '-e', '/home/developer/ambiguous-marker')
            for relative in ['worktree-detached', 'worktree-branch']:
                output = remote(REMOTE_BIN, 'run', '--config', REMOTE_CONFIG, '--path', relative,
                                '--runner', 'local', '--', 'cat', 'code.txt')
                assert output == 'aaaaa\n', (relative, output)
                checkout = runner_root / ids[relative] / 'tree'
                assert (checkout / 'code.txt').read_text() == 'aaaaa\n'
                assert checkout != first_runner and checkout != second_runner
                assert not (checkout / '.git').exists()
            detached = subprocess.run(ssh + [shlex.join(['git', '-C', f'{VM_ROOT}/worktree-detached',
                                                       'symbolic-ref', '--quiet', 'HEAD'])],
                                      capture_output=True, text=True, timeout=30)
            assert detached.returncode == 1, detached
            print('PASS real Git worktrees, detached HEAD + separate complete source runners', flush=True)
            for path, cwd in [('escape', ''), ('a/api', 'escape'), ('../outside', ''),
                              ('a/api/vendor/library', ''), ('.codex/credentials', ''), ('.env-private/repo', '')]:
                result = subprocess.run(ssh + [shlex.join([REMOTE_BIN, 'exec', '--config', REMOTE_CONFIG,
                                                         '--path', path, '--cwd', cwd, '--on', 'vm', '--', 'pwd'])],
                                        capture_output=True, text=True, timeout=30)
                assert result.returncode != 0, result
            remote(REMOTE_BIN, 'exec', '--config', REMOTE_CONFIG, '--path', 'a/api', '--on', 'local',
                   '--', 'sh', '-c', 'printf artifact-ok > report.txt')
            remote(REMOTE_BIN, 'fetch', '--config', REMOTE_CONFIG, '--path', 'a/api', '--file',
                   'report.txt', '--output', '/home/developer/report.txt')
            assert remote('cat', '/home/developer/report.txt') == 'artifact-ok'
            print('PASS checkout/cwd symlink and traversal rejection; dynamic artifact returned to VM', flush=True)
            remote('sh', '-c', f'printf nongit-root > {VM_ROOT}/plain/root-file')
            assert remote(REMOTE_BIN, 'run', '--config', REMOTE_CONFIG, '--path', 'plain/src/tests',
                          '--runner', 'local', '--', 'cat', '../../root-file') == 'nongit-root'
            print('PASS non-Git subdirectory uses the direct-child checkout and full source sync', flush=True)
            root_window = remote('tmux', '-L', 'stealthbox', 'display-message', '-p', '-t', 'stealthbox', '#{window_id}').strip()
            remote('tmux', '-L', 'stealthbox', 'send-keys', '-t', root_window,
                   f'cd {VM_ROOT}/a/api; codex --fixture-first', 'Enter')
            wait_for(lambda: 'workspace||' + VM_ROOT + '/a/api' in remote('cat', '/home/developer/agent-launches'))
            remote('tmux', '-L', 'stealthbox', 'new-window', '-t', 'stealthbox', '-n', 'second',
                   '-c', f'{VM_ROOT}/b/api')
            remote('tmux', '-L', 'stealthbox', 'send-keys', '-t', 'stealthbox:second',
                   'codex --fixture-second', 'Enter')
            launches = wait_for(lambda: remote('cat', '/home/developer/agent-launches')
                               if 'workspace||' + VM_ROOT + '/b/api' in remote('cat', '/home/developer/agent-launches') else '')
            assert 'workspace||' + VM_ROOT + '/a/api' in launches and 'workspace||' + VM_ROOT + '/b/api' in launches
            args = remote('cat', '/home/developer/agent-arguments')
            assert 'STEALTHBOX_SCOPE="workspace"' in args, args
            print('PASS root tmux shell, ordinary new window, codex wrapper + workspace MCP arguments (mock launcher)', flush=True)
            plan = temporary / 'export-plan.json'
            preview = call([str(binary), 'source', 'export', '--config', str(config), '--path', 'b/api',
                            '--plan', str(plan)], env=env)
            assert plan.exists(), preview
            assert not (local_root / 'b/api').exists(), 'preview mutated local checkout'
            call([str(binary), 'source', 'apply', '--config', str(config), '--plan', str(plan)], env=env)
            assert (local_root / 'b/api/code.txt').read_text() == 'bbbbb\n'
            conflict = temporary / 'conflict-plan.json'
            call([str(binary), 'source', 'export', '--config', str(config), '--path', 'b/api',
                  '--plan', str(conflict)], env=env)
            (local_root / 'b/api/code.txt').write_text('local-conflict')
            rejected = subprocess.run([str(binary), 'source', 'apply', '--config', str(config),
                                       '--plan', str(conflict)], env=env, capture_output=True, text=True, timeout=60)
            assert rejected.returncode != 0, rejected
            assert (local_root / 'b/api/code.txt').read_text() == 'local-conflict'
            (local_root / 'b/api/code.txt').write_text('bbbbb\n')
            changed_source = temporary / 'source-change-plan.json'
            call([str(binary), 'source', 'export', '--config', str(config), '--path', 'b/api',
                  '--plan', str(changed_source)], env=env)
            remote('sh', '-c', f"printf 'zzzzz\\n' > {VM_ROOT}/b/api/code.txt")
            rejected = subprocess.run([str(binary), 'source', 'apply', '--config', str(config),
                                       '--plan', str(changed_source)], env=env, capture_output=True, text=True, timeout=60)
            assert rejected.returncode != 0, rejected
            assert (local_root / 'b/api/code.txt').read_text() == 'bbbbb\n'
            remote('rm', f'{VM_ROOT}/a/api/escape')
            import_plan = temporary / 'import-plan.json'
            call([str(binary), 'source', 'import', '--config', str(config), '--path', 'a/api',
                  '--plan', str(import_plan)], env=env)
            assert remote('cat', f'{VM_ROOT}/a/api/code.txt') == 'ccccc\n', 'import preview mutated VM'
            call([str(binary), 'source', 'apply', '--config', str(config), '--plan', str(import_plan)], env=env)
            assert remote('cat', f'{VM_ROOT}/a/api/code.txt') == 'local-source-must-stay'
            print('PASS source import/export preview + apply; changed source/destination block stale plans', flush=True)
            print('ALL WORKSPACE E2E CHECKS PASSED', flush=True)
        finally:
            if config.exists():
                try:
                    subprocess.run([str(binary), 'bridge-stop', '--config', str(config)], env=env,
                                   capture_output=True, timeout=30)
                except subprocess.SubprocessError as error:
                    print(f'Bridge cleanup failed: {error}', flush=True)
            if process is not None and process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=10)
            if master is not None:
                os.close(master)
            subprocess.run(['docker', 'rm', '-f', container], capture_output=True, timeout=30)
            for network in networks:
                subprocess.run(['docker', 'network', 'rm', network], capture_output=True, timeout=30)


if __name__ == '__main__':
    main()
