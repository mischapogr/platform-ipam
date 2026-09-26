"""Regression tests for pilot evidence visibility and freshness."""

from io import BytesIO
from datetime import datetime, timezone as datetime_timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch
from zipfile import ZIP_DEFLATED, ZipFile

from django.core.files.uploadedfile import SimpleUploadedFile
from django.test import RequestFactory
from django.http import HttpResponse
from django.test import SimpleTestCase

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
