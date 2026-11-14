import argparse
import hashlib
import json
from pathlib import Path
import shutil
import sys
import tempfile
import zipfile

sys.dont_write_bytecode = True
root = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(root / 'editor/go'))
from upstream_security import SECURITY, apply_updates, configuration_digest, load_manifest, prepare_source

parser = argparse.ArgumentParser()
parser.add_argument('--source-archive', type=Path, required=True)
args = parser.parse_args()
manifest = load_manifest()
assert manifest['upstreamCommit'] == '676eaf66f8f05d4d558ee7a74b4736623d3feeb9'
assert manifest['configurationSourceSha256'] == '01666deb53beae1df9df9fccbca8e807dc7405996cc8a368c1be6713c26e7cba'
assert {entry['module']: entry['version'] for entry in manifest['dependencyOverrides']} == {
    'github.com/klauspost/compress': 'v1.20.4', 'golang.org/x/crypto': 'v0.59.0'}


def rejected(action, message):
    try:
        action()
    except RuntimeError as error:
        assert message in str(error), str(error)
    else:
        raise AssertionError('Accepted an unreviewed upstream security input')


with tempfile.TemporaryDirectory(prefix='remnacust-upstream-security-test-') as temporary:
    workspace = Path(temporary)
    source, _ = prepare_source(args.source_archive, workspace / 'source')
    with zipfile.ZipFile(args.source_archive) as archive:
        prefix = source.name + '/'
        originals = {item.filename[len(prefix):]: archive.read(item) for item in archive.infolist() if not item.is_dir()}
    assert {path.relative_to(source).as_posix() for path in source.rglob('*') if path.is_file()} == set(originals)
    for name, data in originals.items():
        expected = (SECURITY / name).read_bytes() if name in {'go.mod', 'go.sum'} else data
        assert (source / name).read_bytes() == expected, name
    assert configuration_digest(source) == manifest['configurationSourceSha256']
    registry = (source / 'infra/conf/xray.go').read_text(encoding='utf-8').lower()
    assert all(name not in registry for name in ['xera', 'olcrtc', 'remnacust'])
    print('PASS verified official source differs only in the two reviewed module lockfiles; original builders unchanged')

    for name in ['go.mod', 'go.sum', 'infra/conf/xray.go']:
        for module in ['go.mod', 'go.sum']:
            (source / module).write_bytes(originals[module])
        (source / name).write_bytes(originals[name] + b'\n')
        before = [(source / module).read_bytes() for module in ['go.mod', 'go.sum']]
        rejected(lambda: apply_updates(source, manifest), 'SHA256 mismatch')
        assert before == [(source / module).read_bytes() for module in ['go.mod', 'go.sum']]
        (source / name).write_bytes(originals[name])
    print('PASS modified original modules or builders are rejected before replacing either lockfile')

    security = workspace / 'security'
    shutil.copytree(SECURITY, security)
    for name in ['go.mod', 'go.sum']:
        (security / name).write_bytes((SECURITY / name).read_bytes() + b'\n')
        rejected(lambda: load_manifest(security), 'lock SHA256 mismatch')
        shutil.copyfile(SECURITY / name, security / name)
    altered = (security / 'go.mod').read_bytes() + b'\nreplace example.org/custom => ../custom\n'
    (security / 'go.mod').write_bytes(altered)
    changed = dict(manifest, goModSha256=hashlib.sha256(altered).hexdigest())
    (security / 'manifest.json').write_text(json.dumps(changed, indent=2) + '\n', encoding='utf-8', newline='\n')
    changed = load_manifest(security)
    rejected(lambda: apply_updates(source, changed, security), 'more than the reviewed module versions')
    assert (source / 'go.mod').read_bytes() == originals['go.mod']
    print('PASS tampered lockfiles and additional custom replacements fail even with a rehashed manifest')

    forged = workspace / 'forged.zip'
    with zipfile.ZipFile(forged, 'w') as archive:
        archive.comment = manifest['upstreamCommit'].encode()
        archive.writestr(source.name + '/go.mod', originals['go.mod'])
    rejected(lambda: prepare_source(forged, workspace / 'forged-output'), 'archive SHA256')
    assert not (workspace / 'forged-output').exists()
    rejected(lambda: prepare_source(args.source_archive, workspace / 'mismatch',
                                   dict(manifest, upstreamCommit='0' * 40)), 'Core upstream metadata')
    assert not (workspace / 'mismatch').exists()
    rejected(lambda: prepare_source(args.source_archive, source), 'must be empty')
    assert (source / 'go.mod').read_bytes() == originals['go.mod']
    print('PASS forged ZIPs, mismatched baselines and nonempty destinations cannot substitute source')
