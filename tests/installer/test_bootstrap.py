import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class BootstrapTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.log = self.root / 'download.json'
        curl = self.root / 'curl'
        curl.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
output = args[args.index('--output') + 1]
pathlib.Path(os.environ['FAMILYCHAT_TEST_DOWNLOAD_LOG']).write_text(json.dumps(dict(args=args, output=output)))
if os.environ.get('FAMILYCHAT_TEST_DOWNLOAD_FAIL'):
    sys.exit(22)
pathlib.Path(output).write_text('#!/bin/bash\\nprintf "<%s>\\\\n" "$@"\\n')
''')
        curl.chmod(0o755)
        self.env = os.environ | {'PATH': str(self.root) + os.pathsep + os.environ['PATH'],
                                'FAMILYCHAT_TEST_DOWNLOAD_LOG': str(self.log)}

    def test_keyserver_installer_is_delegated_with_arguments_preserved(self):
        result = subprocess.run(['bash', str(ROOT / 'install.sh'), '--key-server',
                                 '--domain', 'keys.example.com', '--import-json', '/root/key backup.json'],
                                env=self.env, input='', text=True, capture_output=True)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual('<--domain>\n<keys.example.com>\n<--import-json>\n</root/key backup.json>\n', result.stdout)
        downloaded = json.loads(self.log.read_text())
        self.assertIn('https://github.com/balamutik/familychat-keyserver/releases/latest/download/install.sh', downloaded['args'])
        self.assertIn('=https', downloaded['args'])
        self.assertFalse(Path(downloaded['output']).exists())

    def test_keyserver_download_failure_does_not_run_or_leave_script(self):
        result = subprocess.run(['bash', str(ROOT / 'install.sh'), '--key-server'],
                                env=self.env | {'FAMILYCHAT_TEST_DOWNLOAD_FAIL': '1'},
                                input='', text=True, capture_output=True)
        self.assertNotEqual(0, result.returncode)
        self.assertEqual('', result.stdout)
        self.assertFalse(Path(json.loads(self.log.read_text())['output']).exists())


if __name__ == '__main__':
    unittest.main()
