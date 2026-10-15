"""Inspect the built-in housekeeping job in a disposable NetBox restore fixture.

Executed through manage.py shell; the parent runner supplies ACTION.
"""

from dataclasses import asdict, is_dataclass
from datetime import timedelta
import json
import re
import uuid

from django.conf import settings
from django.core.serializers.json import DjangoJSONEncoder
from django.utils import timezone
from django_rq import get_queue
from rq.job import Job as RQJob
from rq.registry import ScheduledJobRegistry

from core.choices import JobStatusChoices
from core.jobs import SystemHousekeepingJob
from core.models import Job
from netbox.config import Config


class Encoder(DjangoJSONEncoder):
    def default(self, value):
        return asdict(value) if is_dataclass(value) else super().default(value)


def snapshot():
    config = Config()
    if not settings.ISOLATED_DEPLOYMENT or config.JOB_RETENTION != 0 or config.CHANGELOG_RETENTION != 0:
        raise AssertionError('System-job fixture requires isolated mode and disabled local pruning')
    active = []
    for job in SystemHousekeepingJob.get_jobs().filter(status__in=JobStatusChoices.ENQUEUED_STATE_CHOICES):
        queue = get_queue(job.queue_name)
        rq_job = RQJob.fetch(str(job.job_id), connection=queue.connection)
        if rq_job.instance is not SystemHousekeepingJob or rq_job.func_name != 'handle':
            raise AssertionError('System job references an unexpected handler')
        payload = rq_job.kwargs['job']
        if payload.pk != job.pk or payload.job_id != job.job_id:
            raise AssertionError('Database and queued system-job identities differ')
        registry = ScheduledJobRegistry(queue.name, connection=queue.connection)
        score = queue.connection.zscore(registry.key, str(job.job_id))
        status = rq_job.get_status(refresh=True).value
        if job.status == 'scheduled' and (status != 'scheduled' or score is None):
            raise AssertionError('Scheduled system job lacks its matching queue registry entry')
        active.append({'id': job.pk, 'job_id': str(job.job_id), 'status': status,
                       'function': f'{rq_job.instance.__module__}.{rq_job.instance.__name__}.{rq_job.func_name}', 'scheduled_score': score,
                       'queued_occurrences': queue.job_ids.count(str(job.job_id))})
    return {'rows': list(SystemHousekeepingJob.get_jobs().order_by('pk').values()), 'active': active,
            'settings': {'isolated_deployment': settings.ISOLATED_DEPLOYMENT,
                         'job_retention': config.JOB_RETENTION, 'changelog_retention': config.CHANGELOG_RETENTION}}


def seed():
    # Replace only this disposable project's pre-existing future occurrence.
    for job in SystemHousekeepingJob.get_jobs().filter(status__in=JobStatusChoices.ENQUEUED_STATE_CHOICES):
        if job.status == 'running':
            raise AssertionError('System-job fixture writer was not quiesced')
        job.delete()
    job = SystemHousekeepingJob.enqueue_once(schedule_at=timezone.now() + timedelta(seconds=15), interval=1440,
                                             notifications='never')
    repeated = SystemHousekeepingJob.enqueue_once(interval=1440)
    if repeated.pk != job.pk:
        raise AssertionError('Repeated registration duplicated the system job')
    result = snapshot()
    if len(result['active']) != 1:
        raise AssertionError('Expected one scheduled housekeeping occurrence')
    return result


def negative():
    original = snapshot()
    job = SystemHousekeepingJob.get_jobs().get(status='scheduled')
    original_id = job.job_id
    Job.objects.filter(pk=job.pk).update(job_id=uuid.uuid4())
    try:
        from rq.exceptions import NoSuchJobError
        try:
            snapshot()
        except NoSuchJobError:
            pass
        else:
            raise AssertionError('Mismatched database/queue identity was admitted')
    finally:
        Job.objects.filter(pk=job.pk).update(job_id=original_id)
    if snapshot() != original:
        raise AssertionError('Negative preflight did not restore the original system-job evidence')
    return True


try:
    result = {'seed': seed, 'snapshot': snapshot, 'negative': negative}[ACTION]()
except Exception as error:
    result = {'fixture_error': type(error).__name__ + ': ' + re.sub(r'[A-Za-z0-9_-]{32,}', '[redacted]', str(error))}
print(json.dumps(result, cls=Encoder, sort_keys=True))
