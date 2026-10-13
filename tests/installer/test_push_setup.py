"""Configure APNs with a real temporary P-256 key and mocked container boundary."""
import io
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from test_installer import installer


class PushSetupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'install'
        installer.configure(self.root, 'chat.example.com', 'a@example.com', '8.8.8.8')
        release = self.root / 'releases/v2.0.1'
        release.mkdir(parents=True)
        (release / 'release.json').write_text(json.dumps({'version': 'v2.0.1'}))
        (self.root / 'current').symlink_to(release)
        self.key = Path(self.temp.name) / 'AuthKey_Test.p8'
        subprocess.run(['openssl', 'genpkey', '-algorithm', 'EC', '-pkeyopt',
                        'ec_paramgen_curve:P-256', '-out', str(self.key)], check=True, capture_output=True)

    def configure(self, key_id='A123456789', mode='development', compose_error=None):
        with patch.object(installer, 'require_system', return_value=('ubuntu', 'noble')), \
             patch.object(installer, 'prompt', side_effect=[str(self.key), key_id, 'Q67GTH2DSB',
                          'com.familychat.FamilyChatSelfhosted', mode]), \
             patch.object(installer, 'compose', side_effect=compose_error) as compose, \
             patch('sys.stdout', new_callable=io.StringIO) as output:
            installer.main(['--root', str(self.root), 'configure-push'])
            return compose.call_args_list, output.getvalue()

    def test_interactive_setup_preserves_database_credentials_and_installs_key(self):
        original = (self.root / '.env').read_text()
        calls, output = self.configure()
        configured = (self.root / '.env').read_text()
        for line in original.splitlines():
            if not line.startswith('APNS_'):
                self.assertIn(line + '\n', configured)
        self.assertIn('APNS_TEAM_ID=Q67GTH2DSB\n', configured)
        self.assertIn('APNS_SANDBOX=true\n', configured)
        key_files = list((self.root / 'secrets').glob('apns-*.p8'))
        self.assertEqual(len(key_files), 1)
        self.assertEqual(key_files[0].read_bytes(), self.key.read_bytes())
        self.assertEqual(self.root.stat().st_mode & 0o777, 0o700)
        self.assertEqual((self.root / 'secrets').stat().st_mode & 0o777, 0o700)
        self.assertNotIn('BEGIN PRIVATE KEY', output)
        self.assertEqual(calls[0].args[3:], ('config', '-q'))
        self.assertIn('--force-recreate', calls[1].args)
        self.assertEqual(calls[1].args[-2:], ('api', 'worker'))
        self.assertEqual(calls[2].args[3:], ('exec', '-T', 'nginx', 'nginx', '-s', 'reload'))
        self.configure(mode='production')
        self.assertIn('APNS_SANDBOX=false\n', (self.root / '.env').read_text())
        self.assertEqual(len(list((self.root / 'secrets').glob('apns-*.p8'))), 1)

    def test_invalid_identifiers_cannot_inject_environment(self):
        before = (self.root / '.env').read_bytes()
        with self.assertRaises(installer.InstallError):
            self.configure(key_id='A123456789\nPOSTGRES_PASSWORD=bad')
        self.assertEqual((self.root / '.env').read_bytes(), before)
        self.assertEqual(list((self.root / 'secrets').glob('apns-*.p8')), [])

    def test_wrong_curve_is_rejected_before_writing_config(self):
        subprocess.run(['openssl', 'genpkey', '-algorithm', 'EC', '-pkeyopt',
                        'ec_paramgen_curve:P-384', '-out', str(self.key)], check=True, capture_output=True)
        before = (self.root / '.env').read_bytes()
        with self.assertRaises(installer.InstallError):
            self.configure()
        self.assertEqual((self.root / '.env').read_bytes(), before)

    def test_invalid_compose_config_restores_previous_settings(self):
        before = (self.root / '.env').read_bytes()
        with self.assertRaises(installer.InstallError):
            self.configure(compose_error=installer.InstallError('bad compose'))
        self.assertEqual((self.root / '.env').read_bytes(), before)
