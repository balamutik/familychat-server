import base64
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('key_installer', ROOT / 'scripts/familychat.py')
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)

class KeyServiceInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'state'
        self.state = installer.configure(self.root, 'chat.example.com', 'owner@example.com', '8.8.8.8')
        public = bytes(range(32))
        self.config = dict(scheme='fc1', key_id=hashlib.sha256(public).hexdigest(),
            public_key=base64.b64encode(public).decode(), key_server_url='https://keys.example.com:8443/api/v1/keys/current',
            ticket_issuer='test-family', ticket_secret=base64.b64encode(b's'*32).decode())
        self.input = Path(self.temp.name) / 'main-server.json'
        self.input.write_text(json.dumps(self.config)); self.input.chmod(0o600)

    def test_external_configuration_is_private_and_does_not_generate_content_keys(self):
        installer.configure_key_service(self.root, self.input)
        saved = self.root / 'secrets/key-service.json'
        self.assertEqual(self.config, json.loads(saved.read_text()))
        self.assertEqual(0o400, saved.stat().st_mode & 0o777)
        self.assertIn('KEY_SERVICE_CONFIG_FILE=./secrets/key-service.json', (self.root / '.env').read_text())
        self.assertFalse((self.root / 'secrets/content-private-key.txt').exists())
        with patch.object(installer, 'run') as run:
            installer.compose(self.root, ROOT, self.state, 'config', '-q')
        self.assertIn(ROOT / 'compose.keyserver.yaml', run.call_args.args[0])

    def test_mismatched_existing_key_is_rejected_without_changes(self):
        (self.root / 'secrets/content-public-key.txt').write_text(base64.b64encode(b'x'*32).decode())
        before = (self.root / '.env').read_bytes()
        with self.assertRaisesRegex(installer.InstallError, 'ключ'):
            installer.configure_key_service(self.root, self.input)
        self.assertEqual(before, (self.root / '.env').read_bytes())
        self.assertFalse((self.root / 'secrets/key-service.json').exists())

    def test_same_existing_key_preserves_private_backup_without_mounting_it(self):
        (self.root / 'secrets/content-public-key.txt').write_text(self.config['public_key'])
        private = self.root / 'secrets/content-private-key.txt'; private.write_text('original-backup')
        installer.configure_key_service(self.root, self.input)
        self.assertEqual('original-backup', private.read_text())

    def test_invalid_integration_never_changes_configuration(self):
        before = (self.root / '.env').read_bytes()
        for changes in [dict(private_key='must-never-be-imported'), dict(ticket_secret='bad'),
                dict(key_id='0'*64), dict(key_server_url='http://keys.example.com:8443/api/v1/keys/current'),
                dict(key_server_url='https://keys.example.com:443/api/v1/keys/current'),
                dict(key_server_url='https://keys.example.com:0/api/v1/keys/current')]:
            self.input.write_text(json.dumps(self.config | changes))
            with self.assertRaises(installer.InstallError): installer.configure_key_service(self.root, self.input)
            self.assertEqual(before, (self.root / '.env').read_bytes())
            self.assertFalse((self.root / 'secrets/key-service.json').exists())

    def test_external_configuration_cannot_activate_an_old_release(self):
        installer.configure_key_service(self.root, self.input)
        release = self.root / 'releases/v1.0.0'; release.mkdir(parents=True)
        (release / 'release.json').write_text(json.dumps(dict(content_encryption=2)))
        with patch.object(installer, 'run') as run, self.assertRaisesRegex(installer.InstallError, 'сервер ключей'):
            installer.activate(self.root, release, self.state)
        run.assert_not_called()

    def test_managed_upgrade_preserves_external_configuration_and_updates_controller(self):
        installer.configure_key_service(self.root, self.input)
        candidate = self.root / 'releases/v2.3.0'; (candidate / 'scripts').mkdir(parents=True)
        (candidate / 'scripts/familychat.py').write_text('new controller')
        (candidate / 'release.json').write_text(json.dumps(dict(content_encryption=2, external_key_service=1)))
        installer.link_state(self.root, candidate)
        with patch.object(installer, 'install_controller') as controller:
            installer.prepare_managed_upgrade(candidate)
        controller.assert_called_once_with(self.root.resolve(), candidate.resolve() / 'scripts/familychat.py')
        self.assertEqual('v2.3.0', installer.load_state(self.root)['pending_version'])
        self.assertTrue(installer.external_keys_enabled(self.root))

    def release(self, version, **capabilities):
        release = self.root / 'releases' / version
        (release / 'scripts').mkdir(parents=True)
        (release / 'scripts/familychat.py').write_bytes((ROOT / 'scripts/familychat.py').read_bytes())
        (release / 'release.json').write_text(json.dumps(dict(version=version, content_encryption=2,
            external_key_service=1) | capabilities))
        return release

    def test_device_grant_current_release_blocks_external_jwt_downgrade(self):
        installer.configure_key_service(self.root, self.input)
        current = self.release('v2.4.0', device_key_grants=1, key_delivery='device-grant-v1')
        previous = self.release('v2.3.0')
        (self.root / 'current').symlink_to(current)
        with patch.object(installer, 'compose') as compose, patch.object(installer, 'run') as run:
            with self.assertRaisesRegex(installer.InstallError, 'устройств'):
                installer.activate(self.root, previous, self.state)
        compose.assert_not_called(); run.assert_not_called()
        self.assertEqual(current.resolve(), (self.root / 'current').resolve())

    def test_device_grant_state_floor_blocks_downgrade_after_interruption(self):
        installer.configure_key_service(self.root, self.input)
        previous = self.release('v2.3.0')
        candidate = self.release('v2.4.0', device_key_grants=1, key_delivery='device-grant-v1')
        (self.root / 'current').symlink_to(previous)
        with patch.object(installer, 'compose'), patch.object(installer, 'run', side_effect=installer.InstallError('interrupted')):
            with self.assertRaisesRegex(installer.InstallError, 'interrupted'):
                installer.activate(self.root, candidate, self.state)
        persisted = installer.load_state(self.root)
        self.assertEqual(1, persisted['device_key_grants'])
        self.assertEqual(previous.resolve(), (self.root / 'current').resolve())
        with patch.object(installer, 'compose') as compose, patch.object(installer, 'run') as run:
            with self.assertRaisesRegex(installer.InstallError, 'устройств'):
                installer.activate(self.root, previous, persisted)
        compose.assert_not_called(); run.assert_not_called()

    def test_managed_device_grant_upgrade_persists_floor_before_old_controller_can_resume(self):
        installer.configure_key_service(self.root, self.input)
        previous = self.release('v2.3.0')
        candidate = self.release('v2.4.0', device_key_grants=1, key_delivery='device-grant-v1')
        (self.root / 'current').symlink_to(previous)
        installer.link_state(self.root, candidate)
        launcher = self.root / 'familychat-command'
        with patch.object(installer, 'COMMAND_PATH', launcher):
            installer.prepare_managed_upgrade(candidate)
        upgraded_spec = importlib.util.spec_from_file_location('device_grant_manager', self.root / 'manager.py')
        upgraded = importlib.util.module_from_spec(upgraded_spec); upgraded_spec.loader.exec_module(upgraded)
        persisted = upgraded.load_state(self.root)
        self.assertEqual(1, persisted['device_key_grants'])
        with patch.object(upgraded, 'compose') as compose:
            with self.assertRaisesRegex(upgraded.InstallError, 'устройств'):
                upgraded.activate(self.root, previous, persisted)
        compose.assert_not_called()
        installer.link_state(self.root, previous)
        with patch.object(installer, 'install_controller') as controller:
            with self.assertRaisesRegex(installer.InstallError, 'устройств'):
                installer.prepare_managed_upgrade(previous)
        controller.assert_not_called()

    def test_device_grant_capability_requires_matching_delivery_protocol(self):
        current = self.release('v2.4.0', device_key_grants=1, key_delivery='device-grant-v1')
        candidate = self.release('v2.4.1', device_key_grants=1, key_delivery='jwt')
        (self.root / 'current').symlink_to(current)
        with patch.object(installer, 'compose') as compose, patch.object(installer, 'run'):
            with self.assertRaisesRegex(installer.InstallError, 'устройств'):
                installer.activate(self.root, candidate, self.state)
        compose.assert_not_called()

    def test_direct_legacy_installation_without_external_keys_remains_supported(self):
        candidate = self.release('v2.3.0')
        with patch.object(installer, 'compose'), patch.object(installer, 'run'):
            installer.activate(self.root, candidate, self.state)
        self.assertEqual(candidate.resolve(), (self.root / 'current').resolve())
        self.assertEqual(0, installer.load_state(self.root).get('device_key_grants', 0))

    def test_connect_command_requires_device_grant_release_before_configuration_changes(self):
        previous = self.release('v2.3.0')
        (self.root / 'current').symlink_to(previous)
        before = (self.root / '.env').read_bytes()
        with patch.object(installer, 'require_system', return_value=('ubuntu', 'noble')), patch.object(installer, 'compose') as compose:
            with self.assertRaisesRegex(installer.InstallError, 'устройств'):
                installer.main(['--root', str(self.root), 'configure-key-server', '--key-service-config', str(self.input)])
        self.assertEqual(before, (self.root / '.env').read_bytes())
        self.assertFalse((self.root / 'secrets/key-service.json').exists())
        compose.assert_not_called()

    def test_connect_persists_device_grant_floor_before_restarting_api(self):
        candidate = self.release('v2.4.0', device_key_grants=1, key_delivery='device-grant-v1')
        (self.root / 'current').symlink_to(candidate)
        def compose(*args, **kwargs):
            self.assertEqual(1, installer.load_state(self.root)['device_key_grants'])
            raise installer.InstallError('compose failed after protocol checkpoint')
        with patch.object(installer, 'require_system', return_value=('ubuntu', 'noble')), patch.object(installer, 'compose', side_effect=compose):
            with self.assertRaisesRegex(installer.InstallError, 'checkpoint'):
                installer.main(['--root', str(self.root), 'configure-key-server', '--key-service-config', str(self.input)])
        self.assertTrue(installer.external_keys_enabled(self.root))
        self.assertEqual(1, installer.load_state(self.root)['device_key_grants'])

    def test_update_with_new_key_service_rejects_old_release_before_import(self):
        previous = self.release('v2.3.0')
        (self.root / 'current').symlink_to(previous)
        before = (self.root / '.env').read_bytes()
        with patch.object(installer, 'require_system', return_value=('ubuntu', 'noble')), \
                patch.object(installer, 'check_network'), patch.object(installer, 'prompt', return_value='yes'), \
                patch.object(installer, 'compose') as compose:
            with self.assertRaisesRegex(installer.InstallError, 'устройств'):
                installer.main(['--root', str(self.root), 'update', '--version', 'v2.3.0', '--key-service-config', str(self.input)])
        self.assertEqual(before, (self.root / '.env').read_bytes())
        self.assertFalse((self.root / 'secrets/key-service.json').exists())
        compose.assert_not_called()
