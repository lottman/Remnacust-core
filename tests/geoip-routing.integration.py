"""Exercise domain-to-IP routing against a loopback HTTP target, without a VPN."""
import http.server
import json
from pathlib import Path
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time

binary = str(Path(sys.argv[1]).resolve())

class Target(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'geoip-direct-ok')
    def log_message(self, *_):
        pass

target = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Target)
thread = threading.Thread(target=target.serve_forever, daemon=True)
thread.start()

def exact(stream, count):
    result = b''
    while len(result) < count:
        chunk = stream.recv(count-len(result))
        if not chunk:
            raise OSError('SOCKS connection closed')
        result += chunk
    return result

def fetch(port, host):
    with socket.create_connection(('127.0.0.1', port), timeout=3) as stream:
        stream.sendall(b'\x05\x01\x00')
        assert exact(stream, 2) == b'\x05\x00'
        domain = host.encode('ascii')
        stream.sendall(b'\x05\x01\x00\x03' + bytes([len(domain)]) + domain + struct.pack('!H', target.server_port))
        reply = exact(stream, 4)
        if reply[1] != 0:
            return b''
        size = {1:4, 4:16}.get(reply[3])
        if size is None:
            size = exact(stream, 1)[0]
        exact(stream, size+2)
        stream.sendall(b'GET / HTTP/1.0\r\nHost: direct.example.test\r\n\r\n')
        body = b''
        while chunk := stream.recv(4096):
            body += chunk
            if len(body) > 16384:
                raise AssertionError('Unexpected HTTP response size')
        return body

try:
    for strategy in ['IPIfNonMatch', 'IPOnDemand']:
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0))
            port = reservation.getsockname()[1]
        config = {
            'log': {'loglevel':'warning'},
            'dns': {'hosts': {'direct.example.test':'127.0.0.1', 'manual.example.test':'127.0.0.1'}},
            'inbounds': [{'listen':'127.0.0.1', 'port':port, 'protocol':'socks', 'settings':{'auth':'noauth'}}],
            'outbounds': [{'tag':'blocked', 'protocol':'blackhole'}, {'tag':'geo-freedom', 'protocol':'freedom', 'settings':{'domainStrategy':'UseIP'}}],
            'routing': {'domainStrategy':strategy, 'rules':[
                {'type':'field', 'domain':['full:manual.example.test'], 'outboundTag':'blocked'},
                {'type':'field', 'ip':['127.0.0.1/32'], 'outboundTag':'geo-freedom'},
                {'type':'field', 'network':'tcp,udp', 'outboundTag':'blocked'},
            ]},
        }
        with tempfile.TemporaryDirectory(prefix='remnacust-geoip-') as directory:
            file = Path(directory)/'config.json'
            file.write_text(json.dumps(config))
            with (Path(directory)/'core.log').open('w+') as log:
                process = subprocess.Popen([binary, 'run', '-config', str(file)], stdout=log, stderr=log)
                try:
                    for _ in range(80):
                        if process.poll() is not None:
                            log.seek(0)
                            raise AssertionError('Core exited: '+log.read())
                        try:
                            with socket.create_connection(('127.0.0.1', port), timeout=.1):
                                break
                        except OSError:
                            time.sleep(.05)
                    else:
                        raise AssertionError('Core listener timeout')
                    def attempt(host):
                        try:
                            return b'geoip-direct-ok' in fetch(port, host)
                        except OSError:
                            return False
                    assert attempt('direct.example.test') == (strategy == 'IPOnDemand'), strategy
                    assert not attempt('manual.example.test'), 'Earlier manual rules lost priority'
                finally:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
    print('PASS native GeoIP domain resolution, catch-all regression and manual-rule priority')
finally:
    target.shutdown()
    target.server_close()
