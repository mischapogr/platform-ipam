"""Synthetic jobs and controls for the disposable pinned-RQ recovery runner."""

from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import time

from redis import Redis
from rq import Queue, Worker
from rq.job import Job
from rq.registry import FailedJobRegistry, ScheduledJobRegistry, StartedJobRegistry

RECEIPT = Path('/opt/netbox/netbox/media/interrupted.json')
EVENTS = RECEIPT.with_name('executions.jsonl')
PAYLOAD = b'{"key":"interrupted","result":42}\n'


def connection():
    return Redis(host='netbox-redis', password=os.environ['REDIS_PASSWORD'], db=0)


def receipt_once():
    """Crash after a durable local receipt; a reviewed retry reuses it."""
    try:
        descriptor = os.open(RECEIPT, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        if RECEIPT.read_bytes() != PAYLOAD:
            raise AssertionError('Existing receipt is not the expected result')
        action = 'receipt_reused'
    else:
        with os.fdopen(descriptor, 'wb') as output:
            output.write(PAYLOAD)
            output.flush()
            os.fsync(output.fileno())
        directory = os.open(RECEIPT.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        action = 'receipt_created'
    with EVENTS.open('a') as output:
        output.write(json.dumps({'action': action}) + '\n')
        output.flush()
        os.fsync(output.fileno())
    if action == 'receipt_created':
        # The runner kills the worker container before the result is committed.
        while True:
            time.sleep(0.1)
    return 42


def state():
    client = connection()
    queue = Queue('recovery', connection=client)
    jobs = {}
    for name in ('interrupted', 'scheduled'):
        job = Job.fetch(name, connection=client)
        status = job.get_status(refresh=True)
        jobs[name] = {'id': job.id, 'status': status.value, 'function': job.func_name,
                      'args': job.args, 'result': job.return_value(),
                      'queued_occurrences': queue.job_ids.count(name),
                      'abandoned_error': 'AbandonedJobError' in (job.exc_info or '')}
    manifest = {}
    for path in (RECEIPT, EVENTS):
        if path.exists():
            stat = path.stat()
            manifest[path.name] = {'sha256': hashlib.sha256(path.read_bytes()).hexdigest(),
                                   'bytes': stat.st_size, 'mode': stat.st_mode & 0o777,
                                   'uid': stat.st_uid, 'gid': stat.st_gid}
    started = StartedJobRegistry('recovery', connection=client)
    return {'jobs': jobs, 'media': manifest,
            'events': [json.loads(line) for line in EVENTS.read_text().splitlines()] if EVENTS.exists() else [],
            'started_entries': [[key.decode(), score] for key, score in client.zrange(started.key, 0, -1, withscores=True)],
            'scheduled_ids': ScheduledJobRegistry('recovery', connection=client).get_job_ids(),
            'failed_ids': FailedJobRegistry('recovery', connection=client).get_job_ids()}


def requeue_after_review():
    client = connection()
    job = Job.fetch('interrupted', connection=client)
    if job.get_status(refresh=True).value != 'failed' or RECEIPT.read_bytes() != PAYLOAD:
        raise AssertionError('Fixture review refuses retry without the failed job and its exact receipt')
    FailedJobRegistry('recovery', connection=client).requeue(job)
    return True


def dispatch(action):
    client = connection()
    queue = Queue('recovery', connection=client)
    if action == 'seed':
        scheduled_at = datetime.now(timezone.utc) + timedelta(seconds=10)
        queue.enqueue_at(scheduled_at, 'builtins.sum', [20, 22], job_id='scheduled', result_ttl=3600)
        queue.enqueue(receipt_once, job_id='interrupted', job_timeout=10, failure_ttl=3600, result_ttl=3600)
        from importlib.metadata import version
        return {'scheduled_at': scheduled_at.isoformat(), 'packages': {name: version(name) for name in ('rq', 'redis')}}
    if action == 'worker':
        Worker([queue], connection=client).work(with_scheduler=os.environ['QUEUE_RECOVERY_SCHEDULER'] == 'true')
        return None
    if action == 'state':
        return state()
    if action == 'cleanup':
        # Use the real clock; no timestamp override or simulated expiry.
        StartedJobRegistry('recovery', connection=client).cleanup()
        return state()
    if action == 'requeue':
        return requeue_after_review()
    if action == 'negative':
        original = RECEIPT.read_bytes()
        RECEIPT.write_bytes(b'deliberately-invalid-receipt')
        try:
            try:
                requeue_after_review()
            except AssertionError:
                pass
            else:
                raise AssertionError('Corrupted receipt was admitted')
        finally:
            RECEIPT.write_bytes(original)
        return True
    raise ValueError('Unknown fixture action')
