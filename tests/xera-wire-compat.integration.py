import argparse
import base64
import contextlib
import hashlib
import http.server
import json
import os
from pathlib import Path
import re
import shutil
import socket
import socketserver
import ssl
import struct
import subprocess
import tempfile
import threading
import time
import uuid


PAYLOAD_SIZE = 256 * 1024
MODES = ('packet-up', 'stream-up', 'stream-one', 'stream-auto')
ALPN = {'h1': 'http/1.1', 'h2': 'h2', 'h3': 'h3'}
NO_WINDOW = getattr(subprocess, 'CREATE_NO_WINDOW', 0)


def digest(filename):
    with filename.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def command(arguments, timeout=15):
    result = subprocess.run(arguments, capture_output=True, text=True,
                            timeout=timeout, creationflags=NO_WINDOW)
    if result.returncode:
        raise RuntimeError(f'{Path(arguments[0]).name} {arguments[1]} failed')
    return result.stdout


def verify_binary(filename, version):
    filename = filename.resolve(strict=True)
    output = command([str(filename), 'version'])
    first = output.splitlines()[0] if output else ''
    if not re.match(r'^Xray ' + re.escape(version) + r'(?:\s|$)', first):
        raise RuntimeError(f'{filename.name}: expected Xray {version}')
    return filename, digest(filename)


def remaining(deadline):
    value = deadline - time.monotonic()
    if value <= 0:
        raise TimeoutError('fixture deadline exceeded')
    return value


def receive(stream, count, deadline):
    data = bytearray()
    while len(data) < count:
        stream.settimeout(min(10, remaining(deadline)))
        chunk = stream.recv(min(65536, count - len(data)))
        if not chunk:
            raise EOFError(f'stream closed at {len(data)}/{count} bytes')
        data.extend(chunk)
    return bytes(data)


def free_port(host, family, kind=socket.SOCK_STREAM):
    with socket.socket(family, kind) as reservation:
        reservation.bind((host, 0))
        return reservation.getsockname()[1]


def ipv6_available(required):
    try:
        with socket.socket(socket.AF_INET6) as listener:
            listener.settimeout(1)
            listener.bind(('::1', 0))
            listener.listen(1)
            with socket.create_connection(('::1', listener.getsockname()[1]), 1):
                peer, _ = listener.accept()
                peer.close()
        return True
    except OSError as error:
        if required:
            raise RuntimeError(f'required IPv6 loopback unavailable: {error}') from error
        print(f'SKIP IPv6 loopback: {error}', flush=True)
        return False


class Echo(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def do_POST(self):
        self.connection.settimeout(15)
        if self.headers.get('Content-Length') != str(PAYLOAD_SIZE):
            self.send_error(400)
            return
        body = self.rfile.read(PAYLOAD_SIZE)
        if len(body) != PAYLOAD_SIZE:
            self.send_error(400)
            return
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Connection', 'close')
        self.end_headers()
        self.wfile.write(body)
        self.close_connection = True

    def log_message(self, *_):
        pass


@contextlib.contextmanager
def echo_server(host, family):
    server_type = type('LoopbackHTTPServer', (http.server.ThreadingHTTPServer,),
                       {'address_family': family, 'daemon_threads': True,
                        'block_on_close': False})
    server = server_type((host, 0), Echo)
    worker = threading.Thread(target=server.serve_forever,
                              kwargs={'poll_interval': .05}, daemon=True)
    worker.start()
    try:
        yield server.server_port
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=2)


class Decoy(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(8)
        try:
            with self.server.tls.wrap_socket(self.request, server_side=True) as stream:
                if stream.selected_alpn_protocol() == 'h2':
                    stream.sendall(b'\x00\x00\x00\x04\x00\x00\x00\x00\x00')
                while stream.recv(16384):
                    pass
        except (OSError, ssl.SSLError):
            pass


@contextlib.contextmanager
def reality_decoy(cert, key):
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.minimum_version = ssl.TLSVersion.TLSv1_3
    tls.maximum_version = ssl.TLSVersion.TLSv1_3
    tls.set_ecdh_curve('X25519')
    tls.set_alpn_protocols(['h2'])
    tls.load_cert_chain(cert, key)
    server_type = type('LoopbackTLSDecoy', (socketserver.ThreadingTCPServer,),
                       {'daemon_threads': True, 'block_on_close': False})
    server = server_type(('127.0.0.1', 0), Decoy)
    server.tls = tls
    worker = threading.Thread(target=server.serve_forever,
                              kwargs={'poll_interval': .05}, daemon=True)
    worker.start()
    try:
        verify = ssl.create_default_context(cafile=str(cert))
        verify.minimum_version = ssl.TLSVersion.TLSv1_3
        verify.maximum_version = ssl.TLSVersion.TLSv1_3
        verify.set_ecdh_curve('X25519')
        verify.set_alpn_protocols(['h2'])
        with socket.create_connection(server.server_address, 2) as connection:
            with verify.wrap_socket(connection, server_hostname='localhost') as stream:
                if stream.version() != 'TLSv1.3' or stream.selected_alpn_protocol() != 'h2':
                    raise RuntimeError('REALITY decoy did not negotiate TLS 1.3 and h2')
        yield f'127.0.0.1:{server.server_address[1]}'
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=2)


class CoreProcess:
    def __init__(self, binary, config, directory, role):
        self.binary = binary
        self.config = directory / f'{role}.json'
        self.config.write_text(json.dumps(config), encoding='utf-8')
        self.logfile = directory / f'{role}.log'
        self.log = None
        self.process = None

    def __enter__(self):
        self.log = self.logfile.open('wb')
        environment = {key: value for key, value in os.environ.items()
                       if not key.upper().startswith(('XRAY_', 'REMNACUST_'))}
        environment['XRAY_LOCATION_ASSET'] = str(self.config.parent)
        try:
            self.process = subprocess.Popen(
                [str(self.binary), 'run', '-config', str(self.config)],
                cwd=self.config.parent, env=environment, stdout=self.log,
                stderr=subprocess.STDOUT, creationflags=NO_WINDOW)
        except BaseException:
            self.log.close()
            raise
        return self

    def __exit__(self, *_):
        if self.process.poll() is None:
            self.process.terminate()
        try:
            self.process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=3)
        finally:
            self.log.close()

    def check(self):
        if self.process.poll() is not None:
            raise RuntimeError(f'{self.binary.name} exited with {self.process.returncode}')


def wait_socks(processes, host, port, deadline):
    while True:
        for process in processes:
            process.check()
        try:
            with socket.create_connection((host, port), min(.2, remaining(deadline))):
                return
        except (ConnectionError, socket.timeout):
            remaining(deadline)
            time.sleep(.03)


def exchange(host, socks_port, target_port, label, deadline):
    payload = hashlib.shake_256(label.encode('ascii')).digest(PAYLOAD_SIZE)
    with socket.create_connection((host, socks_port), min(5, remaining(deadline))) as stream:
        stream.settimeout(min(10, remaining(deadline)))
        stream.sendall(b'\x05\x01\x00')
        if receive(stream, 2, deadline) != b'\x05\x00':
            raise AssertionError('SOCKS authentication negotiation failed')
        family = socket.AF_INET6 if ':' in host else socket.AF_INET
        address = socket.inet_pton(family, host)
        stream.sendall(b'\x05\x01\x00' + bytes([4 if family == socket.AF_INET6 else 1])
                       + address + struct.pack('!H', target_port))
        reply = receive(stream, 4, deadline)
        if reply[:3] != b'\x05\x00\x00':
            raise AssertionError(f'SOCKS CONNECT failed with status {reply[1]}')
        length = {1: 4, 4: 16}.get(reply[3])
        if length is None:
            if reply[3] != 3:
                raise AssertionError('invalid SOCKS address type')
            length = receive(stream, 1, deadline)[0]
        receive(stream, length + 2, deadline)
        request = (f'POST /{label} HTTP/1.1\r\nHost: localhost\r\n'
                   f'Content-Length: {len(payload)}\r\nConnection: close\r\n\r\n').encode('ascii')
        stream.settimeout(min(15, remaining(deadline)))
        stream.sendall(request + payload)
        header = bytearray()
        while not header.endswith(b'\r\n\r\n'):
            if len(header) >= 16384:
                raise AssertionError('oversized HTTP response headers')
            header.extend(receive(stream, 1, deadline))
        lines = header.decode('iso-8859-1').split('\r\n')
        if lines[0].split()[:2] != ['HTTP/1.1', '200']:
            raise AssertionError('HTTP echo did not return 200')
        lengths = [line.split(':', 1)[1].strip() for line in lines[1:]
                   if line.lower().startswith('content-length:')]
        if lengths != [str(PAYLOAD_SIZE)]:
            raise AssertionError('HTTP echo length differs')
        if receive(stream, PAYLOAD_SIZE, deadline) != payload:
            raise AssertionError('256 KiB echo payload differs')


def stream_config(mode, security, alpn, cert, key, keys, decoy, server):
    config = {'network': 'xera-http', 'security': security,
              'xeraHttpSettings': {'path': '/wire-compat/', 'mode': mode}}
    if security == 'tls':
        settings = {'alpn': [ALPN[alpn]], 'minVersion': '1.3', 'maxVersion': '1.3'}
        if server:
            settings['certificates'] = [{'certificateFile': str(cert), 'keyFile': str(key)}]
        else:
            settings.update({'serverName': 'localhost',
                             'allowInsecure': False, 'disableSystemRoot': True,
                             'certificates': [{'certificateFile': str(cert), 'usage': 'verify'}]})
        config['tlsSettings'] = settings
    elif server:
        config['realitySettings'] = {'target': decoy, 'serverNames': ['localhost'],
                                     'privateKey': keys[0], 'shortIds': ['0123456789abcdef']}
    else:
        config['realitySettings'] = {'serverName': 'localhost', 'fingerprint': 'chrome',
                                     'publicKey': keys[1], 'shortId': '0123456789abcdef'}
    return config


def run_case(root, binaries, host, family, echo_port, cert, key, security,
             http_version, mode, reverse, keys, decoy, timeout, overall_deadline):
    direction = 'current-client_previous-server' if reverse else 'previous-client_current-server'
    label = f'{"ipv6" if family == socket.AF_INET6 else "ipv4"}-{security}-{http_version}-{mode}-{direction}'
    directory = root / label
    directory.mkdir()
    deadline = min(overall_deadline, time.monotonic() + timeout)
    transport_port = free_port(host, family, socket.SOCK_DGRAM if http_version == 'h3' else socket.SOCK_STREAM)
    socks_port = free_port(host, family)
    while socks_port == transport_port:
        socks_port = free_port(host, family)
    credential = str(uuid.uuid4())
    server_config = {
        'log': {'loglevel': 'warning'},
        'inbounds': [{'listen': host, 'port': transport_port, 'protocol': 'vless',
                      'settings': {'clients': [{'id': credential}], 'decryption': 'none'},
                      'streamSettings': stream_config(mode, security, http_version, cert, key, keys, decoy, True)}],
        'outbounds': [{'protocol': 'freedom', 'settings': {'finalRules': [
            {'action': 'allow', 'network': 'tcp', 'ip': [host + ('/128' if family == socket.AF_INET6 else '/32')],
             'port': echo_port}]}}],
    }
    client_config = {
        'log': {'loglevel': 'warning'},
        'inbounds': [{'listen': host, 'port': socks_port, 'protocol': 'socks',
                      'settings': {'auth': 'noauth', 'udp': False}}],
        'outbounds': [{'protocol': 'vless', 'settings': {'vnext': [
            {'address': host, 'port': transport_port,
             'users': [{'id': credential, 'encryption': 'none'}]}]},
            'streamSettings': stream_config(mode, security, http_version, cert, key, keys, decoy, False)}],
    }
    client_binary, server_binary = binaries if reverse else binaries[::-1]
    try:
        with contextlib.ExitStack() as stack:
            server = stack.enter_context(CoreProcess(server_binary, server_config, directory, 'server'))
            client = stack.enter_context(CoreProcess(client_binary, client_config, directory, 'client'))
            wait_socks((server, client), host, socks_port, min(deadline, time.monotonic() + 8))
            exchange(host, socks_port, echo_port, label, deadline)
            server.check()
            client.check()
        print(f'PASS {label} bytes={PAYLOAD_SIZE}', flush=True)
        return True
    except (OSError, EOFError, AssertionError, RuntimeError) as error:
        print(f'FAIL {label}: {error}', flush=True)
        for filename in ('server.log', 'client.log'):
            logfile = directory / filename
            if logfile.exists():
                text = logfile.read_bytes()[-8192:].decode('utf-8', errors='replace')
                for secret in keys:
                    text = text.replace(secret, '[redacted]')
                print(f'{filename}: {text}', flush=True)
        return False


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--current', type=Path, required=True)
    parser.add_argument('--previous', type=Path, required=True)
    parser.add_argument('--previous-sha256')
    parser.add_argument('--openssl', default=shutil.which('openssl'))
    parser.add_argument('--security', choices=('all', 'tls', 'reality'), default='all')
    parser.add_argument('--timeout', type=float, default=25)
    parser.add_argument('--total-timeout', type=float, default=360)
    args = parser.parse_args()
    if not 3 <= args.timeout <= 60 or not 30 <= args.total_timeout <= 1800:
        parser.error('timeout must be 3..60 and total-timeout 30..1800 seconds')
    if not args.openssl:
        parser.error('OpenSSL CLI is required')
    try:
        current, current_digest = verify_binary(args.current, '1.1.21')
        previous, previous_digest = verify_binary(args.previous, '1.1.20')
    except (OSError, RuntimeError, subprocess.SubprocessError) as error:
        parser.error(str(error))
    if args.previous_sha256 and (not re.fullmatch(r'[0-9a-fA-F]{64}', args.previous_sha256)
                                or previous_digest != args.previous_sha256.lower()):
        parser.error('previous binary SHA256 differs')
    print(f'CURRENT 1.1.21 sha256={current_digest}', flush=True)
    print(f'PREVIOUS 1.1.20 sha256={previous_digest}', flush=True)
    overall_deadline = time.monotonic() + args.total_timeout
    families = [('127.0.0.1', socket.AF_INET)]
    if ipv6_available(os.environ.get('REMNACUST_REQUIRE_IPV6_TESTS') == '1'):
        families.append(('::1', socket.AF_INET6))
    passed = failed = 0
    with tempfile.TemporaryDirectory(prefix='remnacust-xera-wire-') as temporary:
        root = Path(temporary)
        cert, key = root / 'localhost.pem', root / 'localhost-key.pem'
        command([args.openssl, 'req', '-x509', '-newkey', 'rsa:2048', '-sha256', '-days', '1',
                 '-nodes', '-keyout', str(key), '-out', str(cert), '-subj', '/CN=localhost',
                 '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1,IP:::1',
                 '-addext', 'basicConstraints=critical,CA:TRUE'])
        with contextlib.ExitStack() as stack:
            targets = [(host, family, stack.enter_context(echo_server(host, family)))
                       for host, family in families]
            securities = ('tls', 'reality') if args.security == 'all' else (args.security,)
            for security in securities:
                keys, decoy = (), None
                if security == 'reality':
                    output = command([str(current), 'x25519'])
                    private = re.search(r'^PrivateKey:\s*([A-Za-z0-9_-]+)\s*$', output, re.M)
                    public = re.search(r'^(?:Password \(PublicKey\)|PublicKey):\s*([A-Za-z0-9_-]+)\s*$', output, re.M)
                    if not private or not public:
                        raise RuntimeError('Core x25519 output format differs')
                    keys = (private[1], public[1])
                    if any(len(base64.urlsafe_b64decode(value + '=' * (-len(value) % 4))) != 32 for value in keys):
                        raise RuntimeError('Core x25519 returned invalid key lengths')
                    decoy = stack.enter_context(reality_decoy(cert, key))
                for host, family, echo_port in targets:
                    for http_version in ALPN if security == 'tls' else ('h2',):
                        for mode in MODES:
                            for reverse in (False, True):
                                remaining(overall_deadline)
                                good = run_case(root, (current, previous), host, family, echo_port,
                                                cert, key, security, http_version, mode, reverse,
                                                keys, decoy, args.timeout, overall_deadline)
                                passed += int(good)
                                failed += int(not good)
    if digest(current) != current_digest or digest(previous) != previous_digest:
        raise RuntimeError('a binary changed during the interoperability matrix')
    print(f'RESULT passed={passed} failed={failed} families={len(families)} bytes_per_case={PAYLOAD_SIZE}', flush=True)
    return int(failed != 0)


if __name__ == '__main__':
    raise SystemExit(main())
