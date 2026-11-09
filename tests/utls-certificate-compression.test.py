import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
source = root / 'vendor/utls'
metadata = json.loads((source / 'REMNACUST-PATCH.json').read_text(encoding='utf-8'))
module, version = metadata['module'], metadata['version']
parser = argparse.ArgumentParser()
parser.add_argument('--race', action='store_true')
parser.add_argument('--verify-upstream', action='store_true')
args = parser.parse_args()
go = os.environ.get('GO', 'go')

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def run(*command, capture=False, cwd=root / 'xray'):
    return subprocess.run(command, cwd=cwd, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None, timeout=180)

expected = metadata['upstreamFiles'] | metadata['patchedFiles']
extras = {'REMNACUST-PATCH.json', 'certificate-decompression.patch', 'remnacust_certificate_compression_test.go'}
actual = {p.relative_to(source).as_posix() for p in source.rglob('*') if p.is_file()}
if actual != set(expected) | extras:
    raise RuntimeError('Local uTLS source inventory differs from the reviewed subset')
for name, checksum in expected.items():
    if digest(source / name) != checksum:
        raise RuntimeError(f'Local uTLS integrity mismatch: {name}')
patch = source / 'certificate-decompression.patch'
if digest(patch) != metadata['patchSha256']:
    raise RuntimeError('Certificate decompression patch integrity mismatch')
with tempfile.TemporaryDirectory(prefix='remnacust-utls-integrity-') as temporary:
    workspace = Path(temporary)
    for name in metadata['patchedFiles']:
        (workspace / name).write_bytes((source / name).read_bytes())
    if metadata['patchedFiles']:
        run('git', 'apply', '--reverse', str(patch), cwd=workspace)
    elif patch.read_bytes() or metadata.get('certificateCompressionFix') != 'included-upstream':
        raise RuntimeError('Unexpected untracked certificate patch')
    for name in metadata['patchedFiles']:
        if digest(workspace / name) != metadata['upstreamFiles'][name]:
            raise RuntimeError(f'Patch does not reproduce the upstream baseline: {name}')

for folder in [root / 'xray', root / 'editor/go']:
    pins = re.findall(r'^\s*' + re.escape(module) + r'\s+(\S+)\s*$', (folder / 'go.mod').read_text(encoding='utf-8'), re.M)
    if pins != [version]:
        raise RuntimeError(f'{folder}: expected uTLS baseline {version}, got {pins}')
    selected = json.loads(run(go, 'list', '-mod=readonly', '-m', '-json', module, capture=True, cwd=folder).stdout)
    if selected.get('Version') != version or Path(selected.get('Replace', {}).get('Dir', '')).resolve() != source.resolve():
        raise RuntimeError(f'{folder}: selected uTLS source differs from the reviewed backport')

if args.verify_upstream:
    downloaded = json.loads(run(go, 'mod', 'download', '-json', module + '@' + version, capture=True).stdout)
    if downloaded.get('Sum') != metadata['moduleSum']:
        raise RuntimeError('Upstream module checksum mismatch')
    upstream = Path(downloaded['Dir'])
    for name, checksum in metadata['upstreamFiles'].items():
        if digest(upstream / name) != checksum:
            raise RuntimeError(f'Upstream source integrity mismatch: {name}')

command = [go, 'test', '-mod=readonly', '-count=1', '-timeout=60s',
           '-run=^TestRemnacustCertificateCompression', '-v']
if args.race:
    command.append('-race')
run(*command, module)
print('PASS reproducible uTLS backport, native/editor graph and codec regressions')
