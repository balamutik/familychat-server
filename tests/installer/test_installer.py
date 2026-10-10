import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('installer', ROOT / 'scripts/familychat.py')
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'state'

    def test_configuration_survives_rerun_without_rotating_secrets(self):
        first = installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        original = (self.root / '.env').read_bytes()
        password = (self.root / 'secrets/admin-password.txt').read_bytes()
        second = installer.configure(self.root, 'changed.example.com', 'other@example.com', '1.1.1.1')
        self.assertEqual(first, second)
        self.assertEqual(original, (self.root / '.env').read_bytes())
        self.assertEqual(password, (self.root / 'secrets/admin-password.txt').read_bytes())
        self.assertEqual(0o600, (self.root / '.env').stat().st_mode & 0o777)
        self.assertEqual(0o700, self.root.stat().st_mode & 0o777)
        self.assertIn(b'TURN_URL=turn:8.8.8.8:3478?transport=udp', original)

    def test_existing_unmanaged_installation_is_not_overwritten(self):
        self.root.mkdir()
        (self.root / '.env').write_text('valuable old config')
        with self.assertRaisesRegex(installer.InstallError, 'существующ'):
            installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        self.assertEqual('valuable old config', (self.root / '.env').read_text())

    def test_incomplete_saved_installation_does_not_regenerate_secrets(self):
        installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        (self.root / '.env').unlink()
        with self.assertRaises(installer.InstallError):
            installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        self.assertFalse((self.root / '.env').exists())

    def test_invalid_input_cannot_enter_configuration(self):
        for domain, email, ip in [('chat.example.com\nX=bad','a@b.com','8.8.8.8'),
                                  ('localhost','a@b.com','8.8.8.8'),
                                  ('chat.example.com','x\nX=bad','8.8.8.8'),
                                  ('chat.example.com','a@b.com','127.0.0.1')]:
            with self.subTest(domain=domain, email=email, ip=ip):
                with self.assertRaises(installer.InstallError):
                    installer.configure(self.root, domain, email, ip)
                self.assertFalse((self.root / '.env').exists())

    def test_archive_rejects_traversal_and_links(self):
        for name, link in [('../escape', False), ('/tmp/escape', False), ('compose.yaml', True)]:
            data = io.BytesIO()
            with tarfile.open(fileobj=data, mode='w:gz') as archive:
                entry = tarfile.TarInfo(name)
                if link:
                    entry.type = tarfile.SYMTYPE
                    entry.linkname = '/etc/passwd'
                archive.addfile(entry)
            with self.assertRaises(installer.InstallError):
                installer.extract_bundle(data.getvalue(), self.root)

    def test_download_checksum_is_enforced(self):
        with patch.object(installer, 'download', side_effect=[b'bad archive', b'0'*64+b'  familychat-server.tar.gz\n']):
            with self.assertRaisesRegex(installer.InstallError, 'SHA256'):
                installer.fetch_bundle('balamutik/familychat-releases', 'v1.0.0', self.root)
        self.assertFalse(self.root.exists())

    def test_failed_pull_keeps_current_release_and_secrets(self):
        config = installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        previous = self.root / 'releases/v1.0.0'
        candidate = self.root / 'releases/v1.0.1'
        previous.mkdir(parents=True)
        candidate.mkdir()
        (self.root / 'current').symlink_to(previous)
        before = (self.root / '.env').read_bytes()
        with patch.object(installer, 'compose', side_effect=installer.InstallError('pull failed')):
            with self.assertRaisesRegex(installer.InstallError, 'pull failed'):
                installer.activate(self.root, candidate, config)
        self.assertEqual(previous.resolve(), (self.root / 'current').resolve())
        self.assertEqual(before, (self.root / '.env').read_bytes())

    def test_encrypted_installation_cannot_downgrade_to_plaintext_release(self):
        state = installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        previous = self.root / 'releases/v2.0.0'
        candidate = self.root / 'releases/v1.0.3'
        previous.mkdir(parents=True)
        candidate.mkdir()
        (previous / 'release.json').write_text(json.dumps({'content_encryption': 1}))
        (candidate / 'release.json').write_text(json.dumps({'version': 'v1.0.3'}))
        (self.root / 'current').symlink_to(previous)
        with patch.object(installer, 'compose') as compose:
            with self.assertRaises(installer.InstallError):
                installer.activate(self.root, candidate, state)
        compose.assert_not_called()

    def test_interrupted_encryption_activation_prevents_legacy_downgrade(self):
        state = installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        previous = self.root / 'releases/v1.0.3'
        candidate = self.root / 'releases/v2.0.0'
        previous.mkdir(parents=True)
        candidate.mkdir()
        (previous / 'release.json').write_text(json.dumps({'version': 'v1.0.3'}))
        (candidate / 'release.json').write_text(json.dumps({'content_encryption': 1}))
        (self.root / 'current').symlink_to(previous)
        with patch.object(installer, 'compose'), patch.object(installer, 'run', side_effect=installer.InstallError('interrupted after migration')):
            with self.assertRaises(installer.InstallError):
                installer.activate(self.root, candidate, state)
        self.assertEqual(previous.resolve(), (self.root / 'current').resolve())
        persisted = installer.load_state(self.root)
        with patch.object(installer, 'compose') as compose:
            with self.assertRaises(installer.InstallError):
                installer.activate(self.root, previous, persisted)
        compose.assert_not_called()

    def test_legacy_launcher_is_replaced_before_encryption_migration(self):
        state = installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        previous = self.root / 'releases/v1.0.3'
        candidate = self.root / 'releases/v2.0.0'
        (previous / 'scripts').mkdir(parents=True)
        (candidate / 'scripts').mkdir(parents=True)
        (previous / 'release.json').write_text(json.dumps({'version': 'v1.0.3'}))
        (candidate / 'release.json').write_text(json.dumps({'content_encryption': 1}))
        (candidate / 'scripts/familychat.py').write_bytes((ROOT / 'scripts/familychat.py').read_bytes())
        (self.root / 'current').symlink_to(previous)
        installer.link_state(self.root, candidate)
        launcher = self.root / 'familychat-command'
        launcher.write_text('legacy controller')
        with patch.object(installer, 'COMMAND_PATH', launcher):
            installer.prepare_managed_upgrade(candidate)
        self.assertIn(str(self.root / 'manager.py'), launcher.read_text())
        self.assertEqual(previous.resolve(), (self.root / 'current').resolve())
        spec = importlib.util.spec_from_file_location('upgraded_installer', self.root / 'manager.py')
        upgraded = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(upgraded)
        persisted = upgraded.load_state(self.root)
        self.assertEqual(1, persisted['content_encryption'])
        self.assertEqual(candidate.name, persisted['pending_version'])
        with patch.object(upgraded, 'compose') as compose:
            with self.assertRaises(upgraded.InstallError):
                upgraded.activate(self.root, previous, persisted)
        compose.assert_not_called()

    def test_existing_admin_after_interruption_is_not_recreated(self):
        state = installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        with patch.object(installer, 'compose', return_value=subprocess.CompletedProcess([], 0, '1\n', '')) as command:
            self.assertFalse(installer.bootstrap_admin(self.root, self.root / 'current', state))
        self.assertEqual(1, command.call_count)
        self.assertTrue(installer.load_state(self.root)['admin_created'])

    def test_failed_activation_records_release_to_resume(self):
        state = installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        candidate = self.root / 'releases/v1.0.0'
        candidate.mkdir(parents=True)
        with patch.object(installer, 'compose'), patch.object(installer, 'run', side_effect=installer.InstallError('certificate failure')):
            with self.assertRaises(installer.InstallError):
                installer.activate(self.root, candidate, state)
        self.assertEqual('v1.0.0', installer.load_state(self.root)['pending_version'])
        self.assertFalse((self.root / 'current').exists())

    def test_successful_activation_switches_release_and_clears_pending(self):
        state = installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        candidate = self.root / 'releases/v1.0.1'
        candidate.mkdir(parents=True)
        with patch.object(installer, 'compose') as compose, patch.object(installer, 'run') as run:
            installer.activate(self.root, candidate, state)
        self.assertEqual(candidate.resolve(), (self.root / 'current').resolve())
        self.assertNotIn('pending_version', installer.load_state(self.root))
        self.assertEqual(['config', 'pull', 'stop', 'up'], [call.args[3] for call in compose.call_args_list])
        self.assertIn('--no-build', run.call_args.args[0])

    def test_network_rejects_wrong_dns_and_occupied_ports_before_config(self):
        def lookup(host, port, family):
            if family == installer.socket.AF_INET6:
                raise installer.socket.gaierror()
            return [(family, 1, 6, '', ('1.1.1.1', 0))]
        with patch.object(installer.socket, 'getaddrinfo', side_effect=lookup):
            with self.assertRaisesRegex(installer.InstallError, 'DNS'):
                installer.check_network('chat.example.com', '8.8.8.8', fresh=True)
        with patch.object(installer.socket, 'getaddrinfo', side_effect=lookup), patch.object(installer.socket, 'socket') as sock:
            sock.return_value.__enter__.return_value.bind.side_effect = OSError('busy')
            with self.assertRaisesRegex(installer.InstallError, '80.*занят'):
                installer.check_network('chat.example.com', '1.1.1.1', fresh=True)
        self.assertFalse((self.root / '.env').exists())

    def test_compose_ignores_unrelated_shell_configuration(self):
        state = installer.configure(self.root, 'chat.example.com', 'admin@example.com', '8.8.8.8')
        with patch.dict(os.environ, {'COMPOSE_PROJECT_NAME': 'another', 'POSTGRES_PASSWORD': 'wrong', 'APNS_KEY_FILE': '/bad'}):
            env = installer.environment(self.root, state)
        self.assertEqual('familychat', env['COMPOSE_PROJECT_NAME'])
        self.assertNotIn('POSTGRES_PASSWORD', env)
        self.assertNotIn('APNS_KEY_FILE', env)

    def test_cli_install_rerun_and_update_keep_credentials_and_project(self):
        versions = []
        def fetch(repository, version, destination):
            versions.append(version)
            destination.mkdir(parents=True)
            (destination / 'release.json').write_text(json.dumps({'version': version}))
        def command(*args, **kwargs):
            if 'psql' in args:
                return subprocess.CompletedProcess([], 0, '0\n', '')
            return subprocess.CompletedProcess([], 0, '', '')
        answers = iter(['chat.example.com', 'admin@example.com', 'yes'])
        with patch('sys.stdout', new_callable=io.StringIO), \
             patch.object(installer, 'require_system', return_value=('ubuntu','noble')), \
             patch.object(installer, 'ensure_docker'), \
             patch.object(installer, 'check_network'), \
             patch.object(installer, 'install_commands'), \
             patch.object(installer, 'download', return_value=b'8.8.8.8'), \
             patch.object(installer, 'prompt', side_effect=lambda _: next(answers)), \
             patch.object(installer, 'fetch_bundle', side_effect=fetch), \
             patch.object(installer, 'run', return_value=subprocess.CompletedProcess([],0,'','')), \
             patch.object(installer, 'compose', side_effect=command) as compose:
            installer.main(['--root', str(self.root), 'install', '--version', 'v1.0.0'])
            original = (self.root / '.env').read_bytes()
            password = (self.root / 'secrets/admin-password.txt').read_bytes()
            installer.main(['--root', str(self.root), 'install', '--version', 'v1.0.1'])
            self.assertEqual('v1.0.0', (self.root / 'current').resolve().name)
            installer.main(['--root', str(self.root), 'update', '--version', 'v1.0.1'])
            self.assertEqual(1, sum('bootstrap-admin' in call.args for call in compose.call_args_list))
        self.assertEqual(['v1.0.0', 'v1.0.1'], versions)
        self.assertEqual('v1.0.1', (self.root / 'current').resolve().name)
        self.assertEqual(original, (self.root / '.env').read_bytes())
        self.assertEqual(password, (self.root / 'secrets/admin-password.txt').read_bytes())
        self.assertEqual('familychat', installer.load_state(self.root)['project'])

    def test_release_bundle_uses_one_pinned_image_and_no_source_or_secrets(self):
        output = Path(self.temp.name) / 'bundle'
        subprocess.run(['python3', str(ROOT/'scripts/package-release.py'), '--version','v1.0.0',
                        '--image', 'ghcr.io/balamutik/familychat-server@sha256:'+'a'*64,
                        '--output',str(output)],check=True)
        data = (output/'familychat-server.tar.gz').read_bytes()
        self.assertIn(hashlib.sha256(data).hexdigest(), (output/'SHA256SUMS').read_text())
        installer.extract_bundle(data, self.root)
        manifest = json.loads((self.root/'release.json').read_text())
        self.assertEqual('v1.0.0', manifest['version'])
        compose = (self.root/'compose.yaml').read_text()
        self.assertNotIn('build:', compose)
        self.assertEqual(3, compose.count('image: ghcr.io/balamutik/familychat-server@sha256:'))
        self.assertFalse((self.root/'.env').exists())
        self.assertFalse((self.root/'internal').exists())


if __name__ == '__main__':
    unittest.main()
