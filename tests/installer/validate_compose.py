#!/usr/bin/env python3
"""Validate the real release Compose using disposable generated credentials."""
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('installer', ROOT / 'scripts/familychat.py')
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)

with tempfile.TemporaryDirectory() as temp:
    root = Path(temp).resolve()
    state = installer.configure(root / 'state', 'chat.example.com', 'admin@example.com', '8.8.8.8')
    release = root / 'release'
    installer.extract_bundle(Path(sys.argv[1]).read_bytes(), release)
    installer.link_state(root / 'state', release)
    result = installer.compose(root / 'state', release, state, 'config', '--format', 'json', capture_output=True, text=True)
    config = json.loads(result.stdout)
    assert config['name'] == 'familychat'
    assert config['services']['api']['image'] == config['services']['worker']['image']
    assert '@sha256:' in config['services']['api']['image']
    assert 'build' not in config['services']['api']
    assert not config['services']['api'].get('ports')
    assert config['services']['api']['environment']['ALLOWED_ORIGINS'] == 'chat.example.com'
    assert config['volumes']['postgres_data']['name'] == 'familychat_postgres_data'
    assert config['volumes']['s3_data']['name'] == 'familychat_s3_data'
    assert config['volumes']['encryption_keys']['name'] == 'familychat_encryption_keys'
    assert config['services']['worker']['depends_on']['api']['condition'] == 'service_healthy'
    assert any(v.get('source') == 'encryption_keys' for v in config['services']['api']['volumes'])
    assert config['secrets']['admin_password']['file'] == str(release / 'secrets/admin-password.txt')
print('Release Compose valid: fixed image, stable volumes, HTTPS-only API, generated secrets.')
