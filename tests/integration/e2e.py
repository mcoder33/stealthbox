#!/usr/bin/env python3
"""Real SSH + Linux tmux + reverse Unix socket + local Docker. No cloud account/LLM needed."""
import json, os, pathlib, pty, select, shlex, signal, struct, subprocess, tempfile, time, fcntl, termios

ROOT = pathlib.Path(__file__).resolve().parents[2]
BIN = ROOT / 'bin/stealthbox'

def call(args, **kw):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kw).stdout

def wait_for(fn, timeout=45):
    deadline=time.time()+timeout
    last=None
    while time.time()<deadline:
        try:
            result=fn()
            if result:return result
        except (subprocess.CalledProcessError, OSError) as e:last=e
        time.sleep(.3)
    raise AssertionError(f'timed out: {last}')

def main():
    call(['docker','build','-t','stealthbox-integration:local',str(ROOT/'tests/integration')])
    arch=call(['docker','image','inspect','stealthbox-integration:local','--format','{{.Architecture}}']).strip()
    remote_binary=ROOT/'bin'/f'stealthbox-linux-{arch}'
    call(['go','build','-o',str(BIN),'./cmd/stealthbox'],cwd=ROOT)
    call(['go','build','-o',str(remote_binary),'./cmd/stealthbox'],cwd=ROOT,env=dict(os.environ,GOOS='linux',GOARCH=arch,CGO_ENABLED='0'))
    with tempfile.TemporaryDirectory(prefix='sb-') as temp:
        temp=pathlib.Path(os.path.realpath(temp));key=temp/'key';config=temp/'config.json';known=temp/'known_hosts';ssh_config=temp/'ssh_config';mac=temp/'runner'
        call(['ssh-keygen','-q','-t','ed25519','-N','','-f',str(key)])
        container=call(['docker','run','-d','--rm','-p','127.0.0.1::22','-v',f'{key}.pub:/test-key.pub:ro','stealthbox-integration:local']).strip()
        child=None;master=None;compose_name='sb-e2e-'+container[:12]
        try:
            port=call(['docker','port',container,'22/tcp']).strip().rsplit(':',1)[1]
            host_key=wait_for(lambda:call(['docker','exec',container,'cat','/etc/ssh/ssh_host_ed25519_key.pub']).strip())
            known.write_text(f'[127.0.0.1]:{port} {host_key}\n')
            ssh_config.write_text(f'Host sb-test\n  HostName 127.0.0.1\n  User developer\n  Port {port}\n  IdentityFile {key}\n  UserKnownHostsFile {known}\n  StrictHostKeyChecking yes\n  IdentitiesOnly yes\n  BatchMode yes\n')
            ssh=['ssh','-F',str(ssh_config),'sb-test']
            remote=lambda *argv:call(ssh+[shlex.join(argv)])
            wait_for(lambda:remote('true') == '')
            env=dict(os.environ,STEALTHBOX_SSH_CONFIG=str(ssh_config),TERM='xterm-256color');env.pop('TMUX',None)
            call([str(BIN),'init','--config',str(config),'--host','sb-test','--enable-mac','--allow-mac-exec'],env=env)
            call([str(BIN),'project','--config',str(config),'--project','demo','--path','/home/developer/project','--mac-path',str(mac),'--agent','codex','--runner','mac'],env=env)
            c=json.loads(config.read_text());c['bridge']['socket']=str(temp/'mac.sock')
            c['agents']={'codex':['sh','-c',"printf 'CODEX-READY\\n'; echo start >> /home/developer/codex-starts; exec sh"],'claude':['sh','-c',"printf 'CLAUDE-READY\\n'; exec sh"],'shell':['sh','-l']}
            theme=temp/'tmux.conf';theme.write_text('set -g status-position top\nset -g status-style bg=colour236,fg=colour183\n')
            c['workspace']['theme']=str(theme)
            config.write_text(json.dumps(c));os.chmod(config,0o600)
            remote('sh','-c',"printf 'uncommitted-from-vm\\n' > /home/developer/project/code.txt; printf 'secret\\n' > /home/developer/project/.env")
            compose='services:\n  smoke:\n    image: alpine:3.21\n    volumes: ["./:/src:ro"]\n    command: ["sh", "-c", "cat /src/code.txt; uname -s"]\n'
            call(ssh+['cat > /home/developer/project/compose.yaml'],input=compose)
            master,slave=pty.openpty();fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',32,120,0,0))
            child=subprocess.Popen([str(BIN),'connect','--config',str(config),'--project','demo','--agent','codex','--runner','mac','--binary',str(remote_binary)],stdin=slave,stdout=slave,stderr=slave,env=env,start_new_session=True);os.close(slave)
            def drain():
                chunks=[]
                while select.select([master],[],[],.02)[0]:
                    try:
                        chunk=os.read(master,65536)
                        if not chunk:break
                        chunks.append(chunk)
                    except OSError:break
                return b''.join(chunks)
            remote_bin='/home/developer/.stealthbox/bin/stealthbox';remote_cfg='/home/developer/.stealthbox/config.json'
            def bridge_ready():
                if child.poll() is not None:raise AssertionError('connect failed: '+drain().decode(errors='replace'))
                drain()
                return remote(remote_bin,'bridge-health','--config',remote_cfg) == ''
            wait_for(bridge_ready,90)
            print('PASS deployment + reverse Unix-socket bridge',flush=True)
            out=remote(remote_bin,'run','--config',remote_cfg,'--project','demo','--runner','mac','--','docker','compose','-p',compose_name,'run','--rm','smoke')
            assert 'uncommitted-from-vm' in out and 'Linux' in out,out
            assert not (mac/'.env').exists(),'VM .env copied'
            print('PASS VM snapshot → actual local Docker Compose → stdout',flush=True)
            direct=remote(remote_bin,'exec','--config',remote_cfg,'--project','demo','--on','mac','--','uname','-s').strip()
            assert direct in ('Darwin','Linux'),direct
            print(f'PASS direct Mac/local-host command ({direct})',flush=True)
            fail=subprocess.run(ssh+[shlex.join([remote_bin,'exec','--config',remote_cfg,'--project','demo','--on','mac','--','sh','-c','exit 7'])],capture_output=True,text=True)
            assert fail.returncode==7,fail
            print('PASS nonzero exit status forwarded end to end',flush=True)
            remote(remote_bin,'workspace','--config',remote_cfg,'--project','demo','--agent','claude','--runner','mac','--slot','second','--no-attach')
            windows=remote('tmux','-L','stealthbox','list-windows','-t','stealthbox','-F','#{window_name}')
            assert 'demo-codex-mac-main' in windows and 'demo-claude-mac-second' in windows,windows
            print('PASS separate Codex/Claude tmux windows',flush=True)
            assert remote('tmux','-L','stealthbox','show-options','-g','status-position').strip()=='status-position top'
            print('PASS remote tmux style deployment',flush=True)
            call(['docker','exec',container,'pkill','-KILL','-f','sshd: developer@pts'])
            time.sleep(2)
            wait_for(bridge_ready,60)
            wait_for(lambda:bool(remote('tmux','-L','stealthbox','list-clients','-t','stealthbox','-F','#{client_session}').strip()))
            starts=remote('cat','/home/developer/codex-starts').strip().splitlines();assert starts==['start'],starts
            print('PASS reconnect preserves agent process; does not rerun it',flush=True)
            call(['docker','exec',container,'pkill','-KILL','-u','developer','-x','sshd'])
            time.sleep(1)
            wait_for(bridge_ready,60)
            print('PASS background reverse tunnel reconnects independently',flush=True)
            wait_for(lambda:bool(remote('tmux','-L','stealthbox','list-clients','-t','stealthbox','-F','#{client_session}').strip()))
            remote('tmux','-L','stealthbox','detach-client','-s','stealthbox')
            detach_output=b'';deadline=time.time()+20
            while child.poll() is None and time.time()<deadline:
                detach_output+=drain();time.sleep(.1)
            assert child.poll() is not None,detach_output.decode(errors='replace')
            assert child.returncode==0,detach_output.decode(errors='replace')
            assert remote(remote_bin,'exec','--config',remote_cfg,'--project','demo','--on','mac','--','printf','still-online')=='still-online'
            print('PASS background Mac bridge survives tmux detach',flush=True)
            remote(remote_bin,'exec','--config',remote_cfg,'--project','demo','--on','mac','--','sh','-c','printf artifact-ok > report.txt')
            remote(remote_bin,'fetch','--config',remote_cfg,'--project','demo','--file','report.txt','--output','/home/developer/report.txt')
            assert remote('cat','/home/developer/report.txt')=='artifact-ok'
            print('PASS artifact downloaded from Mac back to VM',flush=True)
            print('ALL E2E CHECKS PASSED',flush=True)
        finally:
            if config.exists():
                subprocess.run([str(BIN),'bridge-stop','--config',str(config)],capture_output=True,env=locals().get('env',os.environ))
            if child is not None and child.poll() is None:
                os.killpg(child.pid,signal.SIGTERM)
                try:child.wait(timeout=10)
                except subprocess.TimeoutExpired:os.killpg(child.pid,signal.SIGKILL);child.wait()
            if master is not None:os.close(master)
            subprocess.run(['docker','rm','-f',container],capture_output=True)
            # Compose one-off containers are --rm; its temporary network has a deterministic name.
            subprocess.run(['docker','network','rm',compose_name+'_default'],capture_output=True)

if __name__=='__main__':main()
