"""Exercise gateway installation, secret handling and managed release compatibility."""
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from test_installer import installer, ROOT

TOKEN = 'AAAAAAAAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

class PushGatewayInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'installation'
        self.state = installer.configure(self.root, 'chat.example.com', 'owner@example.com', '8.8.8.8')
        self.release = self.root / 'releases/v2.5.0'; self.release.mkdir(parents=True)
        (self.release / 'release.json').write_text(json.dumps(dict(version='v2.5.0', push_gateway=1)))
        (self.release / 'compose.push-gateway.yaml').write_bytes((ROOT / 'compose.push-gateway.yaml').read_bytes()) if (ROOT / 'compose.push-gateway.yaml').exists() else None
        (self.root / 'current').symlink_to(self.release)
        self.source = Path(self.temp.name) / 'family.token'; self.source.write_text(TOKEN+'\n'); self.source.chmod(0o600)

    def configure(self, **kwargs):
        return installer.configure_push_gateway(self.root, self.release, self.state,
            kwargs.get('url','https://push.example.com:9443'), self.source, restart=kwargs.get('restart',True))

    def test_configuration_private_preserves_credentials_and_restart(self):
        before=(self.root/'.env').read_text()
        with patch.object(installer,'probe_push_gateway') as probe, patch.object(installer,'compose') as compose, patch('sys.stdout',new_callable=io.StringIO) as out:
            self.configure()
        env=(self.root/'.env').read_text()
        for line in before.splitlines(): self.assertIn(line+'\n',env)
        self.assertIn('PUSH_GATEWAY_URL=https://push.example.com:9443\n',env)
        self.assertNotIn(TOKEN,env+out.getvalue())
        files=list((self.root/'secrets').glob('push-gateway-*.token'));self.assertEqual(1,len(files))
        self.assertEqual(TOKEN,files[0].read_text().strip());self.assertEqual(0o400,files[0].stat().st_mode&0o777)
        probe.assert_called_once_with('https://push.example.com:9443',TOKEN)
        self.assertEqual(('config','-q'),compose.call_args_list[0].args[3:])
        self.assertIn('--force-recreate',compose.call_args_list[1].args)
        with patch.object(installer,'run') as run:
            installer.compose(self.root,self.release,self.state,'config','-q')
        self.assertIn(self.release/'compose.push-gateway.yaml',run.call_args.args[0])

    def test_invalid_address_and_credentials_do_not_change_config(self):
        before=(self.root/'.env').read_bytes()
        for url in ['http://push.example.com','https://u:p@push.example.com','https://push.example.com/x','https://push.example.com?token=x','https://push.example.com:0','https://push.example.com\nEVIL=yes','https://push.example.com/$TOKEN']:
            with self.subTest(url=url), patch.object(installer,'probe_push_gateway') as probe, self.assertRaises(installer.InstallError): self.configure(url=url)
            self.assertEqual(before,(self.root/'.env').read_bytes());probe.assert_not_called()
        self.source.write_text('bad\nPOSTGRES_PASSWORD=bad')
        with self.assertRaises(installer.InstallError): self.configure()
        self.assertEqual(before,(self.root/'.env').read_bytes())

    def test_probe_and_compose_failure_preserve_previous_configuration(self):
        before=(self.root/'.env').read_bytes()
        with patch.object(installer,'probe_push_gateway',side_effect=installer.InstallError('unreachable')), self.assertRaises(installer.InstallError): self.configure()
        self.assertEqual(before,(self.root/'.env').read_bytes())
        with patch.object(installer,'probe_push_gateway'), patch.object(installer,'compose',side_effect=installer.InstallError('invalid compose')), self.assertRaises(installer.InstallError): self.configure()
        self.assertEqual(before,(self.root/'.env').read_bytes())
        self.assertEqual([],list((self.root/'secrets').glob('push-gateway-*.token')))

    def test_existing_provider_keys_disabled_but_files_preserved(self):
        key=self.root/'secrets/apple.p8';key.write_text('existing')
        with (self.root/'.env').open('a') as out: out.write('APNS_KEY_FILE=./secrets/apple.p8\nAPNS_KEY_ID=TEST\nAPNS_TEAM_ID=TEAM\nAPNS_TOPIC=com.test\nFCM_CREDENTIALS_FILE=./secrets/google.json\nFCM_PROJECT_ID=project\nFCM_APPLICATION_ID=com.test\n')
        with patch.object(installer,'probe_push_gateway'),patch.object(installer,'compose'):self.configure()
        env=(self.root/'.env').read_text()
        for name in ['APNS_KEY_FILE','APNS_KEY_ID','APNS_TEAM_ID','APNS_TOPIC','FCM_CREDENTIALS_FILE','FCM_PROJECT_ID','FCM_APPLICATION_ID']:self.assertNotIn(name+'=',env)
        self.assertEqual('existing',key.read_text())

    def test_incompatible_release_rejected_before_network_and_changes(self):
        (self.release/'release.json').write_text('{}');before=(self.root/'.env').read_bytes()
        with patch.object(installer,'probe_push_gateway') as probe,self.assertRaises(installer.InstallError):self.configure()
        probe.assert_not_called();self.assertEqual(before,(self.root/'.env').read_bytes())

    def test_gateway_configuration_blocks_downgrade(self):
        with patch.object(installer,'probe_push_gateway'),patch.object(installer,'compose'):self.configure()
        old=self.root/'releases/v2.0.0';old.mkdir();(old/'release.json').write_text('{}')
        with patch.object(installer,'compose') as compose,self.assertRaises(installer.InstallError):installer.activate(self.root,old,self.state)
        compose.assert_not_called()

    def test_configure_command_and_interactive_gateway(self):
        with patch.object(installer,'require_system',return_value=('ubuntu','noble')),patch.object(installer,'probe_push_gateway'),patch.object(installer,'compose'):
            installer.main(['--root',str(self.root),'configure-push','--push-gateway-url','https://push.example.com','--push-gateway-token-file',str(self.source)])
            with patch.object(installer,'prompt',side_effect=['gateway','https://second.example.com',str(self.source)]):
                installer.main(['--root',str(self.root),'configure-push'])
        self.assertIn('PUSH_GATEWAY_URL=https://second.example.com\n',(self.root/'.env').read_text())

    def test_install_arguments_apply_before_activation_without_restart(self):
        (self.root/'current').unlink()
        def activate(root,release,state): self.assertTrue(installer.push_gateway_enabled(root))
        with patch.object(installer,'require_system',return_value=('ubuntu','noble')),patch.object(installer,'ensure_docker'),patch.object(installer,'check_network'),patch.object(installer,'probe_push_gateway'),patch.object(installer,'compose') as compose,patch.object(installer,'activate',side_effect=activate),patch.object(installer,'bootstrap_admin',return_value=False),patch.object(installer,'install_commands'):
            installer.main(['--root',str(self.root),'install','--version','v2.5.0','--push-gateway-url','https://push.example.com','--push-gateway-token-file',str(self.source)])
        self.assertFalse(any('--force-recreate' in call.args for call in compose.call_args_list))
