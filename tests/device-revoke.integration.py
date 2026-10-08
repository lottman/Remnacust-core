"""Check live VLESS revocation against a built core, using only loopback fixtures."""
import json, pathlib, socket, struct, subprocess, sys, tempfile, threading, time, uuid

binary=str(pathlib.Path(sys.argv[1]).resolve())
ids={k:str(uuid.uuid4()) for k in ('A','B','shared')}
emails={'A':'42~aaaaaaaaaaaaaaaaaaaaaaaa','B':'42~bbbbbbbbbbbbbbbbbbbbbbbb','shared':'42'}
stop=threading.Event()
listener=socket.socket(); listener.bind(('127.0.0.1',0)); listener.listen(); listener.settimeout(.2)
echo_port=listener.getsockname()[1]
def free_port():
    with socket.socket() as connection:
        connection.bind(('127.0.0.1',0))
        return connection.getsockname()[1]
inbound_port,api_port=free_port(),free_port()
def echo(client):
    with client:
        client.settimeout(.2)
        while not stop.is_set():
            try:
                data=client.recv(16384)
                if not data: return
                client.sendall(data)
            except socket.timeout: pass
            except OSError: return
def serve():
    while not stop.is_set():
        try: client,_=listener.accept()
        except socket.timeout: continue
        except OSError: return
        threading.Thread(target=echo,args=(client,),daemon=True).start()
threading.Thread(target=serve,daemon=True).start()
def run(*args):
    p=subprocess.run(args,capture_output=True,text=True,timeout=30)
    if p.returncode: raise RuntimeError(f'Command {args[0:2]} failed: {p.stderr[:250]}')
    return p.stdout
def exact(s,n):
    b=b''
    while len(b)<n:
        part=s.recv(n-len(b))
        if not part: raise EOFError('stream closed')
        b+=part
    return b
def connect(which):
    s=socket.create_connection(('127.0.0.1',inbound_port),3); s.settimeout(2)
    payload=(b'fixture-'+which.encode())*32
    s.sendall(b'\0'+uuid.UUID(ids[which]).bytes+b'\0\1'+struct.pack('!H',echo_port)+b'\1'+socket.inet_aton('127.0.0.1')+payload)
    try:
        response=exact(s,2); assert response[0]==0
        if response[1]: exact(s,response[1])
        assert exact(s,len(payload))==payload
        return s
    except BaseException:
        s.close(); raise
def exchange(s):
    payload=uuid.uuid4().bytes*256; s.sendall(payload); assert exact(s,len(payload))==payload
def closed(s):
    try:
        s.sendall(b'blocked'); data=s.recv(32)
        assert data==b'', 'Revoked stream still receives data'
    except (ConnectionError,EOFError): pass
def remove(which):
    output=run(binary,'api','rmu','--server=127.0.0.1:'+str(api_port),'-tag=fixture',emails[which])
    assert 'Removed 1 user(s)' in output, 'Core rejected removal'
with tempfile.TemporaryDirectory(prefix='remnacust-device-live-') as directory:
    root=pathlib.Path(directory)
    clients=[{'id':ids[k],'email':emails[k]} for k in ids]
    config={'log':{'loglevel':'debug'},'api':{'tag':'api','listen':'127.0.0.1:'+str(api_port),'services':['HandlerService']},
        'inbounds':[{'tag':'fixture','listen':'127.0.0.1','port':inbound_port,'protocol':'vless','settings':{'clients':clients,'decryption':'none'}}],
        'outbounds':[{'protocol':'freedom','tag':'direct','settings':{'finalRules':[{'action':'allow','network':'tcp','port':echo_port,'ip':['127.0.0.1/32']}]}}]}
    (root/'config.json').write_text(json.dumps(config))
    for k in ids:
        added={**config,'inbounds':[{**config['inbounds'][0],'settings':{'clients':[{'id':ids[k],'email':emails[k]}],'decryption':'none'}}]}
        (root/(k+'.json')).write_text(json.dumps(added))
    sockets=[]
    try:
        log=(root/'core.log').open('w')
        process=subprocess.Popen([binary,'run','-config',str(root/'config.json')],stdout=log,stderr=log)
        for attempt in range(30):
            try: a=connect('A'); break
            except (OSError,EOFError): time.sleep(.15)
        else: raise RuntimeError('Core not ready')
        b=connect('B'); shared=connect('shared'); sockets=[a,b,shared]
        assert a.getsockname()[0]==b.getsockname()[0]==shared.getsockname()[0]=='127.0.0.1'
        remove('A'); time.sleep(.15); closed(a)
        for i in range(12): exchange(b); exchange(shared)
        try:
            unexpected=connect('A'); unexpected.close(); raise AssertionError('Removed device authenticated')
        except (OSError,EOFError): pass
        print('PASS live VLESS: device A revoked; same-IP device B and shared credential continue; reconnect A rejected')
        output=run(binary,'api','adu','--server=127.0.0.1:'+str(api_port),str(root/'A.json'))
        assert 'Added 1 user(s)' in output
        renewed=connect('A'); sockets.append(renewed); exchange(renewed)
        print('PASS re-add clears revocation guard; new device A connection works')
        remove('shared'); time.sleep(.15); closed(shared)
        for i in range(12): exchange(renewed); exchange(b)
        print('PASS shared-credential cutover terminates only shared stream; both personal device streams remain active')
    finally:
        for s in sockets: s.close()
        process.terminate()
        try: process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill(); process.wait()
        log.close()
        stop.set(); listener.close()
