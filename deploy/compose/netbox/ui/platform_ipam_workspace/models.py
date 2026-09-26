"""Operator-saved, immutable projections of pilot evidence."""

import uuid

from django.conf import settings
from django.db import models


class MigrationPilot(models.Model):
    id = models.UUIDField(primary_key=True, default=uuid.uuid4, editable=False)
    name = models.CharField(max_length=160)
    owner = models.CharField(max_length=160)
    created_at = models.DateTimeField(auto_now_add=True)
    created_by = models.ForeignKey(settings.AUTH_USER_MODEL, on_delete=models.PROTECT)

    class Meta:
        ordering = ("-created_at",)

    def __str__(self):
        return self.name


class PilotSnapshot(models.Model):
    id = models.UUIDField(primary_key=True, default=uuid.uuid4, editable=False)
    name = models.CharField(max_length=160)
    created_at = models.DateTimeField(auto_now_add=True)
    created_by = models.ForeignKey(settings.AUTH_USER_MODEL, on_delete=models.PROTECT)
    pilot = models.ForeignKey(MigrationPilot, on_delete=models.PROTECT, related_name="snapshots")
    report_sha256 = models.CharField(max_length=64)
    report = models.JSONField()

    class Meta:
        ordering = ("-created_at",)

    def __str__(self):
        return self.name
