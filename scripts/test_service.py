import importlib.util
from pathlib import Path
import plistlib
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('service', Path(__file__).with_name('service.py'))
service = importlib.util.module_from_spec(spec)
spec.loader.exec_module(service)


class ServiceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.state = Path(self.directory.name)
        self.patch = patch.object(service, 'STATE', self.state)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.config = {'project': str(self.state), 'listen': '127.0.0.1:8090',
                       'token_file': '/private/token', 'path': '/usr/bin:/bin'}

    def manifest(self):
        path = self.state / 'jobs/one/manifest.json'
        service.save(path, {'job_id': 'one', 'config': self.config,
            'candidate': str(self.state / 'new'), 'previous': str(self.state / 'old'),
            'server_id': 'old-server', 'adopt_pid': 0, 'fingerprint': 'new-tools',
            'previous_fingerprint': 'old-tools'})
        (self.state / 'pending').write_text('one')
        return path

    def status(self):
        return service.load(self.state / 'jobs/one/status.json')

    def test_atomic_and_release(self):
        binary = self.state / 'binary'
        service.atomic(binary, b'original', 0o700)
        old = service.release(binary)
        service.atomic(binary, b'new', 0o700)
        new = service.release(binary)
        self.assertNotEqual(old, new)
        self.assertEqual((Path(old) / 'terminal-mcp').read_bytes(), b'original')
        service.activate(old)
        self.assertTrue((self.state / 'current').samefile(old))
        service.activate(new)
        self.assertTrue((self.state / 'current').samefile(new))
        self.assertEqual(binary.stat().st_mode & 0o777, 0o700)

    def test_success_requires_refresh_when_metadata_changes(self):
        path = self.manifest()
        with patch.object(service, 'acquire_gate', return_value=True), \
             patch.object(service, 'restart') as restart, \
             patch.object(service, 'wait_ready', return_value={'server_id': 'new-server'}), \
             patch.object(service, 'metadata', return_value='new-tools'):
            service.worker(path)
        self.assertEqual(self.status()['status'], 'applied')
        self.assertTrue(self.status()['refresh_tools_required'])
        self.assertFalse((self.state / 'pending').exists())
        restart.assert_called_once()

    def test_implementation_only_update_needs_no_refresh(self):
        path = self.manifest()
        job = service.load(path)
        job['previous_fingerprint'] = 'new-tools'
        service.save(path, job)
        with patch.object(service, 'acquire_gate', return_value=True), \
             patch.object(service, 'restart'), \
             patch.object(service, 'wait_ready', return_value={'server_id': 'new-server'}), \
             patch.object(service, 'metadata', return_value='new-tools'):
            service.worker(path)
        self.assertFalse(self.status()['refresh_tools_required'])

    def test_bad_new_process_rolls_back(self):
        path = self.manifest()
        with patch.object(service, 'acquire_gate', return_value=True), \
             patch.object(service, 'restart') as restart, \
             patch.object(service, 'launch') as launch, \
             patch.object(service, 'wait_ready', side_effect=[RuntimeError('not ready'), {'server_id': 'restored'}]):
            launch.return_value.returncode = 0
            service.worker(path)
        self.assertEqual(self.status()['status'], 'rolled_back')
        self.assertEqual((self.state / 'current').readlink(), self.state / 'old')
        self.assertEqual(restart.call_count, 2)

    def test_restart_gate_requests_full_session_list(self):
        with patch.object(service, 'call', return_value={
            'server_id': 'old-server', 'sessions': [], 'counts': {
                'running': 0, 'exited_unclosed': 0, 'closed_retained': 3,
            },
        }) as call, patch.object(service, 'request', return_value={}) as request:
            self.assertTrue(service.acquire_gate(self.config, 'old-server', 0))
        call.assert_called_once_with(self.config, 'terminal_list', {'include_closed': True})
        request.assert_called_once_with(self.config, '/admin/maintenance',
                                        {'server_id': 'old-server', 'action': 'begin'})

    def test_restart_gate_handles_legacy_server_schema(self):
        def legacy_call(config, name, arguments):
            if arguments:
                raise RuntimeError('validating root: unexpected additional properties ["include_closed"]')
            return {'server_id': 'old-server', 'sessions': []}
        with patch.object(service, 'call', side_effect=legacy_call) as call, \
             patch.object(service, 'request', return_value={}):
            self.assertTrue(service.acquire_gate(self.config, 'old-server', 0))
        self.assertEqual([c.args[2] for c in call.call_args_list],
                         [{'include_closed': True}, {}])

    def test_restart_gate_does_not_mask_other_errors(self):
        with patch.object(service, 'call', side_effect=RuntimeError('authentication failed')) as call:
            with self.assertRaisesRegex(RuntimeError, 'authentication failed'):
                service.acquire_gate(self.config, 'old-server', 0)
        self.assertEqual(call.call_count, 1)

    def test_busy_gate_does_not_stop_or_switch(self):
        path = self.manifest()
        with patch.object(service, 'acquire_gate', side_effect=RuntimeError('active sessions')), \
             patch.object(service, 'activate') as activate, patch.object(service, 'restart') as restart:
            service.worker(path)
        activate.assert_not_called()
        restart.assert_not_called()
        self.assertEqual(self.status()['status'], 'failed')

    def test_manual_stop_wins_over_rollback(self):
        path = self.manifest()
        def stop(*args, **kwargs):
            (self.state / 'stopped').write_text('user')
            raise RuntimeError('user stopped')
        with patch.object(service, 'acquire_gate', return_value=True), \
             patch.object(service, 'restart', side_effect=stop) as restart, \
             patch.object(service, 'launch') as launch:
            service.worker(path)
        self.assertEqual(self.status()['status'], 'stopped')
        restart.assert_called_once()
        launch.assert_called_with('bootout', service.TARGET, check=False)

    def test_cancel_gate_when_activation_fails(self):
        path = self.manifest()
        with patch.object(service, 'acquire_gate', return_value=True), \
             patch.object(service, 'activate', side_effect=OSError('disk full')), \
             patch.object(service, 'request') as request:
            service.worker(path)
        request.assert_called_with(self.config, '/admin/maintenance', {'server_id': 'old-server', 'action': 'cancel'})
        self.assertEqual(self.status()['status'], 'failed')

    def test_metadata_fingerprint_ignores_binary_version(self):
        def rpc(version):
            return [{'serverInfo': {'version': version}, 'instructions': 'hello'},
                    {'tools': [{'name': 'x', 'description': 'do x'}]}]
        with patch.object(service, 'rpc', side_effect=rpc('1')):
            first = service.metadata(self.config)
        with patch.object(service, 'rpc', side_effect=rpc('2')):
            self.assertEqual(first, service.metadata(self.config))
        with patch.object(service, 'rpc', side_effect=[{'instructions': 'different'}, {'tools': []}]):
            self.assertNotEqual(first, service.metadata(self.config))

    def test_plist_is_supervised_and_does_not_embed_secret(self):
        value = service.plist(self.config)
        self.assertTrue(value['KeepAlive'])
        self.assertTrue(value['RunAtLoad'])
        self.assertEqual(value['WorkingDirectory'], str(self.state))
        self.assertNotIn('TERMINAL_MCP_TOKEN', value['EnvironmentVariables'])
        self.assertEqual(plistlib.loads(plistlib.dumps(value)), value)


if __name__ == '__main__':
    unittest.main()
