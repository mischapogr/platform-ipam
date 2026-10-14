"""Regression tests for pilot evidence visibility and freshness."""

from io import BytesIO
from copy import deepcopy
from datetime import datetime, timezone as datetime_timezone
import hashlib
import json
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch
import uuid
from zipfile import ZIP_DEFLATED, ZipFile

from django.core.files.uploadedfile import SimpleUploadedFile
from django.test import RequestFactory
from django.http import HttpResponse
from django.test import SimpleTestCase
from django.template import Context, Engine

from .views import DEMO_UPLOAD_FILES, WorkspaceView, link_existing_prefixes, prepare_pilot_view


class DemoUploadTest(SimpleTestCase):
    @patch("platform_ipam_workspace.views.render", return_value=HttpResponse("workspace"))
    @patch.object(WorkspaceView, "pilot_list", return_value=[])
    def test_bundled_100_and_500_account_zips_render_reports(self, _pilot_list, render_report):
        root = Path(__file__).with_name("fixtures")
        user = SimpleNamespace(is_authenticated=True, is_superuser=True,
                               has_perm=lambda _permission: False)
        for size in ("100", "500"):
            with self.subTest(size=size):
                archive = BytesIO()
                with ZipFile(archive, "w", compression=ZIP_DEFLATED) as bundle:
                    for filename in DEMO_UPLOAD_FILES:
                        bundle.write(root / size / filename, arcname=filename)
                request = RequestFactory().post("/plugins/platform-ipam/", {
                    "mode": "pilot",
                    "bundle": SimpleUploadedFile(
                        f"demo-{size}.zip", archive.getvalue(), content_type="application/zip"),
                })
                request.user = user

                response = WorkspaceView.as_view()(request)

                self.assertEqual(response.status_code, 200, response.content[:1000])
                context = render_report.call_args.args[2]
                report = context["report"]
                self.assertEqual(len(report["scope"]["accounts"]), int(size))
                self.assertEqual(report["execution_evidence"]["reported_moves"],
                                 len(report["replacements"]["moves"]))
                self.assertNotIn(b"id=\"server-upload-error\"", response.content)


class PrefixLinkPermissionTest(SimpleTestCase):
    @patch("ipam.models.Prefix.objects.filter")
    def test_only_prefixes_in_restricted_queryset_are_linked(self, filter_prefixes):
        user = SimpleNamespace(has_perm=lambda permission: True)
        prefix = Mock(prefix="10.20.0.0/22", get_absolute_url=Mock(return_value="/ipam/prefixes/7/"))
        query = Mock()
        restricted = Mock()
        query.restrict.return_value = restricted
        restricted.only.return_value = [prefix]
        filter_prefixes.return_value = query
        report = {"relationships": [{"sides": [{"cidr": "10.20.0.0/22"},
                                                 {"cidr": "10.30.0.0/22"}]}]}

        link_existing_prefixes(report, SimpleNamespace(user=user))

        query.restrict.assert_called_once_with(user, "view")
        self.assertEqual(report["relationships"][0]["sides"][0]["prefix_url"],
                         "/ipam/prefixes/7/")
        self.assertNotIn("prefix_url", report["relationships"][0]["sides"][1])
        self.assertEqual(report["relationships"][0]["sides"][1]["prefix_lookup_status"],
                         "not_in_netbox")


class VerificationFreshnessTest(SimpleTestCase):
    def test_saved_discovery_review_expires_on_read(self):
        report = {
            "version": 1, "scope": {"accounts": [], "regions": []},
            "discovery": {"cells": []}, "pilot_gates": {"discovery": "PASS"},
            "input_review": {"discovery_valid_until": "2026-09-23T12:10:00+00:00"},
            "replacements": {"moves": []},
        }
        with patch("platform_ipam_workspace.views.timezone.now",
                   return_value=datetime(2026, 9, 24, tzinfo=datetime_timezone.utc)):
            prepare_pilot_view(report)
        self.assertEqual(report["pilot_gates"]["discovery"], "PASS")
        self.assertEqual(report["discovery_review_current_status"], "EXPIRED")

    def test_expired_snapshot_keeps_history_but_shows_current_expiry(self):
        report = {
            "version": 1,
            "scope": {"accounts": [], "regions": []},
            "discovery": {"cells": []},
            "authority_verification": {"valid_until": "2026-09-23T00:00:00Z"},
            "replacements": {"moves": [{"approved": True,
                                         "reservation_status": "VERIFIED"}]},
        }
        with patch("platform_ipam_workspace.views.timezone.now",
                   return_value=datetime(2026, 9, 24, tzinfo=datetime_timezone.utc)):
            prepare_pilot_view(report)

        move = report["replacements"]["moves"][0]
        self.assertEqual(move["reservation_status"], "VERIFIED")
        self.assertEqual(move["reservation_current_status"], "EXPIRED")
        self.assertEqual(move["lifecycle_stage"], "VERIFICATION_EXPIRED")
        self.assertEqual(report["authority_verification_current_status"], "EXPIRED")

    def test_unexpired_snapshot_stays_current(self):
        report = {
            "version": 1,
            "scope": {"accounts": [], "regions": []},
            "discovery": {"cells": []},
            "authority_verification": {"valid_until": "2026-09-25T00:00:00Z"},
            "replacements": {"moves": [{"approved": True,
                                         "reservation_status": "VERIFIED"}]},
        }
        with patch("platform_ipam_workspace.views.timezone.now",
                   return_value=datetime(2026, 9, 24, tzinfo=datetime_timezone.utc)):
            prepare_pilot_view(report)

        self.assertEqual(report["authority_verification_current_status"], "CURRENT")
        self.assertEqual(report["replacements"]["moves"][0]["lifecycle_stage"], "VERIFIED")


class ReservationHandoffTest(SimpleTestCase):
    def test_workspace_refuses_unauthenticated_and_non_operator_reads(self):
        anonymous = SimpleNamespace(is_authenticated=False)
        non_operator = SimpleNamespace(
            is_authenticated=True, is_superuser=False,
            groups=SimpleNamespace(filter=lambda **_kwargs: SimpleNamespace(exists=lambda: False)))
        for user in (anonymous, non_operator):
            for method in ("get", "post"):
                with self.subTest(user=user, method=method):
                    request = getattr(RequestFactory(), method)("/plugins/platform-ipam/")
                    request.user = user
                    self.assertEqual(WorkspaceView.as_view()(request).status_code, 403)

    def test_only_reviewed_platform_moves_get_credential_free_handoff(self):
        target = {"tenant_id": "tenant-a", "allocation_key": "prod/orders", "scope": "vpc",
                  "environment": "prod", "region": "eu-central-1", "account_id": "123456789012",
                  "prefix_length": 20}
        moves = [{"approved": approved, "authority": authority, "target": target.copy()}
                 for approved, authority in ((True, "platform-ipam"),
                                             (False, "platform-ipam"), (True, "aws-ipam"))]
        report = {"version": 1, "scope": {"accounts": [], "regions": []},
                  "discovery": {"cells": []}, "replacements": {"moves": moves}}

        prepare_pilot_view(report)

        self.assertIn("platform-ipam client reserve", moves[0]["reserve_command"])
        self.assertEqual(json.loads(moves[0]["reserve_api_body"]),
                         {key: value for key, value in target.items() if key != "tenant_id"})
        self.assertNotIn("token", moves[0]["reserve_api_body"].lower())
        for move in moves[1:]:
            self.assertNotIn("reserve_command", move)
            self.assertNotIn("reserve_api_body", move)

        template = (Path(__file__).with_name("templates") / WorkspaceView.template_name).read_text()
        handoff = template.split('<div class="card-body border-top"><strong>Reservation handoff</strong>', 1)[1]
        handoff = handoff.split('<div class="card-body small text-muted">', 1)[0]
        rendered = Engine().from_string(handoff).render(Context({"report": report}))
        self.assertIn("prod/orders", rendered)
        self.assertIn("123456789012", rendered)
        self.assertIn("POST /v1/allocations", rendered)
        self.assertNotIn("Bearer ", rendered)
        self.assertNotIn("Authorization:", rendered)


class SnapshotDigestTest(SimpleTestCase):
    @patch.object(WorkspaceView, "pilot_list", return_value=[])
    @patch("platform_ipam_workspace.views.PilotSnapshot.objects.select_related")
    def test_changed_snapshot_is_refused_for_display_and_download(self, snapshots, _pilot_list):
        snapshots.return_value.get.return_value = SimpleNamespace(
            report={"version": 1}, report_sha256="0" * 64)
        user = SimpleNamespace(is_authenticated=True, is_superuser=True)
        snapshot_id = "00000000-0000-0000-0000-000000000001"
        for suffix in ("", "&download=1"):
            request = RequestFactory().get(f"/plugins/platform-ipam/?snapshot={snapshot_id}{suffix}")
            request.user = user
            response = WorkspaceView.as_view()(request)
            self.assertEqual(response.status_code, 409)


class SnapshotHistoryIntegrityTest(SimpleTestCase):
    report = {
        "version": 1, "scope": {"accounts": [], "regions": []},
        "discovery": {"cells": [], "scanned_cells": 2, "requested_cells": 3},
        "conflicts": {"confirmed": 1},
        "replacements": {"moves": [], "reservations_verified": 1, "planned": 2},
    }

    def setUp(self):
        template_root = Path(__file__).with_name("templates")
        source = (template_root / WorkspaceView.template_name).read_text()
        history_markup = source.split("{% block content %}", 1)[1].split(
            '<div class="alert alert-info">', 1)[0]
        self.template = Engine().from_string(history_markup)

    def snapshot(self, number, report, digest_report=None):
        body = json.dumps(report if digest_report is None else digest_report,
                          sort_keys=True, indent=2) + "\n"
        return SimpleNamespace(
            id=uuid.UUID(int=number), report=deepcopy(report),
            report_sha256=hashlib.sha256(body.encode()).hexdigest(),
            created_at=datetime(2026, 9, number, tzinfo=datetime_timezone.utc),
            created_by="operator",
        )

    def render_report(self, _request, _template, context, **kwargs):
        return HttpResponse(self.template.render(Context(context)), **kwargs)

    def request_with_history(self, history, query=""):
        snapshots = Mock()
        snapshots.select_related.return_value.all.return_value = history
        snapshots.count.return_value = len(history)
        pilot = SimpleNamespace(id=uuid.UUID(int=10), name="History pilot",
                                owner="pilot owner", snapshots=snapshots)
        for snapshot in history:
            snapshot.pilot = pilot
            snapshot.pilot_id = pilot.id
        request = RequestFactory().get("/plugins/platform-ipam/" + query)
        request.user = SimpleNamespace(is_authenticated=True, is_superuser=True,
                                       has_perm=lambda _permission: False)
        with patch("platform_ipam_workspace.views.MigrationPilot.objects.select_related") as pilots, \
                patch("platform_ipam_workspace.views.PilotSnapshot.objects.select_related") as reports, \
                patch("platform_ipam_workspace.views.render", side_effect=self.render_report):
            pilots.return_value.all.return_value = [pilot]
            reports.return_value.get.side_effect = lambda pk: next(
                snapshot for snapshot in history if snapshot.id == pk)
            response = WorkspaceView.as_view()(request)
        return response, pilot

    def test_tampered_history_keeps_valid_views_and_refuses_corrupt_selection(self):
        changed_counts = deepcopy(self.report)
        changed_counts["discovery"]["scanned_cells"] = 987654
        malformed_section = deepcopy(self.report)
        malformed_section["discovery"] = None
        malformed_count = deepcopy(self.report)
        malformed_count["conflicts"]["confirmed"] = "987654"
        for changed in (changed_counts, malformed_section, malformed_count, []):
            for corrupt_is_latest in (True, False):
                for query, expected_status in (
                        ("", 200), ("?snapshot=" + str(uuid.UUID(int=1)), 200),
                        ("?snapshot=" + str(uuid.UUID(int=1)) + "&download=1", 200),
                        ("?snapshot=" + str(uuid.UUID(int=2)), 409),
                        ("?snapshot=" + str(uuid.UUID(int=2)) + "&download=1", 409)):
                    with self.subTest(report=changed, corrupt_is_latest=corrupt_is_latest,
                                      query=query):
                        valid = self.snapshot(1, self.report)
                        corrupt = self.snapshot(2, changed, digest_report=self.report)
                        history = [corrupt, valid] if corrupt_is_latest else [valid, corrupt]
                        response, pilot = self.request_with_history(history, query)
                        self.assertEqual(response.status_code, expected_status)
                        self.assertFalse(corrupt.digest_valid)
                        self.assertIsNone(corrupt.history_summary)
                        self.assertIsNone(pilot.change)
                        if expected_status == 200 and "download" not in query:
                            self.assertContains(response, "Integrity check failed")
                            self.assertNotContains(response, "987654")
                            self.assertNotContains(response, "Latest versus previous")

    def test_checked_history_displays_counts_and_deltas(self):
        latest = deepcopy(self.report)
        latest["discovery"]["scanned_cells"] = 3
        latest["conflicts"]["confirmed"] = 0
        latest["replacements"]["reservations_verified"] = 2
        response, pilot = self.request_with_history([
            self.snapshot(2, latest), self.snapshot(1, self.report)])

        self.assertEqual(pilot.change, {"coverage": 1, "conflicts": -1, "verified": 1})
        self.assertContains(response, "Latest versus previous: scanned cells +1, blockers -1, verified moves +1")
        self.assertNotContains(response, "Integrity check failed")

    def test_checked_but_malformed_summaries_do_not_crash_history(self):
        malformed_section = deepcopy(self.report)
        malformed_section["discovery"] = None
        for value in ("2", None, [], True, -1, 1.5):
            malformed_count = deepcopy(self.report)
            malformed_count["replacements"]["reservations_verified"] = value
            with self.subTest(value=value):
                response, pilot = self.request_with_history([
                    self.snapshot(2, malformed_count), self.snapshot(1, self.report)])
                self.assertIsNone(pilot.change)
                self.assertContains(response, "Saved report summary unavailable.")
                self.assertNotContains(response, "Latest versus previous")
        response, pilot = self.request_with_history([
            self.snapshot(2, malformed_section), self.snapshot(1, self.report)])
        self.assertIsNone(pilot.change)
        self.assertContains(response, "Saved report summary unavailable.")
