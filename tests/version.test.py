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
assert json.loads((root / 'xray/REMNACUST-UPSTREAM.json').read_text())['version'] == expected
output = subprocess.check_output([sys.argv[1], 'version'], text=True)
assert output.startswith('Xray ' + expected + ' '), 'Built executable has the wrong version'
print('PASS declared and executable core version ' + expected)
