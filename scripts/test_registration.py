import importlib.util
from pathlib import Path
import plistlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('service', Path(__file__).with_name('service.py'))
service = importlib.util.module_from_spec(spec)
spec.loader.exec_module(service)


class RegistrationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.state = self.base / 'state'
        self.state.mkdir()
        self.agent = self.base / 'LaunchAgents' / (service.LABEL + '.plist')
        for key, value in [('STATE', self.state), ('PLIST', self.agent)]:
            p = patch.object(service, key, value)
            p.start()
            self.addCleanup(p.stop)
        self.token = self.base / 'token'
        self.token.write_text('a' * 64)
        self.project = self.base / 'project'
        (self.project / 'bin').mkdir(parents=True)
        (self.project / 'bin/terminal-mcp').write_bytes(b'fake executable')
        self.config = {'project': str(self.project), 'listen': '127.0.0.1:8090',
                       'token_file': str(self.token), 'path': '/usr/bin:/bin'}

    def configured(self):
        service.save(self.state / 'config.json', self.config)
        service.atomic(self.agent, plistlib.dumps(service.plist(self.config)))
        return self.agent

    def test_install_already_loaded_is_idempotent(self):
        self.configured()
        with patch.object(service, 'launch') as launch:
            launch.return_value.returncode = 0
            service.install_service(self.config)
        launch.assert_called_once_with('print', service.TARGET, check=False)
        self.assertTrue(self.agent.exists())

    def test_install_does_not_override_manual_stop(self):
        self.configured()
        service.atomic(self.state / 'stopped', b'explicit stop')
        with patch.object(service, 'launch') as launch:
            service.install_service(self.config)
        launch.assert_not_called()
        self.assertTrue((self.state / 'stopped').exists())

    def test_reinstall_retains_state_and_starts_agent(self):
        service.save(self.state / 'config.json', self.config)
        prior = self.state / 'release'
        prior.mkdir()
        (prior / 'terminal-mcp').write_bytes(b'previous')
        service.activate(str(prior))
        service.atomic(self.state / 'stopped', b'explicit stop')
        (self.state / 'stderr.log').write_text('prior log')
        with patch.object(service, 'launch') as launch, \
             patch.object(service, 'preflight') as preflight, \
             patch.object(service, 'wait_ready', return_value={'server_id': 'new'}):
            launch.return_value.returncode = 1
            with patch.object(service.socket, 'socket') as sock:
                sock.return_value.__enter__.return_value.connect_ex.return_value = 1
                service.install_service(self.config)
        self.assertTrue(self.agent.exists())
        self.assertFalse((self.state / 'stopped').exists())
        self.assertEqual((self.state / 'stderr.log').read_text(), 'prior log')
        preflight.assert_called_once_with(self.config, str(prior.resolve()), full=False)
        self.assertIn(('bootstrap', service.DOMAIN, str(self.agent)),
                      [c.args for c in launch.call_args_list])

    def test_fresh_install_checks_build_before_registering(self):
        with patch.object(service, 'launch') as launch, \
             patch.object(service, 'preflight') as preflight, \
             patch.object(service, 'wait_ready', return_value={'server_id': 'new'}), \
             patch.object(service.subprocess, 'run') as run, \
             patch.object(service.socket, 'socket') as sock:
            launch.return_value.returncode = 1
            sock.return_value.__enter__.return_value.connect_ex.return_value = 1
            service.fresh_install(self.config)
        run.assert_called_once()
        self.assertTrue(self.agent.exists())
        self.assertEqual(service.load(self.state / 'config.json'), self.config)
        self.assertTrue((self.state / 'current/terminal-mcp').exists())
        self.assertEqual(preflight.call_count, 2)

    def test_fresh_install_preflight_failure_does_not_register(self):
        with patch.object(service, 'launch') as launch, \
             patch.object(service, 'preflight', side_effect=RuntimeError('bad binary')), \
             patch.object(service.subprocess, 'run'), \
             patch.object(service.socket, 'socket') as sock:
            launch.return_value.returncode = 1
            sock.return_value.__enter__.return_value.connect_ex.return_value = 1
            with self.assertRaisesRegex(RuntimeError, 'bad binary'):
                service.fresh_install(self.config)
        self.assertFalse(self.agent.exists())
        self.assertFalse((self.state / 'config.json').exists())

    def test_failed_first_bootstrap_can_be_retried(self):
        def fail_bootstrap(*args, **kwargs):
            if args[0] == 'bootstrap':
                raise RuntimeError('launchd rejected job')
            return subprocess.CompletedProcess(args, 1 if args[0] == 'print' else 0)
        with patch.object(service, 'launch', side_effect=fail_bootstrap), \
             patch.object(service, 'preflight'), \
             patch.object(service.subprocess, 'run'), \
             patch.object(service.socket, 'socket') as sock:
            sock.return_value.__enter__.return_value.connect_ex.return_value = 1
            with self.assertRaisesRegex(RuntimeError, 'launchd rejected job'):
                service.fresh_install(self.config)
        self.assertFalse(self.agent.exists())
        self.assertFalse((self.state / 'current').exists())
        self.assertFalse((self.state / 'config.json').exists())
        self.assertTrue((self.state / 'releases').exists())
        self.assertTrue(self.token.exists())

    def test_uninstall_preserves_files_and_is_idempotent(self):
        self.configured()
        retained = self.state / 'releases' / 'one'
        retained.mkdir(parents=True)
        (retained / 'terminal-mcp').write_bytes(b'binary')
        (self.state / 'stderr.log').write_text('old logs')
        loaded = True
        def launch(*args, **kwargs):
            nonlocal loaded
            if args[0] == 'bootout':
                loaded = False
            return subprocess.CompletedProcess(args, 0 if loaded or args[0] != 'print' else 1)
        with patch.object(service, 'launch', side_effect=launch):
            service.uninstall_service()
            service.uninstall_service()
        self.assertFalse(self.agent.exists())
        self.assertTrue((self.state / 'stopped').exists())
        self.assertTrue((self.state / 'config.json').exists())
        self.assertEqual((retained / 'terminal-mcp').read_bytes(), b'binary')
        self.assertEqual((self.state / 'stderr.log').read_text(), 'old logs')
        self.assertTrue(self.token.exists())

    def test_uninstall_refuses_when_update_pending(self):
        self.configured()
        (self.state / 'pending').write_text('job')
        with patch.object(service, 'launch') as launch:
            with self.assertRaisesRegex(RuntimeError, 'update pending'):
                service.uninstall_service()
        launch.assert_not_called()
        self.assertTrue(self.agent.exists())
        self.assertFalse((self.state / 'stopped').exists())

    def test_uninstall_never_deletes_plist_if_bootout_failed(self):
        self.configured()
        with patch.object(service, 'launch') as launch:
            launch.return_value.returncode = 0
            with self.assertRaisesRegex(RuntimeError, 'still loaded'):
                service.uninstall_service()
        self.assertTrue(self.agent.exists())
        self.assertTrue((self.state / 'stopped').exists())

    def test_uninstall_refuses_foreign_agent(self):
        self.configured()
        service.atomic(self.agent, plistlib.dumps({'Label': 'other.service'}))
        with patch.object(service, 'launch') as launch:
            launch.return_value.returncode = 1
            with self.assertRaisesRegex(RuntimeError, 'unrecognized'):
                service.uninstall_service()
        self.assertTrue(self.agent.exists())
        self.assertFalse((self.state / 'stopped').exists())


if __name__ == '__main__':
    unittest.main()
