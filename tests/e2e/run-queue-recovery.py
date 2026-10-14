#!/usr/bin/env python3
"""Cold-restore scheduled and interrupted synthetic jobs using pinned RQ/Valkey."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
COMPOSE = ROOT / 'deploy/compose'
FIXTURE = ROOT / 'tests/e2e/queue_recovery_fixture.py'


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path)
    args = parser.parse_args()
    project = 'platform-ipam-queue-recovery-test-' + secrets.token_hex(5)
    env = os.environ.copy()
    env.update(COMPOSE_PROJECT_NAME=project, NETBOX_REDIS_PASSWORD=secrets.token_urlsafe(24),
               NETBOX_UI_PASSWORD=secrets.token_urlsafe(32), NETBOX_SUPERUSER_PASSWORD=secrets.token_urlsafe(32),
               IPAM_NETBOX_TOKEN=secrets.token_hex(20), NETBOX_E2E_VIEWER_TOKEN=secrets.token_hex(20))
    checks = []
    result = None
    restored = False
    with tempfile.TemporaryDirectory(prefix='platform-ipam-queue-recovery-') as directory:
        private = Path(directory)
        overlay = private / 'queue.json'
        argv = ['docker', 'compose', '--project-name', project, '--env-file', str(COMPOSE / '.env.example'),
                '-f', str(COMPOSE / 'compose.yaml'), '-f', str(COMPOSE / 'compose.netbox.yaml'), '-f', str(overlay)]

        def write_overlay():
            services = {}
            for name in ('netbox', 'netbox-worker'):
                services[name] = {'entrypoint': ['/opt/netbox/venv/bin/python'], 'user': '0:0',
                                  'command': ['-c', "import sys; sys.path.insert(0,'/fixture'); import queue_recovery_fixture as f; f.dispatch('worker')"],
                                  'environment': {'QUEUE_RECOVERY_SCHEDULER': str(restored).lower()},
                                  'volumes': [str(FIXTURE.parent) + ':/fixture:ro']}
                if restored:
                    services[name]['volumes'].append('media-restored:/opt/netbox/netbox/media')
            if restored:
                services['netbox-redis'] = {'volumes': ['queue-restored:/data']}
            for name, source, target in (('queue', 'netbox-redis-data', 'queue-restored'),
                                         ('media', 'netbox-media-files', 'media-restored')):
                services[name + '-custody'] = {'image': 'docker.io/valkey/valkey:9.1-alpine', 'profiles': ['custody'],
                                              'network_mode': 'none', 'user': '0:0', 'entrypoint': ['/bin/sh', '-c'],
                                              'volumes': [source + ':/source:ro', target + ':/target']}
            overlay.write_text(json.dumps({'services': services, 'volumes': {'queue-restored': {}, 'media-restored': {}}}))
            overlay.chmod(0o600)

        def execute(*parts, data=None):
            completed = subprocess.run(argv + list(parts), cwd=COMPOSE, env=env, input=data,
                                       capture_output=True, timeout=180)
            if completed.returncode:
                # Never print interpolated configuration or credential-bearing output.
                raise RuntimeError(f'Compose {parts[0]} failed with exit {completed.returncode}')
            return completed.stdout

        def control(action):
            code = "import sys,json; sys.path.insert(0,'/fixture'); import queue_recovery_fixture as f; "
            code += 'print(json.dumps(f.dispatch(' + repr(action) + '),sort_keys=True))'
            return json.loads(execute('run', '--rm', '--no-deps', '-T', 'netbox', '-c', code).decode().splitlines()[-1])

        def wait_state(predicate, timeout=120, cleanup=False):
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                value = control('cleanup' if cleanup else 'state')
                if predicate(value):
                    return value
                time.sleep(1)
            raise AssertionError('Synthetic queue recovery did not reach its required state')

        write_overlay()
        try:
            print('Starting isolated pinned queue runtime: ' + project, flush=True)
            execute('up', '-d', '--no-deps', '--wait', 'netbox-redis')
            seed = control('seed')
            execute('up', '-d', '--no-deps', 'netbox-worker')
            wait_state(lambda s: s['jobs']['interrupted']['status'] == 'started' and len(s['events']) == 1)
            execute('kill', '--signal', 'SIGKILL', 'netbox-worker')
            source = control('state')
            if source['jobs']['interrupted']['status'] != 'started' or source['jobs']['interrupted']['result'] is not None:
                raise AssertionError('Interrupted job was not killed before committing its result')
            if source['jobs']['scheduled']['status'] != 'scheduled' or source['scheduled_ids'] != ['scheduled']:
                raise AssertionError('Scheduled witness executed before backup')
            if source['events'] != [{'action': 'receipt_created'}] or not source['started_entries']:
                raise AssertionError('Interrupted job lacks its durable receipt/started entry')
            checks.append('scheduled_job_and_killed_started_job_with_durable_receipt')
            print('Worker killed after durable receipt, before result commit; archiving cold queue/media', flush=True)
            execute('stop', 'netbox-redis')
            archives = {}
            backup_evidence = {}
            for name in ('queue', 'media'):
                data = execute('run', '--rm', '--no-deps', '-T', name + '-custody', 'tar -C /source -cf - .')
                path = private / (name + '.tar')
                path.write_bytes(data)
                path.chmod(0o600)
                archives[name] = data
                backup_evidence[name] = {'bytes': len(data), 'sha256': hashlib.sha256(data).hexdigest()}
                execute('run', '--rm', '--no-deps', '-T', name + '-custody',
                        'test -z "$(ls -A /target)" && tar -xpf - -C /target', data=data)
            restored = True
            write_overlay()
            execute('up', '-d', '--no-deps', '--force-recreate', '--wait', 'netbox-redis')
            if control('state') != source:
                raise AssertionError('Cold restore changed jobs, registries, receipt bytes or file metadata')
            checks.append('fresh_queue_media_restore_matches_started_and_scheduled_state')
            denied = execute('exec', '-T', 'netbox-redis', 'valkey-cli', 'PING')
            if b'NOAUTH' not in denied or b'PONG' in denied:
                raise AssertionError('Restored queue admitted anonymous access')
            checks.append('restored_queue_requires_authentication')
            execute('up', '-d', '--no-deps', '--force-recreate', 'netbox-worker')
            scheduled = wait_state(lambda s: s['jobs']['scheduled']['status'] == 'finished')
            if scheduled['jobs']['scheduled']['result'] != 42 or scheduled['scheduled_ids'] or scheduled['jobs']['scheduled']['queued_occurrences']:
                raise AssertionError('Restored scheduled job did not settle once')
            checks.append('real_rq_scheduler_completed_restored_due_job')
            print('Scheduled job finished; waiting for real started-entry expiry and abandoned-job cleanup', flush=True)
            failed = wait_state(lambda s: s['jobs']['interrupted']['status'] == 'failed', cleanup=True)
            if not failed['jobs']['interrupted']['abandoned_error'] or failed['jobs']['interrupted']['queued_occurrences']:
                raise AssertionError('Abandoned job was silently requeued or lacks failure evidence')
            if failed['events'] != source['events'] or failed['media'] != source['media']:
                raise AssertionError('Abandoned-job cleanup replayed the durable effect')
            checks.append('real_expiry_marks_abandoned_job_failed_without_replay')
            if control('negative') is not True or control('state')['jobs']['interrupted']['status'] != 'failed':
                raise AssertionError('Invalid receipt review mutated job admission')
            checks.append('fixture_review_refuses_invalid_receipt_without_requeue')
            control('requeue')
            completed = wait_state(lambda s: s['jobs']['interrupted']['status'] == 'finished')
            if completed['jobs']['interrupted']['result'] != 42 or completed['jobs']['interrupted']['queued_occurrences']:
                raise AssertionError('Reviewed retry failed or remained queued')
            if completed['media']['interrupted.json'] != source['media']['interrupted.json']:
                raise AssertionError('Reviewed retry changed the durable receipt')
            if completed['events'] != [{'action': 'receipt_created'}, {'action': 'receipt_reused'}]:
                raise AssertionError('Reviewed retry duplicated the synthetic effect')
            checks.append('explicit_same_job_retry_reuses_receipt_without_duplicate_effect')
            execute('stop', 'netbox-worker')
            final = control('state')
            if final['started_entries'] or final['scheduled_ids'] or final['failed_ids']:
                raise AssertionError('Settled jobs remain in an active/failure registry')
            checks.append('finished_jobs_leave_started_scheduled_and_failed_registries')
            images = {}
            for name in ('netbox-worker', 'netbox-redis'):
                container = execute('ps', '-aq', name).decode().strip()
                images[name] = subprocess.check_output(['docker', 'inspect', '--format', '{{.Image}}', container], text=True).strip()
            result = {'status': 'PASSED', 'completed_at': datetime.now(timezone.utc).isoformat(), 'project': project,
                      'checks': checks, 'seed': seed, 'source': source, 'failed_after_restore': failed,
                      'final': final, 'archives': backup_evidence, 'runtime_image_ids': images,
                      'source_sha256': {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
                                        for path in (Path(__file__).resolve(), FIXTURE, COMPOSE / 'compose.yaml', COMPOSE / 'compose.netbox.yaml')},
                      'limits': ['development_compose_dependencies_only', 'pinned_rq_not_django_netbox_management_integration',
                                 'synthetic_local_receipt_not_external_job_side_effects', 'fixture_review_not_production_retry_policy',
                                 'no_customer_jobs_or_real_oidc_or_aws', 'no_production_custody_or_cutover'],
                      'credential_values_retained': False}
        finally:
            execute('down', '--volumes', '--remove-orphans')
            ownership = 'label=com.docker.compose.project=' + project
            volumes = subprocess.check_output(['docker', 'volume', 'ls', '--filter', ownership, '--format', '{{.Name}}'], text=True).splitlines()
            if volumes:
                subprocess.run(['docker', 'volume', 'rm', *volumes], check=True, capture_output=True)
            for command in (['docker', 'ps', '-aq'], ['docker', 'volume', 'ls', '-q'], ['docker', 'network', 'ls', '-q']):
                if subprocess.check_output(command + ['--filter', ownership]).strip():
                    raise AssertionError('Disposable queue fixture resources remain')
        result['cleanup'] = 'isolated_project_removed'
        if args.out:
            args.out.write_text(json.dumps(result, indent=2, sort_keys=True) + '\n')
        print(json.dumps({'status': result['status'], 'checks': len(checks), 'cleanup': result['cleanup']}), flush=True)


if __name__ == '__main__':
    run()
