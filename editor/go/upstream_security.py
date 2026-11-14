import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import zipfile


SECURITY = Path(__file__).resolve().parents[2] / 'upstream-security'


def digest(data):
    return hashlib.sha256(data).hexdigest()


def configuration_digest(source):
    result = hashlib.sha256()
    for filename in sorted((source / 'infra/conf').glob('*.go')):
        if not filename.name.endswith('_test.go'):
            result.update(filename.name.encode() + b'\0' + filename.read_bytes())
    return result.hexdigest()


def load_manifest(security=SECURITY):
    data = (security / 'manifest.json').read_bytes()
    manifest = json.loads(data)
    if data != (json.dumps(manifest, ensure_ascii=False, indent=2) + '\n').encode():
        raise RuntimeError('Security manifest must use canonical UTF-8 JSON and LF')
    if manifest['schemaVersion'] != 1 or manifest['policy'] != 'official-baseline-with-security-dependency-updates':
        raise RuntimeError('Unsupported upstream security policy')
    if not re.fullmatch(r'[a-f0-9]{40}', manifest['upstreamCommit']):
        raise RuntimeError('Invalid upstream security source commit')
    for field in ['sourceArchiveSha256', 'tagSourceArchiveSha256', 'configurationSourceSha256',
                  'originalGoModSha256', 'originalGoSumSha256', 'goModSha256', 'goSumSha256']:
        if not re.fullmatch(r'[a-f0-9]{64}', manifest[field]):
            raise RuntimeError('Invalid security manifest SHA256: ' + field)
    for name, field in [('go.mod', 'goModSha256'), ('go.sum', 'goSumSha256')]:
        if digest((security / name).read_bytes()) != manifest[field]:
            raise RuntimeError('Security dependency lock SHA256 mismatch: ' + name)
    return manifest


def apply_updates(source, manifest, security=SECURITY):
    for name, field in [('go.mod', 'originalGoModSha256'), ('go.sum', 'originalGoSumSha256')]:
        if digest((source / name).read_bytes()) != manifest[field]:
            raise RuntimeError('Official upstream module SHA256 mismatch: ' + name)
    if configuration_digest(source) != manifest['configurationSourceSha256']:
        raise RuntimeError('Official upstream configuration builders SHA256 mismatch')
    expected = (source / 'go.mod').read_bytes()
    for update in manifest['dependencyOverrides'] + manifest['mvsUpdates']:
        before = ('\t' + update['module'] + ' ' + update['previousVersion']).encode()
        after = ('\t' + update['module'] + ' ' + update['version']).encode()
        if expected.count(before + b'\n') + expected.count(before + b' // indirect\n') != 1:
            raise RuntimeError('Unexpected upstream module version: ' + update['module'])
        expected = expected.replace(before, after, 1)
    if expected != (security / 'go.mod').read_bytes():
        raise RuntimeError('Security overrides change more than the reviewed module versions')
    for name in ['go.mod', 'go.sum']:
        shutil.copyfile(security / name, source / name)


def prepare_source(archive_path, workspace, metadata=None, security=SECURITY):
    manifest = load_manifest(security)
    if metadata and any(manifest[field] != metadata[field] for field in ['upstreamVersion', 'upstreamCommit']):
        raise RuntimeError('Security dependency baseline does not match Core upstream metadata')
    if archive_path.stat().st_size > 64 * 1024 * 1024:
        raise RuntimeError('Official upstream source archive is too large')
    archive_hash = digest(archive_path.read_bytes())
    roots = {
        manifest['sourceArchiveSha256']: 'Xray-core-' + manifest['upstreamCommit'] + '/',
        manifest['tagSourceArchiveSha256']: 'Xray-core-' + manifest['upstreamVersion'].removeprefix('v') + '/',
    }
    if archive_hash not in roots:
        raise RuntimeError('Source archive does not match the reviewed official upstream archive SHA256')
    if workspace.exists() and any(workspace.iterdir()):
        raise RuntimeError('Official source output directory must be empty')
    prefix = roots[archive_hash]
    with zipfile.ZipFile(archive_path) as archive:
        if archive.comment.decode('ascii') != manifest['upstreamCommit']:
            raise RuntimeError('Archive is not the pinned official upstream commit')
        if sum(item.file_size for item in archive.infolist()) > 256 * 1024 * 1024:
            raise RuntimeError('Upstream archive is too large')
        for item in archive.infolist():
            if (not item.filename.startswith(prefix) or '..' in Path(item.filename).parts
                    or '\\' in item.filename or (item.external_attr >> 16) & 0o170000 == 0o120000):
                raise RuntimeError('Unsafe upstream archive path')
        workspace.mkdir(parents=True, exist_ok=True)
        archive.extractall(workspace)
    source = workspace / prefix.rstrip('/')
    apply_updates(source, manifest, security)
    return source, manifest


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--source-archive', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--govulncheck')
    parser.add_argument('--go', default='go')
    args = parser.parse_args()
    go = str(Path(args.go).resolve()) if Path(args.go).is_file() else args.go
    scanner_path = (str(Path(args.govulncheck).resolve()) if args.govulncheck and Path(args.govulncheck).is_file()
                    else args.govulncheck)
    metadata = json.loads((SECURITY.parent / 'xray/REMNACUST-UPSTREAM.json').read_text(encoding='utf-8'))
    source, manifest = prepare_source(args.source_archive, args.output, metadata)
    if args.govulncheck:
        env = dict(os.environ, GOTOOLCHAIN=manifest['goVersion'], GOOS='linux', GOARCH='amd64',
                   CGO_ENABLED='0', GOFLAGS='-mod=readonly', GOWORK='off')
        version = subprocess.check_output([go, 'version'], cwd=source, env=env, text=True)
        if version.split()[2] != manifest['goVersion']:
            raise RuntimeError('Native security scan requires the pinned Go toolchain')
        scanner = subprocess.check_output([scanner_path, '-version'], cwd=source, env=env, text=True)
        if 'Scanner: govulncheck@' + manifest['govulncheckVersion'] + '\n' not in scanner:
            raise RuntimeError('Native security scan requires the pinned govulncheck version')
        print(scanner, end='', flush=True)
        subprocess.run([go, 'mod', 'verify'], cwd=source, env=env, check=True, timeout=240)
        subprocess.run([scanner_path, './...'], cwd=source, env=env, check=True, timeout=600)
        for name, field in [('go.mod', 'goModSha256'), ('go.sum', 'goSumSha256')]:
            if digest((source / name).read_bytes()) != manifest[field]:
                raise RuntimeError('Native security scan changed the reviewed dependency locks')
    print(source.resolve())


if __name__ == '__main__':
    main()
