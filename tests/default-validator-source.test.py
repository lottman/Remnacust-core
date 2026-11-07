from pathlib import Path
import subprocess
import sys
import tempfile
import zipfile

script = Path(__file__).resolve().parents[1] / 'editor/go/build-default.py'
with tempfile.TemporaryDirectory(prefix='remnacust-default-source-test-') as temporary:
    root = Path(temporary)
    archive = root / 'forged.zip'
    with zipfile.ZipFile(archive, 'w') as output:
        output.comment = b'a4a3dff5b20c2faae567b742c172c7b95e75c083'
        output.writestr('Xray-core-26.10.31/go.mod', 'module attacker.example/fork\n')
    assets = root / 'assets'
    assets.mkdir()
    result = subprocess.run([sys.executable, str(script), '--source-archive', str(archive), '--output', str(assets)],
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=15)
    assert result.returncode != 0
    assert 'SHA256' in result.stderr
    assert not list(assets.iterdir())
print('PASS forged commit comments cannot substitute a different source archive or write assets')
