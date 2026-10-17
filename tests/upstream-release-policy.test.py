"""Refuse development branch patch sets as the declared upstream baseline."""
import json
from pathlib import Path
import re
import subprocess
import sys

source = Path(__file__).resolve().parents[1] / 'xray/REMNACUST-UPSTREAM.json'
metadata = json.loads(source.read_text(encoding='utf-8'))
assert metadata['updatePolicy'] == 'published-official-releases-only'
assert metadata['upstreamRepository'] == 'https://github.com/XTLS/Xray-core'
version = metadata['upstreamVersion']
assert re.fullmatch(r'v\d+\.\d+\.\d+', version)
assert metadata['upstreamRef'] == 'refs/tags/' + version
assert not metadata.get('upstreamPatchCommit'), 'Post-release main commits are not an allowed baseline'
assert metadata.get('upstreamPatches') == [], 'Post-release patch sets are disabled'
assert re.fullmatch(r'[0-9a-f]{40}', metadata['upstreamCommit'])
if '--verify-remote' in sys.argv:
    ref = metadata['upstreamRef']
    rows = subprocess.check_output(['git', 'ls-remote', '--tags', metadata['upstreamRepository'], ref, ref + '^{}'], text=True, timeout=60)
    references = {name: sha for sha, name in (line.split() for line in rows.splitlines())}
    assert references.get(ref + '^{}', references.get(ref)) == metadata['upstreamCommit'], 'Pinned commit is not the published official tag'
print('PASS official release tag ' + version + '; no development patch set')
