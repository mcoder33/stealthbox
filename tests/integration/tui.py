#!/usr/bin/env python3
"""Drive the actual TUI through a pseudo-terminal, including persisted settings."""
import fcntl, html, json, os, pathlib, pty, re, select, signal, struct, subprocess, tempfile, termios, time
ROOT=pathlib.Path(__file__).resolve().parents[2]
ANSI=re.compile(r'\x1b\[[0-?]*[ -/]*[@-~]')
def run():
 subprocess.run(['go','build','-o','bin/stealthbox','./cmd/stealthbox'],cwd=ROOT,check=True)
 with tempfile.TemporaryDirectory(prefix='sb-tui-') as d:
  config=pathlib.Path(d)/'config.json';master,slave=pty.openpty();original=termios.tcgetattr(slave)
  fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',28,100,0,0))
  p=subprocess.Popen([str(ROOT/'bin/stealthbox'),'tui','--config',str(config)],stdin=slave,stdout=slave,stderr=slave,start_new_session=True)
  os.close(slave)
  def until(text):
   data=b'';deadline=time.time()+8
   while time.time()<deadline:
    if select.select([master],[],[],.1)[0]:
     data+=os.read(master,65536)
     if text in data.decode(errors='replace'):return data.decode(errors='replace')
    if p.poll() is not None:raise AssertionError(f'TUI exited {p.returncode}: {data!r}')
   raise AssertionError(f'timed out waiting for {text!r}: {data!r}')
  def send(s):os.write(master,s.encode())
  try:
   initial=until('q/Esc back')
   for _ in range(100):
    os.kill(p.pid,signal.SIGURG);time.sleep(.002)
   send('j\r');until('Add named profile');send('\r');until('Project name')
   for _ in range(100):
    os.kill(p.pid,signal.SIGURG);time.sleep(.002)
   send('demo\r')
   until('Absolute project path');send('/home/developer/project\r')
   until('Disposable Mac runner');send(os.path.realpath(d)+'/runner\r')
   until('Default agent');send('\r');until('Default runner');send('\r');until('VM: dev-vm')
   c=json.loads(config.read_text());assert c['projects']['demo']['agent']=='codex';assert c['projects']['demo']['runner']=='vm'
   send('jjj\r');until('Mac-user permissions');send('\r');until('VM: dev-vm · Mac bridge: on');assert json.loads(config.read_text())['bridge']['enabled']
   send('jjj\r');until('Mac-user permissions');send('j\r');until('VM: dev-vm · Mac bridge: on');assert json.loads(config.read_text())['bridge']['allow_exec']
   send('\x1b');p.wait(timeout=3);assert p.returncode==0
   restored=termios.tcgetattr(master);assert restored[3]&(termios.ICANON|termios.ECHO)==original[3]&(termios.ICANON|termios.ECHO)
   clean=ANSI.sub('',initial).replace('\r','').strip('\n')
   lines=clean.splitlines();elements=[]
   for i,line in enumerate(lines):
    color='#c5acf4' if 'STEALTH BOX' in line else '#a6dba6' if '›' in line else '#b9bdd4'
    elements.append(f'<text x="28" y="{75+i*22}" fill="{color}">{html.escape(line)}</text>')
   height=max(460,110+len(lines)*22);svg=f'<svg xmlns="http://www.w3.org/2000/svg" width="1000" height="{height}" viewBox="0 0 1000 {height}"><rect width="1000" height="{height}" rx="16" fill="#191a24"/><path d="M0 40H1000" stroke="#343849"/><circle cx="22" cy="20" r="6" fill="#fb6058"/><circle cx="42" cy="20" r="6" fill="#f5bd4f"/><circle cx="62" cy="20" r="6" fill="#34c949"/><text x="88" y="25" fill="#9298b0" font-family="monospace" font-size="13">Stealth Box · actual TUI output</text><g font-family="monospace" font-size="16">'+''.join(elements)+'</g></svg>'
   assets=ROOT/'docs/assets';assets.mkdir(exist_ok=True);(assets/'tui.svg').write_text(svg)
   print('PASS TUI signal interruptions, named profile creation, bridge settings, Esc, terminal restoration')
  finally:
   if p.poll() is None:
    p.terminate()
    try:p.wait(timeout=5)
    except subprocess.TimeoutExpired:p.kill();p.wait()
   os.close(master)
if __name__=='__main__':run()
