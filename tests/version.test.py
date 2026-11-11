"""Check declared source and executable versions before publishing."""
import json
import pathlib
import re
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parents[1]
expected = (root / 'VERSION').read_text().strip()
source = (root / 'xray/core/core.go').read_text()
actual = '.'.join(re.search(r'Version_' + axis + r'\s+byte\s*=\s*(\d+)', source).group(1) for axis in 'xyz')
assert actual == expected, 'Runtime version differs from VERSION'
wire = '.'.join(re.search(r'UpstreamVersion_' + axis + r'\s+byte\s*=\s*(\d+)', source).group(1) for axis in 'xyz')
metadata = json.loads((root / 'xray/REMNACUST-UPSTREAM.json').read_text())
assert metadata['version'] == expected
assert 'v' + wire == metadata['upstreamVersion'], 'REALITY wire version differs from the reviewed upstream baseline'
modules = (root / 'xray/go.mod').read_text()
for dependency in metadata.get('dependencySecurityUpdates', []):
    declared = re.search(r'^\s*' + re.escape(dependency['module']) + r'\s+(\S+)\s*$', modules, re.MULTILINE)
    assert declared and declared.group(1) == dependency['version'], 'Dependency provenance differs from go.mod: ' + dependency['module']
output = subprocess.check_output([sys.argv[1], 'version'], text=True)
security_toolchain = metadata.get('toolchainSecurityUpdate', {}).get('version')
if security_toolchain:
    required = tuple(map(int, security_toolchain.removeprefix('go').split('.')))
    built = re.search(r'\(go(\d+\.\d+\.\d+) ', output)
    assert built and tuple(map(int, built.group(1).split('.'))) >= required, 'Binary uses an outdated security toolchain'
assert output.startswith('Xray ' + expected + ' '), 'Built executable has the wrong version'
assert f"Based on Xray-core {metadata['upstreamVersion']}." in output, 'Executable banner differs from the pinned official release'
if not metadata.get('upstreamPatchCommit') and not metadata.get('upstreamPatches'):
    assert 'with fixes through' not in output, 'Executable claims an undeclared development patch set'
print('PASS declared and executable core version ' + expected)
