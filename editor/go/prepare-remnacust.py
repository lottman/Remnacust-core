"""Build the browser validator from the same Remnacust core as the node.

Only runtime socket code is excluded. Configuration builders and validation stay
identical to the native core, including new transports and custom extensions.
"""
from pathlib import Path
import re, shutil, subprocess

editor = Path(__file__).resolve().parent
workspace = editor.parents[1]
core = workspace / 'xray'
target = editor / 'assets/xray-remnacust'
# This is an ignored generated tree. Reusing it after upstream deletes a
# source file would silently keep both old and new transport implementations.
expected_target = (editor / 'assets' / 'xray-remnacust').absolute()
if target.is_symlink() or target.is_junction() or target.resolve() != expected_target:
    raise RuntimeError('Generated core directory must not redirect outside editor assets')
if target.exists():
    shutil.rmtree(target)
target.mkdir(parents=True, exist_ok=True)
for source in core.rglob('*'):
    if source.is_file() and source.suffix in {'.go', '.proto', '.mod', '.sum', '.html', '.pem', '.json'}:
        dest = target / source.relative_to(core)
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, dest)

module = target / 'go.mod'
module.write_text(module.read_text().replace('../vendor/olcrtc', '../../../../vendor/olcrtc'))

# Resolve the validator against the exact same dependency graph, retaining its
# local source substitution rather than an independently aging Go module.
editor_module = (core / 'go.mod').read_text(encoding='utf-8')
editor_module = editor_module.replace('module github.com/xtls/xray-core', 'module github.com/mmmray/xray-online', 1)
editor_module = editor_module.replace('require (\n', 'require (\n\tgithub.com/xtls/xray-core v0.0.0\n', 1)
editor_module = editor_module.replace('../vendor/olcrtc', '../../vendor/olcrtc')
editor_module += '\nreplace github.com/xtls/xray-core => ./assets/xray-remnacust\n'
(editor / 'go.mod').write_text(editor_module, encoding='utf-8', newline='\n')
shutil.copyfile(core / 'go.sum', editor / 'go.sum')

patch = (editor / 'xray-wasm.patch').read_text(encoding='utf-8')
for chunk in re.split(r'(?=^diff --git )', patch, flags=re.M):
    match = re.match(r'diff --git a/(\S+) b/(\S+)', chunk)
    if not match:
        continue
    name = match[2]
    p = target / name
    if '+//go:build !wasm' in chunk and '-//go:build' not in chunk:
        if p.exists():
            p.write_text('//go:build !wasm\n\n' + p.read_text(encoding='utf-8'), encoding='utf-8', newline='\n')
    elif 'new file mode' in chunk:
        data = '\n'.join(line[1:] for line in chunk.splitlines() if line.startswith('+') and not line.startswith('+++')) + '\n'
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(data, encoding='utf-8', newline='\n')

# The browser patch excludes the RPC servers themselves. Native receipt methods
# and the core's WASM runtime shim refer to those servers and cannot be linked
# into this configuration-only program; protobuf config builders remain intact.
(target / 'app/proxyman/command/runtime_wasm.go').unlink()
p = target / 'app/stats/command/usage_receipt.go'
p.write_text('//go:build !wasm\n\n' + p.read_text(encoding='utf-8'), encoding='utf-8', newline='\n')

# The JS platform has no RawSockaddrUnix, but config validation still checks
# names against exactly the same Linux abstract socket length.
for name in ['common/utils/unixsocket.go', 'infra/conf/transport_security.go',
             'infra/conf/trojan.go', 'infra/conf/vless.go', 'transport/internet/system_listener.go']:
    p = target / name
    s = p.read_text(encoding='utf-8').replace('len(syscall.RawSockaddrUnix{}.Path)', '108')
    if not re.search(r'\bsyscall\.', re.sub(r'//[^\n]*', '', s)):
        s = s.replace('\n\t"syscall"', '')
    p.write_text(s, encoding='utf-8', newline='\n')
p = target / 'transport/internet/filelocker_other.go'
s = p.read_text(encoding='utf-8').replace('//go:build !windows', '//go:build !windows && !wasm')
s = re.sub(r'^// \+build.*\n', '', s, flags=re.M)
p.write_text(s, encoding='utf-8', newline='\n')

# Native proxy runtimes require OS facilities, while their protobuf config types
# and pure builders are portable and remain compiled into the validator.
for folder in ['proxy/masque', 'proxy/olcrtc', 'transport/internet/masque',
               'transport/internet/xdrive', 'transport/internet/xerahttp', 'proxy/shadowsocks_2022']:
    for p in (target / folder).glob('*.go'):
        if p.name.endswith(('.pb.go', '_test.go')) or p.name in {'account.go', 'config.go', 'common.go', 'mode.go', 'xpadding.go', 'downlink_padding.go', 'uplink_padding.go', 'tun_default.go', 'xerahttp.go', 'validate.go', 'cipher.go', 'methods.go'}:
            continue
        s = p.read_text(encoding='utf-8')
        if s.startswith('//go:build'):
            s = re.sub(r'^//go:build (.*)', r'//go:build (\1) && !wasm', s, count=1)
            s = re.sub(r'^// \+build.*\n', '', s, flags=re.M)
        else:
            s = '//go:build !wasm\n\n' + s
        p.write_text(s, encoding='utf-8', newline='\n')

# This reverse-proxy worker cannot run in the browser; retaining the pure
# VLESS config/encryption validator avoids weakening configuration validation.
p = target / 'proxy/vless/outbound/outbound.go'
s = p.read_text(encoding='utf-8').replace('\n\tproxyman "github.com/xtls/xray-core/app/proxyman/outbound"', '')
s = s.replace('session.FullHandlerFromContext(ctx).(*proxyman.Handler)', 'nil')
p.write_text(s, encoding='utf-8', newline='\n')
# Browser geodata is embedded and served through NewFileReader. Keep the native
# asset path checks, but avoid os.Stat, which is unavailable in js/wasm.
p = target / 'common/platform/filesystem/file.go'
s = p.read_text(encoding='utf-8')
anchor = '\tpath := platform.GetAssetLocation(local)\n'
if s.count(anchor) != 1:
    raise RuntimeError('Filesystem asset resolver changed; review the WASM adaptation')
s = s.replace('\n\t"path/filepath"', '\n\t"path/filepath"\n\t"runtime"')
s = s.replace(anchor, anchor + '\tif runtime.GOOS == "js" {\n\t\treturn path, nil, nil\n\t}\n')
open_anchor = '\tr, err := NewFileReader(path)\n\treturn r, info.Size(), err\n'
if s.count(open_anchor) != 1:
    raise RuntimeError('Streaming asset reader changed; review the WASM adaptation')
s = s.replace(open_anchor, '''\tr, err := NewFileReader(path)
\tif err != nil { return nil, 0, err }
\tif runtime.GOOS == "js" {
\t\tif sized, ok := r.(interface{ Size() int64 }); ok { return r, sized.Size(), nil }
\t\tr.Close()
\t\treturn nil, 0, errors.New("embedded asset size unavailable")
\t}
\treturn r, info.Size(), nil
''')
p.write_text(s, encoding='utf-8', newline='\n')
print(target)
