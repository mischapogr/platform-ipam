"""Run through NetBox manage.py shell in the isolated restore rehearsal."""

from copy import deepcopy
import hashlib
import json
from pathlib import Path
import uuid

from django.contrib.auth import get_user_model
from django.contrib.auth.models import AnonymousUser
from django.db.models.deletion import ProtectedError
from django.test import RequestFactory
from users.models import Group

from platform_ipam_workspace.integrity import snapshot_digest_matches
from platform_ipam_workspace.models import MigrationPilot, PilotSnapshot
from platform_ipam_workspace.views import WorkspaceView


PILOT_ID = uuid.UUID("00000000-0000-4000-8000-000000000041")
SNAPSHOT_IDS = [uuid.UUID(int=number) for number in (42, 43, 44)]
ALLOCATION_ID = "00000000-0000-4000-8000-000000000045"
User = get_user_model()


def body(report):
    return (json.dumps(report, sort_keys=True, indent=2) + "\n").encode()


def seed():
    operator = User.objects.create_user(username="restore-operator")
    operator.groups.add(Group.objects.get_or_create(name="platform-operators")[0])
    User.objects.create_user(username="restore-outsider")
    pilot = MigrationPilot.objects.create(
        id=PILOT_ID, name="Synthetic restore rehearsal", owner="synthetic-owner", created_by=operator)
    report = json.loads(Path("/opt/netbox/netbox/platform_ipam_workspace/sample_pilot.json").read_text())
    original = deepcopy(report)
    move = report["replacements"]["moves"][0]
    move.update(allocation_id=ALLOCATION_ID, returned_cidr=move.get("proposed_cidr") or "10.240.0.0/22",
                reservation_status="VERIFIED", target_fact="reserved")
    report["authority_verification"] = {
        "read_at": "2026-01-01T00:00:00Z", "valid_until": "2026-01-01T00:10:00Z",
    }
    report["replacements"].update(reservations_verified=1)
    for snapshot_id, value in zip(SNAPSHOT_IDS, (original, report, report)):
        PilotSnapshot.objects.create(
            id=snapshot_id, pilot=pilot, name=pilot.name, created_by=operator,
            report=value, report_sha256=hashlib.sha256(body(value)).hexdigest())
    # Deliberate stored corruption, with the original digest retained.
    corrupted = deepcopy(report)
    corrupted["replacements"]["moves"][0]["allocation_id"] = "changed-without-review"
    PilotSnapshot.objects.filter(pk=SNAPSHOT_IDS[2]).update(report=corrupted)


def request(user, snapshot_id, download=False, method="get"):
    path = f"/plugins/platform-ipam/?snapshot={snapshot_id}"
    if download:
        path += "&download=1"
    req = getattr(RequestFactory(), method)(path)
    req.user = user
    return WorkspaceView.as_view()(req)


def check():
    operator = User.objects.get(username="restore-operator")
    outsider = User.objects.get(username="restore-outsider")
    assert not operator.is_superuser
    assert operator.groups.filter(name="platform-operators").exists()
    assert not outsider.groups.exists()
    pilot = MigrationPilot.objects.get(pk=PILOT_ID)
    snapshots = list(pilot.snapshots.order_by("id"))
    assert [item.id for item in snapshots] == SNAPSHOT_IDS
    assert [snapshot_digest_matches(item) for item in snapshots] == [True, True, False]
    for snapshot in snapshots[:2]:
        downloaded = request(operator, snapshot.id, download=True)
        assert downloaded.status_code == 200
        assert downloaded.content == body(snapshot.report)
        rendered = request(operator, snapshot.id)
        assert rendered.status_code == 200
    rendered = request(operator, SNAPSHOT_IDS[1]).content.decode()
    assert ALLOCATION_ID in rendered
    assert "VERIFICATION_EXPIRED" in rendered
    # Restore must not silently repair or admit corrupted historical evidence.
    for download in (False, True):
        assert request(operator, SNAPSHOT_IDS[2], download).status_code == 409
        for user in (outsider, AnonymousUser()):
            assert request(user, SNAPSHOT_IDS[1], download).status_code == 403
    for user in (outsider, AnonymousUser()):
        assert request(user, SNAPSHOT_IDS[1], method="post").status_code == 403
    try:
        pilot.delete()
    except ProtectedError:
        pass
    else:
        raise AssertionError("Pilot deletion removed retained snapshots")
    try:
        operator.delete()
    except ProtectedError:
        pass
    else:
        raise AssertionError("Creator deletion removed retained provenance")
    return {
        "pilot": {"id": str(pilot.id), "owner": pilot.owner, "name": pilot.name,
                  "created_at": pilot.created_at.isoformat(), "created_by": pilot.created_by.username,
                  "created_by_id": pilot.created_by_id},
        "operator": {"id": operator.pk, "active": operator.is_active,
                     "groups": list(operator.groups.order_by("name").values_list("name", flat=True))},
        "snapshots": [{"id": str(item.id), "name": item.name, "created_at": item.created_at.isoformat(),
                       "created_by": item.created_by.username, "digest": item.report_sha256,
                       "stored_body_sha256": hashlib.sha256(body(item.report)).hexdigest(),
                       "valid": snapshot_digest_matches(item)} for item in snapshots],
        "checks": ["operator_render_and_export", "outsider_and_anonymous_read_write_denied",
                   "corruption_refused", "expired_verification_visible", "creator_and_pilot_protected"],
    }


if REHEARSAL_PHASE == "seed":
    seed()
print("RESTORE_PROBE=" + json.dumps(check(), sort_keys=True))
