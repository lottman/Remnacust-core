import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import zipfile

SOURCE_ARCHIVES = {
    'a4a3dff5b20c2faae567b742c172c7b95e75c083': 'dcaec8dbf4520883c63544f48c972807cc2445f2a932b8fe63867361ef32084c',
}

def digest(data):
    return hashlib.sha256(data).hexdigest()


def write_json(filename, value):
    filename.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n', encoding='utf-8', newline='\n')


def schema_for(source, definitions):
    definitions = json.loads(json.dumps(definitions).replace('RemnacustCore', 'XrayDefault'))
    ref = lambda name: {'$ref': '#/definitions/XrayDefault' + name}
    text = (source / 'infra/conf/xray.go').read_text(encoding='utf-8')
    for loader, config in [('inbound', 'InboundDetourConfig'), ('outbound', 'OutboundDetourConfig')]:
        section = text.split(loader + 'ConfigLoader =', 1)[1].split('}, "protocol", "settings")', 1)[0]
        bindings = re.findall(r'"([\w-]+)":\s*func\(\) interface\{\} \{ return (?:new\((\w+)\)|&(\w+)\{)', section)
        if not bindings:
            raise RuntimeError('Upstream protocol registry changed')
        definition = definitions['XrayDefault' + config]
        definition['properties']['protocol'] = {'enum': [protocol for protocol, _, _ in bindings]}
        definition['allOf'] = [
            {'if': {'required': ['protocol'], 'properties': {'protocol': {'const': protocol}}},
             'then': {'properties': {'settings': ref(first or second)}}}
            for protocol, first, second in bindings
        ]
    stream = definitions['XrayDefaultStreamConfig']['properties']
    transport = (source / 'infra/conf/transport_internet.go').read_text(encoding='utf-8').split('type StreamConfig', 1)[0]
    networks = []
    for case in re.split(r'\bcase ', transport)[1:]:
        names, body = case.split(':', 1)
        body = body.split('default:', 1)[0]
        if re.search(r'return "[^"]+", nil', body):
            networks.extend(re.findall(r'"([\w-]+)"', names))
    for name in ['method', 'network']:
        stream[name] = {'enum': networks}
    definitions['XrayDefaultStreamConfig']['not'] = {'required': ['xeraHttpSettings']}
    root = definitions['XrayDefaultConfig']
    root['not'] = {'required': ['remnacustUserQuotaRequired'],
                   'properties': {'remnacustUserQuotaRequired': {'not': {'const': False}}}}
    routing = root['properties'].get('routing')
    if routing:
        root['allOf'] = [{'properties': {'routing': {'properties': {'rules': {'items': {'not': {'required': ['webhook']}}}}}}}]
    return {'$schema': 'http://json-schema.org/draft-07/schema#',
            'title': 'Xray ' + metadata['upstreamVersion'] + ' (official upstream)',
            **ref('Config'), 'definitions': definitions}


parser = argparse.ArgumentParser()
parser.add_argument('--source-archive', type=Path, required=True)
parser.add_argument('--output', type=Path, required=True)
parser.add_argument('--go', default='go')
args = parser.parse_args()
editor = Path(__file__).resolve().parent
metadata = json.loads((editor.parents[1] / 'xray/REMNACUST-UPSTREAM.json').read_text(encoding='utf-8'))
if not args.output.is_dir():
    raise SystemExit('Output asset directory must already exist')
if args.source_archive.stat().st_size > 64 * 1024 * 1024 or digest(args.source_archive.read_bytes()) != SOURCE_ARCHIVES.get(metadata['upstreamCommit']):
    raise SystemExit('Source archive does not match the reviewed official upstream archive SHA256')
go = str(Path(args.go).resolve()) if Path(args.go).is_file() else args.go
with tempfile.TemporaryDirectory(prefix='remnacust-default-validator-') as temporary:
    workspace = Path(temporary)
    with zipfile.ZipFile(args.source_archive) as archive:
        if archive.comment.decode('ascii') != metadata['upstreamCommit']:
            raise RuntimeError('Archive is not the pinned official upstream commit')
        prefix = 'Xray-core-' + metadata['upstreamVersion'].removeprefix('v') + '/'
        if sum(item.file_size for item in archive.infolist()) > 256 * 1024 * 1024:
            raise RuntimeError('Upstream archive is too large')
        for item in archive.infolist():
            if not item.filename.startswith(prefix) or '..' in Path(item.filename).parts or '\\' in item.filename:
                raise RuntimeError('Unsafe upstream archive path')
        archive.extractall(workspace)
    source = workspace / prefix.rstrip('/')
    builder_hash = hashlib.sha256()
    for filename in sorted((source / 'infra/conf').glob('*.go')):
        if not filename.name.endswith('_test.go'):
            builder_hash.update(filename.name.encode() + b'\0' + filename.read_bytes())
    host_env = os.environ.copy()
    for name in ['GOOS', 'GOARCH', 'GOFLAGS', 'GOWORK']:
        host_env.pop(name, None)
    host_env['GOTOOLCHAIN'] = 'local'
    definitions_path = workspace / 'types.json'
    subprocess.run([go, 'run', str(editor / 'tools/schema-types.go'), str(source / 'infra/conf'), str(definitions_path)],
                   cwd=source, env=host_env, check=True, timeout=120)
    schema = schema_for(source, json.loads(definitions_path.read_text(encoding='utf-8')))
    adaptations = []
    for filename in source.rglob('*.go'):
        if filename.name.endswith('_test.go'):
            continue
        text = filename.read_text(encoding='utf-8')
        if 'len(syscall.RawSockaddrUnix{}.Path)' not in text:
            continue
        text = text.replace('len(syscall.RawSockaddrUnix{}.Path)', '108')
        if not re.search(r'\bsyscall\.', re.sub(r'//[^\n]*', '', text)):
            text = text.replace('\n\t"syscall"', '')
        filename.write_text(text, encoding='utf-8', newline='\n')
        adaptations.append(str(filename.relative_to(source)).replace('\\', '/'))
    locker = source / 'transport/internet/filelocker_other.go'
    original = locker.read_text(encoding='utf-8')
    if original.count('//go:build !windows') != 1:
        raise RuntimeError('Upstream file-lock platform guard changed')
    locker.write_text(original.replace('//go:build !windows', '//go:build !windows && !wasm'), encoding='utf-8', newline='\n')
    patch = (editor / 'xray-wasm.patch').read_text(encoding='utf-8')
    chunks = re.split(r'(?=^diff --git )', patch, flags=re.M)
    chunk = next(chunk for chunk in chunks if chunk.startswith('diff --git a/transport/internet/filelocker_wasm.go '))
    shim = '\n'.join(line[1:] for line in chunk.splitlines() if line.startswith('+') and not line.startswith('+++')) + '\n'
    (source / 'transport/internet/filelocker_wasm.go').write_text(shim, encoding='utf-8', newline='\n')
    adaptations.extend(['transport/internet/filelocker_other.go', 'transport/internet/filelocker_wasm.go'])
    filesystem = source / 'common/platform/filesystem/file.go'
    text = filesystem.read_text(encoding='utf-8')
    anchor = '\tpath := platform.GetAssetLocation(local)\n'
    reader = '\tr, err := NewFileReader(path)\n\treturn r, info.Size(), err\n'
    if text.count(anchor) != 1 or text.count(reader) != 1:
        raise RuntimeError('Review upstream browser asset I/O before building the validator')
    text = text.replace('\n\t"path/filepath"', '\n\t"path/filepath"\n\t"runtime"')
    text = text.replace(anchor, anchor + '\tif runtime.GOOS == "js" { return path, nil, nil }\n')
    text = text.replace(reader, '''\tr, err := NewFileReader(path)
\tif err != nil { return nil, 0, err }
\tif runtime.GOOS == "js" {
\t\tif sized, ok := r.(interface{ Size() int64 }); ok { return r, sized.Size(), nil }
\t\tr.Close()
\t\treturn nil, 0, errors.New("embedded asset size unavailable")
\t}
\treturn r, info.Size(), nil
''')
    filesystem.write_text(text, encoding='utf-8', newline='\n')
    adaptations.append('common/platform/filesystem/file.go')
    wrapper = workspace / 'validator'
    wrapper.mkdir()
    shutil.copyfile(editor / 'main.go', wrapper / 'main.go')
    (wrapper / 'assets').mkdir()
    for name in ['geoip.dat', 'geosite.dat']:
        shutil.copyfile(editor / 'assets' / name, wrapper / 'assets' / name)
    module = (source / 'go.mod').read_text(encoding='utf-8')
    module = module.replace('module github.com/xtls/xray-core', 'module github.com/mmmray/xray-online', 1)
    module = module.replace('require (', 'require (\n\tgithub.com/xtls/xray-core v0.0.0', 1)
    (wrapper / 'go.mod').write_text(module + '\nreplace github.com/xtls/xray-core => ../' + source.name + '\n', encoding='utf-8', newline='\n')
    shutil.copyfile(source / 'go.sum', wrapper / 'go.sum')
    wasm = workspace / 'default.wasm'
    env = dict(host_env, GOOS='js', GOARCH='wasm', CGO_ENABLED='0')
    subprocess.run([go, 'build', '-mod=mod', '-trimpath', '-o', str(wasm), 'main.go'], cwd=wrapper, env=env, check=True, timeout=240)
    version = subprocess.check_output([go, 'version'], env=host_env, text=True).strip()
    generated = workspace / 'generated'
    generated.mkdir()
    shutil.copyfile(wasm, generated / 'default.wasm')
    for name in ['xray.default.schema.json', 'xray.default.schema.cn.json']:
        write_json(generated / name, schema)
    provenance = {'flavor': 'default', 'upstreamVersion': metadata['upstreamVersion'],
                  'upstreamCommit': metadata['upstreamCommit'], 'goVersion': version,
                  'sourceArchiveSha256': digest(args.source_archive.read_bytes()),
                  'configurationSourceSha256': builder_hash.hexdigest(),
                  'validatorWrapperSha256': digest((editor / 'main.go').read_bytes()),
                  'browserPlatformAdapters': sorted(adaptations),
                  'files': {name: digest((generated / name).read_bytes()) for name in ['default.wasm', 'xray.default.schema.json', 'xray.default.schema.cn.json']}}
    write_json(generated / 'xray.default.provenance.json', provenance)
    for filename in generated.iterdir():
        target = args.output / filename.name
        shutil.copyfile(filename, target.with_suffix(target.suffix + '.tmp'))
        os.replace(target.with_suffix(target.suffix + '.tmp'), target)
    print(json.dumps(provenance, indent=2))
