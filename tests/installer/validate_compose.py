#!/usr/bin/env python3
"""Validate the real release Compose using disposable generated credentials."""
import importlib.util
import base64
import hashlib
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
    result = installer.compose(root / 'state', release, state, '--profile', 'tools', 'config', '--format', 'json', capture_output=True, text=True)
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
    assert not config['services']['api'].get('volumes')
    assert config['services']['key-setup']['image'] == config['services']['api']['image']
    assert any(v.get('source') == 'encryption_keys' and v.get('read_only') for v in config['services']['key-setup']['volumes'])
    assert {s['source'] for s in config['services']['api']['secrets']} >= {'content_public_key', 'content_private_key'}
    assert not config['services']['worker'].get('secrets')
    for name in ('public', 'private'):
        assert config['secrets']['content_' + name + '_key']['file'] == str(release / ('secrets/content-' + name + '-key.txt'))
    assert config['secrets']['admin_password']['file'] == str(release / 'secrets/admin-password.txt')

    public = bytes(range(32))
    integration = root / 'main-server.json'
    integration.write_text(json.dumps(dict(
        scheme='fc1', key_id=hashlib.sha256(public).hexdigest(),
        public_key=base64.b64encode(public).decode(),
        key_server_url='https://keys.example.com:8443/api/v1/keys/current',
        ticket_issuer='compose-test-family',
        ticket_secret=base64.b64encode(b's' * 32).decode(),
    )))
    installer.configure_key_service(root / 'state', integration)
    result = installer.compose(root / 'state', release, state, '--profile', 'tools', 'config', '--format', 'json', capture_output=True, text=True)
    external = json.loads(result.stdout)
    api = external['services']['api']
    assert {secret['source'] for secret in api['secrets']} == {'admin_password', 'key_service'}
    assert api['environment']['CONTENT_KEY_SERVICE_FILE'] == '/run/secrets/key_service'
    assert not api.get('volumes')
    assert not api.get('ports')
    assert not external['services']['worker'].get('secrets')
    assert external['secrets']['key_service']['file'] == str(release / 'secrets/key-service.json')
    assert external['services']['nginx']['ports'] == config['services']['nginx']['ports']
    assert not (root / 'state/secrets/content-private-key.txt').exists()
print('Release Compose valid: local and external keys, API has no content-key mounts in external mode, HTTPS ports unchanged.')
