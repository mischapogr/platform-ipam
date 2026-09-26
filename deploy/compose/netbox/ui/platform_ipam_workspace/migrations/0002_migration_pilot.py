import uuid

from django.conf import settings
from django.db import migrations, models
import django.db.models.deletion


def backfill_pilots(apps, schema_editor):
    MigrationPilot = apps.get_model("platform_ipam_workspace", "MigrationPilot")
    PilotSnapshot = apps.get_model("platform_ipam_workspace", "PilotSnapshot")
    for snapshot in PilotSnapshot.objects.all():
        pilot = MigrationPilot.objects.create(
            name=snapshot.name,
            owner=snapshot.report.get("pilot_owner") or "unknown",
            created_by_id=snapshot.created_by_id,
        )
        snapshot.pilot_id = pilot.id
        snapshot.save(update_fields=["pilot"])


class Migration(migrations.Migration):
    dependencies = [
        ("platform_ipam_workspace", "0001_pilot_snapshot"),
        migrations.swappable_dependency(settings.AUTH_USER_MODEL),
    ]

    operations = [
        migrations.CreateModel(
            name="MigrationPilot",
            fields=[
                ("id", models.UUIDField(primary_key=True, default=uuid.uuid4, editable=False, serialize=False)),
                ("name", models.CharField(max_length=160)),
                ("owner", models.CharField(max_length=160)),
                ("created_at", models.DateTimeField(auto_now_add=True)),
                ("created_by", models.ForeignKey(on_delete=django.db.models.deletion.PROTECT,
                                                 to=settings.AUTH_USER_MODEL)),
            ],
            options={"ordering": ("-created_at",)},
        ),
        migrations.AddField(
            model_name="pilotsnapshot", name="pilot",
            field=models.ForeignKey(null=True, on_delete=django.db.models.deletion.PROTECT,
                                    related_name="snapshots", to="platform_ipam_workspace.migrationpilot"),
        ),
        migrations.RunPython(backfill_pilots, migrations.RunPython.noop),
        migrations.AlterField(
            model_name="pilotsnapshot", name="pilot",
            field=models.ForeignKey(on_delete=django.db.models.deletion.PROTECT,
                                    related_name="snapshots", to="platform_ipam_workspace.migrationpilot"),
        ),
    ]
