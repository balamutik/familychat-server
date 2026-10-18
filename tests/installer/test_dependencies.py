import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('dependencies_installer', Path(__file__).resolve().parents[2] / 'scripts/familychat.py')
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class DependencyTests(unittest.TestCase):
    def check_install(self, provider, expected, versions):
        actions = []
        version_values = iter(versions)
        def execute(args, **kwargs):
            actions.append(args)
            if args[:3] == ['docker', 'compose', 'version']:
                value = next(version_values)
                if value is None:
                    raise installer.InstallError('docker code 125')
                return subprocess.CompletedProcess(args, 0, stdout=value)
            if args[0] == 'dpkg-query':
                return subprocess.CompletedProcess(args, 0, stdout='install ok installed' if args[-1] == provider else '')
            return subprocess.CompletedProcess(args, 0, stdout='')
        with patch.object(installer.shutil, 'which', return_value='/usr/bin/tool'), patch.object(installer, 'run', side_effect=execute), patch.object(installer, 'prompt', return_value='yes'):
            installer.ensure_docker('ubuntu', 'noble')
        self.assertIn(['apt-get', 'install', '-y', expected], actions)
        self.assertFalse(any('docker-ce' in command for command in actions))

    def test_existing_ubuntu_docker_gets_missing_compose(self):
        self.check_install('docker.io', 'docker-compose-v2', [None, '2.40.3'])

    def test_existing_official_docker_gets_compose_upgrade(self):
        self.check_install('docker-ce-cli', 'docker-compose-plugin', ['2.20.0', '2.40.3'])

    def test_installed_compose_is_rechecked(self):
        with self.assertRaisesRegex(installer.InstallError, '2.24.4'):
            self.check_install('docker.io', 'docker-compose-v2', [None, '2.20.0'])

    def test_healthy_dependencies_do_not_install_packages(self):
        with patch.object(installer.shutil, 'which', return_value='/usr/bin/tool'), patch.object(installer, 'run', return_value=subprocess.CompletedProcess([], 0, stdout='2.40.3')) as run, patch.object(installer, 'prompt') as prompt:
            installer.ensure_docker('ubuntu', 'noble')
        prompt.assert_not_called()
        self.assertFalse(any(call.args[0][0] == 'apt-get' for call in run.call_args_list))

    def test_declined_dependency_installation_does_not_change_packages(self):
        with patch.object(installer, 'prompt', return_value='no'), patch.object(installer, 'run') as run:
            with self.assertRaisesRegex(installer.InstallError, 'отменена'):
                installer.install_dependencies(['docker-compose-v2'])
        run.assert_not_called()
