#!/usr/bin/env python3
"""User launchd service and independently supervised, rollback-capable updates."""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import plistlib
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid

LABEL = 'com.dimchansky.terminal-mcp'
STATE = Path.home() / 'Library/Application Support/LocalTerminalMCP'
PLIST = Path.home() / 'Library/LaunchAgents' / (LABEL + '.plist')
DOMAIN = 'gui/' + str(os.getuid())
TARGET = DOMAIN + '/' + LABEL


def atomic(path, data, mode=0o600):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix='.update-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
            os.fchmod(stream.fileno(), mode)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def save(path, value):
    atomic(path, (json.dumps(value, indent=2) + '\n').encode())


def load(path):
    return json.loads(Path(path).read_text())


def launch(*args, check=True):
    return subprocess.run(['/bin/launchctl', *args], check=check,
        capture_output=True, text=True, timeout=10)


def request(config, path, payload):
    token = Path(config['token_file']).read_text().strip()
    req = urllib.request.Request('http://' + config['listen'] + path,
        data=json.dumps(payload).encode(), method='POST', headers={
            'Authorization': 'Bearer ' + token,
            'Content-Type': 'application/json',
            'Accept': 'application/json, text/event-stream'})
    # Proxies or redirects must not receive the local bearer token.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *args, **kwargs):
            return None
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(req, timeout=3) as response:
        return json.loads(response.read(1 << 20))


def rpc(config, method, params):
    out = request(config, '/mcp', {'jsonrpc': '2.0', 'id': 1, 'method': method, 'params': params})
    if 'error' in out:
        raise RuntimeError('MCP error: ' + str(out['error']))
    return out['result']


def call(config, name, args):
    out = rpc(config, 'tools/call', {'name': name, 'arguments': args})
    if out.get('isError'):
        raise RuntimeError('MCP tool failed: ' + str(out.get('content')))
    if 'structuredContent' in out:
        return out['structuredContent']
    return json.loads(next(item['text'] for item in out['content'] if item['type'] == 'text'))


def metadata(config):
    initial = rpc(config, 'initialize', {'protocolVersion': '2025-11-25',
        'capabilities': {}, 'clientInfo': {'name': 'terminal-mcp-service', 'version': '1'}})
    tools = sorted(rpc(config, 'tools/list', {})['tools'], key=lambda tool: tool['name'])
    payload = {'instructions': initial.get('instructions'), 'tools': tools}
    return hashlib.sha256(json.dumps(payload, sort_keys=True).encode()).hexdigest()


def stopped():
    return (STATE / 'stopped').exists()


def wait_ready(config, previous=None, seconds=20, respect_stop=True):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if respect_stop and stopped():
            raise RuntimeError('manually stopped')
        try:
            state = call(config, 'terminal_list', {})
            if state['server_id'] != previous and not state.get('draining', False):
                return state
        except (OSError, ValueError, KeyError, RuntimeError):
            pass
        time.sleep(0.25)
    raise RuntimeError('MCP readiness deadline exceeded')


def release(binary):
    digest = hashlib.sha256(Path(binary).read_bytes()).hexdigest()
    destination = STATE / 'releases' / digest / 'terminal-mcp'
    if not destination.exists():
        atomic(destination, Path(binary).read_bytes(), 0o700)
    if hashlib.sha256(destination.read_bytes()).hexdigest() != digest:
        raise RuntimeError('release hash mismatch')
    return str(destination.parent)


def activate(directory):
    temporary = STATE / ('current-' + uuid.uuid4().hex)
    try:
        temporary.symlink_to(directory, target_is_directory=True)
        os.replace(temporary, STATE / 'current')
    finally:
        temporary.unlink(missing_ok=True)


def plist(config):
    return {'Label': LABEL, 'ProgramArguments': [str(STATE / 'current/terminal-mcp'),
        '--listen=' + config['listen'], '--token-file=' + config['token_file']],
        'WorkingDirectory': config['project'], 'RunAtLoad': True, 'KeepAlive': True,
        'ThrottleInterval': 2, 'ExitTimeOut': 15, 'Umask': 0o077,
        'EnvironmentVariables': {'PATH': config['path'], 'HOME': str(Path.home())},
        'StandardOutPath': str(STATE / 'stdout.log'),
        'StandardErrorPath': str(STATE / 'stderr.log')}


def preflight(config, candidate, full=True):
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    probe = dict(config, listen='127.0.0.1:' + str(port))
    with tempfile.TemporaryFile() as logs:
        process = subprocess.Popen([str(Path(candidate) / 'terminal-mcp'),
            '--listen=' + probe['listen'], '--token-file=' + config['token_file']],
            cwd=config['project'], stdin=subprocess.DEVNULL, stdout=logs, stderr=logs,
            start_new_session=True, env={'PATH': config['path'], 'HOME': str(Path.home())})
        try:
            state = wait_ready(probe, seconds=10, respect_stop=False)
            fingerprint = metadata(probe)
            if not full:
                return fingerprint
            result = call(probe, 'terminal_start', {'server_id': state['server_id'],
                'operation_id': 'service-preflight', 'argv': ['/usr/bin/true'],
                'close_on_exit': True, 'wait_for': 'completion', 'wait_ms': 1000})
            if result['receipt']['status'] != 'applied':
                raise RuntimeError('candidate cannot launch a process')
            observation = result['observation']
            if not (observation['session'].get('closed') and observation['session'].get('exit', {}).get('code') == 0):
                raise RuntimeError('candidate did not complete and clean up the smoke command')
            call(probe, 'file_list', {'path': config['project'], 'limit': 1})
            return fingerprint
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


def current_pid(config):
    port = config['listen'].rsplit(':', 1)[1]
    result = subprocess.run(['/usr/sbin/lsof', '-t', '-nP', '-iTCP:' + port, '-sTCP:LISTEN'],
        capture_output=True, text=True, check=False, timeout=5)
    values = set(result.stdout.split())
    if len(values) != 1:
        raise RuntimeError('expected exactly one existing listener')
    return int(values.pop())


def acquire_gate(config, expected, adopt_pid):
    end = time.monotonic() + 45
    while time.monotonic() < end:
        if stopped():
            raise RuntimeError('manually stopped')
        state = call(config, 'terminal_list', {})
        if state['server_id'] != expected:
            raise RuntimeError('server changed since update preparation; refusing stale restart')
        active = [s for s in state['sessions'] if s['process_state'] != 'exited' or not s['output_done']]
        if not active:
            try:
                request(config, '/admin/maintenance', {'server_id': expected, 'action': 'begin'})
                return True
            except urllib.error.HTTPError as exc:
                if exc.code == 404 and adopt_pid and current_pid(config) == adopt_pid:
                    # shortcut: legacy adoption has no mutation fence; only use once while idle.
                    return False
                if exc.code != 409:
                    raise
        time.sleep(0.25)
    raise RuntimeError('update blocked: active sessions or mutations; nothing was stopped')


def restart(config, adopt_pid=0):
    if stopped():
        raise RuntimeError('manually stopped')
    if adopt_pid:
        if current_pid(config) != adopt_pid:
            raise RuntimeError('listener PID changed; refusing adoption')
        os.kill(adopt_pid, signal.SIGTERM)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                os.kill(adopt_pid, 0)
            except ProcessLookupError:
                break
            time.sleep(0.1)
        else:
            raise RuntimeError('legacy server did not stop; refusing forced termination')
        if stopped():
            raise RuntimeError('manually stopped')
        launch('enable', TARGET)
        launch('bootstrap', DOMAIN, str(PLIST))
    else:
        launch('kickstart', '-k', TARGET)


def worker(manifest):
    job = load(manifest)
    config = job['config']
    status = Path(manifest).parent / 'status.json'
    switched, gated = False, False
    try:
        gated = acquire_gate(config, job['server_id'], job['adopt_pid'])
        save(status, {'status': 'activating', 'job_id': job['job_id']})
        activate(job['candidate'])
        switched = True
        restart(config, job['adopt_pid'])
        state = wait_ready(config, job['server_id'])
        found = metadata(config)
        if found != job['fingerprint']:
            raise RuntimeError('running tool metadata differs from the preflighted candidate')
        save(status, {'status': 'applied', 'job_id': job['job_id'], 'server_id': state['server_id'],
            'refresh_tools_required': found != job['previous_fingerprint']})
    except Exception as exc:
        result = {'status': 'failed', 'job_id': job['job_id'], 'error': str(exc)}
        if switched and not stopped():
            try:
                activate(job['previous'])
                if launch('print', TARGET, check=False).returncode != 0:
                    launch('bootstrap', DOMAIN, str(PLIST))
                else:
                    restart(config)
                state = wait_ready(config, job['server_id'])
                result.update(status='rolled_back', server_id=state['server_id'])
            except Exception as rollback:
                result['rollback_error'] = str(rollback)
        elif gated and not stopped():
            with contextlib.suppress(Exception):
                request(config, '/admin/maintenance', {'server_id': job['server_id'], 'action': 'cancel'})
        if stopped():
            launch('bootout', TARGET, check=False)
            result['status'] = 'stopped'
        save(status, result)
    finally:
        (STATE / 'pending').unlink(missing_ok=True)


def queue(config, adopt_pid, previous_binary=None):
    if stopped():
        raise RuntimeError('service is manually stopped; use start explicitly')
    pending = STATE / 'pending'
    if pending.exists():
        raise RuntimeError('an update is already pending; inspect status before retrying')
    before = call(config, 'terminal_list', {})
    previous_fingerprint = metadata(config)
    if adopt_pid:
        if current_pid(config) != adopt_pid:
            raise RuntimeError('adopt-pid does not match the listener')
        if not previous_binary:
            raise RuntimeError('adoption requires --previous-binary pointing to the working binary saved before rebuild')
        previous = release(previous_binary)
        if preflight(config, previous, full=False) != previous_fingerprint:
            raise RuntimeError('previous-binary metadata differs from the running server; rollback is not verified')
    else:
        previous = str((STATE / 'current').resolve(strict=True))
        if launch('print', TARGET, check=False).returncode != 0:
            raise RuntimeError('managed service not loaded; use start, not update')
    env = dict(os.environ, PATH=config['path'])
    subprocess.run(['./scripts/check.sh'], cwd=config['project'], env=env, check=True)
    candidate = release(Path(config['project']) / 'bin/terminal-mcp')
    fingerprint = preflight(config, candidate)
    if adopt_pid:
        activate(previous)
        atomic(PLIST, plistlib.dumps(plist(config)))
    job_id = uuid.uuid4().hex
    directory = STATE / 'jobs' / job_id
    directory.mkdir(parents=True, mode=0o700)
    script = directory / 'worker.py'
    atomic(script, Path(__file__).read_bytes())
    manifest = directory / 'manifest.json'
    save(manifest, dict(job_id=job_id, config=config, candidate=candidate, previous=previous,
        server_id=before['server_id'], adopt_pid=adopt_pid, fingerprint=fingerprint,
        previous_fingerprint=previous_fingerprint))
    job_plist = directory / 'job.plist'
    atomic(job_plist, plistlib.dumps({'Label': LABEL + '.update.' + job_id,
        'ProgramArguments': [sys.executable, str(script), '_worker', str(manifest)],
        'RunAtLoad': True, 'WorkingDirectory': config['project'],
        'StandardOutPath': str(directory / 'worker.log'),
        'StandardErrorPath': str(directory / 'worker.log')}))
    save(directory / 'status.json', {'status': 'queued', 'job_id': job_id})
    save(STATE / 'latest.json', {'job_id': job_id})
    atomic(pending, job_id.encode())
    try:
        launch('bootstrap', DOMAIN, str(job_plist))
    except Exception:
        pending.unlink(missing_ok=True)
        raise
    print(json.dumps({'status': 'queued', 'job_id': job_id,
        'status_file': str(directory / 'status.json'),
        'refresh_tools_required': fingerprint != previous_fingerprint}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['install', 'update', 'status', 'start', 'stop', '_worker'])
    parser.add_argument('manifest', nargs='?')
    parser.add_argument('--adopt-pid', type=int, default=0)
    parser.add_argument('--previous-binary', type=Path)
    args = parser.parse_args()
    if sys.platform != 'darwin':
        parser.error('this service controller requires macOS launchd')
    STATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    if args.action == '_worker':
        worker(args.manifest)
        return
    if args.action == 'stop':
        atomic(STATE / 'stopped', b'explicit manual stop\n')
        launch('disable', TARGET, check=False)
        launch('bootout', TARGET, check=False)
        print('Stopped. Automatic update/restart will not override this; use start explicitly.')
        return
    if args.action == 'status':
        result = {'manually_stopped': stopped(), 'launchd_loaded': launch('print', TARGET, check=False).returncode == 0}
        if (STATE / 'latest.json').exists():
            job_id = load(STATE / 'latest.json')['job_id']
            result['update'] = load(STATE / 'jobs' / job_id / 'status.json')
        if (STATE / 'config.json').exists():
            try:
                state = call(load(STATE / 'config.json'), 'terminal_list', {})
                result.update(healthy=True, server_id=state['server_id'], draining=state.get('draining', False))
            except Exception as exc:
                result.update(healthy=False, error=str(exc))
        print(json.dumps(result, indent=2))
        return
    with open(STATE / 'controller.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.action == 'install':
            if (STATE / 'config.json').exists():
                raise RuntimeError('already configured; use update or start')
            project = str(Path(__file__).resolve().parent.parent)
            config = dict(project=project, listen='127.0.0.1:8090',
                token_file=str(Path.home() / '.config/terminal-mcp/token'),
                path=os.environ.get('PATH', '/usr/bin:/bin'))
            if args.adopt_pid <= 1:
                raise RuntimeError('initial adoption requires --adopt-pid for the running server')
            save(STATE / 'config.json', config)
            try:
                queue(config, args.adopt_pid, args.previous_binary)
            except Exception:
                (STATE / 'config.json').unlink(missing_ok=True)
                raise
        else:
            config = load(STATE / 'config.json')
            if args.action == 'start':
                (STATE / 'stopped').unlink(missing_ok=True)
                launch('enable', TARGET)
                if launch('print', TARGET, check=False).returncode != 0:
                    launch('bootstrap', DOMAIN, str(PLIST))
                else:
                    launch('kickstart', TARGET)
                print(json.dumps({'status': 'started', 'server_id': wait_ready(config)['server_id']}))
            else:
                queue(config, 0)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print('service:', str(error), file=sys.stderr)
        sys.exit(1)
