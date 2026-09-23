"""Run the existing offline engines on uploaded evidence; never write inventory."""

import json
import hashlib
from io import BytesIO
import shlex
import subprocess
import tempfile
import uuid
from pathlib import Path
from urllib.parse import urlencode
from zipfile import BadZipFile, ZIP_DEFLATED, ZipFile

from django.http import HttpResponse, HttpResponseForbidden
from django.http import Http404
from django.shortcuts import render
from django.utils.decorators import method_decorator
from django.views import View
from django.views.decorators.cache import never_cache

from .models import MigrationPilot, PilotSnapshot


MAX_FILE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_BYTES = 64 * 1024 * 1024
INPUT_FILES = {
    "networks": "networks.csv",
    "accounts": "accounts.json",
    "failures": "failures.csv",
    "run": "run.json",
    "matrix": "matrix.yaml",
    "ownership": "ownership.yaml",
    "fixed": "fixed.yaml",
    "decisions": "decisions.yaml",
    "plan": "migration.yaml",
    "allocations": "allocations.json",
    "verification": "verification.json",
    "execution": "execution.json",
    "topology": "topology.json",
    "approved": "approved-plan.json",
    "pilot": "pilot-scope.json",
}
DEMO_UPLOAD_FILES = ("networks.csv", "accounts.json", "failures.csv", "run.json",
                     "matrix.yaml", "ownership.yaml", "fixed.yaml", "approved-plan.json",
                     "pilot-scope.json", "migration.yaml", "execution.json")
REQUIRED_UPLOADS = {
    "assess": ("networks",),
    "progress": ("networks", "plan"),
    "address": ("networks", "accounts", "matrix", "fixed", "approved", "pilot"),
    "pilot": ("networks", "accounts", "run", "matrix", "fixed", "approved", "pilot", "plan"),
    "topology": ("topology",),
}
OPTION_FLAGS = ("matrix", "ownership", "fixed", "decisions")


def link_existing_prefixes(report, request):
    """Link only unique exact NetBox Prefix matches visible to this operator."""
    if not request.user.has_perm("ipam.view_prefix"):
        return
    sides = [side for relationship in report.get("relationships", [])
             for side in relationship.get("sides", [])]
    if not sides and isinstance(report.get("conflicts"), list):
        sides = [side for conflict in report["conflicts"] for side in conflict.get("sides", [])]
    moves = report.get("replacements", {}).get("moves", [])
    inventory = report.get("discovery", {}).get("inventory_vpcs", [])
    cidrs = {side.get("cidr") for side in sides}
    cidrs.update(move.get("returned_cidr") for move in moves)
    cidrs.update(cidr for item in inventory for cidr in item.get("cidrs", []))
    cidrs.discard(None)
    if not cidrs:
        return
    matches = {}
    lookup_status = "checked"
    try:
        from ipam.models import Prefix
        # Keep queries bounded while still linking the 500-account demo.
        if len(cidrs) <= 5000:
            ordered = sorted(cidrs)
            for start in range(0, len(ordered), 250):
                for prefix in Prefix.objects.filter(prefix__in=ordered[start:start + 250]).only("id", "prefix"):
                    matches.setdefault(str(prefix.prefix), []).append(prefix)
        else:
            lookup_status = "skipped"
    except Exception:
        # The report remains useful if the optional NetBox lookup is unavailable.
        lookup_status = "unavailable"

    def link_for(cidr):
        exact = matches.get(cidr, [])
        if lookup_status != "checked":
            return {"prefix_lookup_status": lookup_status}
        if len(exact) == 1:
            return {"prefix_url": exact[0].get_absolute_url()}
        if len(exact) > 1:
            return {"prefix_lookup_status": "multiple_matches",
                    "prefix_search_url": "/ipam/prefixes/?" + urlencode({"q": cidr})}
        return {"prefix_lookup_status": "not_in_netbox"}

    for side in sides:
        side.update(link_for(side.get("cidr")))
    for move in moves:
        cidr = move.get("returned_cidr")
        if cidr and len(matches.get(cidr, [])) == 1:
            move["allocation_prefix_url"] = matches[cidr][0].get_absolute_url()
    for item in inventory:
        item["cidr_links"] = [{"cidr": cidr, **link_for(cidr)}
                              for cidr in item.get("cidrs", [])]


def prepare_pilot_view(report):
    """Arrange existing evidence for drill-down without changing the saved report."""
    if report.get("version") != 1 or "discovery" not in report:
        return
    scope = report.get("scope") or {}
    cells = {(cell["account_id"], cell["region"]): cell
             for cell in report["discovery"].get("cells", [])}
    report["coverage_matrix"] = [
        {"account_id": account, "cells": [
            {"region": region, **cells.get((account, region), {"status": "not_attempted",
                                                        "reason": "missing-from-run"})}
            for region in scope.get("regions", [])]}
        for account in scope.get("accounts", [])
    ]
    confirmed = [relationship for relationship in report.get("relationships", [])
                 if relationship.get("impact") == "confirmed"]
    report["affected_accounts"] = len({side["account_id"] for item in confirmed
                                       for side in item["sides"]})
    report["affected_vpcs"] = len({(side["account_id"], side["region"], side["vpc_id"])
                                   for item in confirmed for side in item["sides"]})
    for move in report.get("replacements", {}).get("moves", []):
        if not move.get("approved"):
            stage = "NEEDS_REVIEW"
        elif move.get("reservation_status") == "VERIFIED":
            stage = "VERIFIED"
        elif move.get("reservation_status") == "FAIL":
            stage = "AUTHORITY_CONFLICT"
        elif move.get("proposal_status") == "REQUEST_FROM_AWS_IPAM":
            stage = "AUTHORITY_HANDOFF_REQUIRED"
        elif move.get("proposal_status") != "ADVISORY_CANDIDATE":
            stage = "NEEDS_CANDIDATE"
        elif move.get("target_fact") in ("reserved", "active"):
            stage = "RESERVATION_UNVERIFIED"
        else:
            stage = "REVIEWED"
        move["lifecycle_stage"] = stage
        target = move.get("target") or {}
        required = ("allocation_key", "scope", "environment", "region", "account_id", "prefix_length")
        if move.get("approved") and move.get("authority") == "platform-ipam" and all(
                target.get(field) for field in required):
            parts = ["platform-ipam", "client", "reserve", "--key", target["allocation_key"],
                     "--scope", target["scope"], "--env", target["environment"],
                     "--region", target["region"], "--account", target["account_id"],
                     "--prefix-length", str(target["prefix_length"])]
            move["reserve_command"] = " ".join(shlex.quote(str(part)) for part in parts)


@method_decorator(never_cache, name="dispatch")
class WorkspaceView(View):
    template_name = "platform_ipam_workspace/workspace.html"

    @staticmethod
    def pilot_list():
        pilots = list(MigrationPilot.objects.select_related("created_by").all()[:20])
        for pilot in pilots:
            pilot.recent_snapshots = list(pilot.snapshots.select_related("created_by").all()[:10])
            pilot.latest_snapshot = pilot.recent_snapshots[0] if pilot.recent_snapshots else None
            if len(pilot.recent_snapshots) > 1:
                current, previous = (item.report for item in pilot.recent_snapshots[:2])
                pilot.change = {
                    "coverage": current.get("discovery", {}).get("scanned_cells", 0) - previous.get("discovery", {}).get("scanned_cells", 0),
                    "conflicts": current.get("conflicts", {}).get("confirmed", 0) - previous.get("conflicts", {}).get("confirmed", 0),
                    "verified": current.get("replacements", {}).get("reservations_verified", 0) - previous.get("replacements", {}).get("reservations_verified", 0),
                }
        return pilots

    def upload_error(self, request, mode, message):
        return render(request, self.template_name,
                      {"error": message, "mode": mode, "saved_pilots": self.pilot_list()}, status=400)

    def dispatch(self, request, *args, **kwargs):
        if not request.user.is_authenticated:
            return HttpResponseForbidden("Sign in to NetBox to view the migration workspace.")
        if not (
            request.user.is_superuser
            or request.user.groups.filter(name="platform-operators").exists()
        ):
            return HttpResponseForbidden("The migration workspace requires an operator identity.")
        return super().dispatch(request, *args, **kwargs)

    def get(self, request):
        fixture_size = request.GET.get("download_fixture")
        if fixture_size in ("100", "500"):
            source = Path(__file__).with_name("fixtures") / fixture_size
            archive = BytesIO()
            with ZipFile(archive, "w", compression=ZIP_DEFLATED) as bundle:
                for filename in DEMO_UPLOAD_FILES:
                    bundle.write(source / filename, arcname=f"platform-ipam-{fixture_size}-account-demo/{filename}")
                bundle.writestr(f"platform-ipam-{fixture_size}-account-demo/README.txt",
                               "Synthetic, dated demo evidence. Upload the files to Migration pilot overview. "
                               "Uploading does not import NetBox objects or reserve CIDRs.\n")
            response = HttpResponse(archive.getvalue(), content_type="application/zip")
            response["Content-Disposition"] = f'attachment; filename="platform-ipam-{fixture_size}-account-demo.zip"'
            return response
        saved_pilots = self.pilot_list()
        saved_id = request.GET.get("pilot")
        snapshot_id = request.GET.get("snapshot")
        if saved_id or snapshot_id:
            try:
                if snapshot_id:
                    snapshot = PilotSnapshot.objects.select_related("pilot").get(pk=uuid.UUID(snapshot_id))
                    if saved_id and str(snapshot.pilot_id) != saved_id:
                        raise Http404("Snapshot does not belong to this pilot")
                else:
                    try:
                        pilot = MigrationPilot.objects.get(pk=uuid.UUID(saved_id))
                        snapshot = pilot.snapshots.first()
                        if snapshot is None:
                            raise Http404("Pilot has no snapshots")
                    except MigrationPilot.DoesNotExist:
                        # Existing snapshot links from the first local release remain valid.
                        snapshot = PilotSnapshot.objects.select_related("pilot").get(pk=uuid.UUID(saved_id))
            except (ValueError, PilotSnapshot.DoesNotExist):
                raise Http404("Pilot report not found")
            if request.GET.get("download") == "1":
                response = HttpResponse(json.dumps(snapshot.report, sort_keys=True, indent=2) + "\n",
                                        content_type="application/json")
                response["Content-Disposition"] = f'attachment; filename="platform-ipam-pilot-{snapshot.id}.json"'
                return response
            report = snapshot.report
            link_existing_prefixes(report, request)
            prepare_pilot_view(report)
            return render(request, self.template_name,
                          {"mode": "pilot", "report": report, "snapshot": snapshot,
                           "selected_pilot": snapshot.pilot,
                           "saved_pilots": saved_pilots})
        large_sample = request.GET.get("sample") == "100"
        filename = "sample_100_pilot.json" if large_sample else "sample_pilot.json"
        sample = json.loads(Path(__file__).with_name(filename).read_text(encoding="utf-8"))
        link_existing_prefixes(sample, request)
        prepare_pilot_view(sample)
        return render(request, self.template_name,
                      {"mode": "pilot", "report": sample, "sample": True,
                       "large_sample": large_sample, "saved_pilots": saved_pilots})

    def post(self, request):
        mode = request.POST.get("mode")
        if mode not in REQUIRED_UPLOADS:
            return self.upload_error(request, mode, "Select a report.")

        try:
            with tempfile.TemporaryDirectory(prefix="platform-ipam-workspace-") as directory:
                root = Path(directory)
                total = 0
                uploaded_names = set()
                bundle = request.FILES.get("bundle")
                selected = {key: uploaded for key in INPUT_FILES
                            if (uploaded := request.FILES.get(key)) is not None}
                if bundle:
                    if mode != "pilot":
                        raise ValueError("ZIP upload is available for Migration pilot overview only.")
                    if selected:
                        raise ValueError("Choose either a ZIP or individual files, not both.")
                    if bundle.size > MAX_FILE_BYTES:
                        raise ValueError("ZIP exceeds the 16 MiB file limit.")
                    with ZipFile(bundle) as archive:
                        allowed_names = set(INPUT_FILES.values())
                        for member in archive.infolist():
                            if member.is_dir():
                                continue
                            parts = Path(member.filename).parts
                            if ".." in parts or Path(member.filename).is_absolute():
                                raise ValueError("ZIP contains an unsafe path.")
                            name = parts[-1]
                            if name == "README.txt":
                                continue
                            if name not in allowed_names or name in uploaded_names:
                                raise ValueError(f"ZIP contains an unexpected or repeated file: {name}")
                            uploaded_names.add(name)
                            if member.file_size > MAX_FILE_BYTES:
                                raise ValueError(f"{name} exceeds the 16 MiB file limit")
                            with archive.open(member) as source, (root / name).open("wb") as target:
                                file_total = 0
                                while chunk := source.read(1024 * 1024):
                                    file_total += len(chunk)
                                    total += len(chunk)
                                    if file_total > MAX_FILE_BYTES:
                                        raise ValueError(f"{name} exceeds the 16 MiB file limit")
                                    if total > MAX_TOTAL_BYTES:
                                        raise ValueError("uploads exceed the 64 MiB total limit")
                                    target.write(chunk)
                else:
                    for key, uploaded in selected.items():
                        name = INPUT_FILES[key]
                        uploaded_names.add(name)
                        if uploaded.size > MAX_FILE_BYTES:
                            raise ValueError(f"{name} exceeds the 16 MiB file limit")
                        with (root / name).open("wb") as target:
                            for chunk in uploaded.chunks():
                                total += len(chunk)
                                if total > MAX_TOTAL_BYTES:
                                    raise ValueError("uploads exceed the 64 MiB total limit")
                                target.write(chunk)
                missing = [INPUT_FILES[key] for key in REQUIRED_UPLOADS[mode]
                           if INPUT_FILES[key] not in uploaded_names]
                if missing:
                    raise ValueError(f"{mode.title()} report needs: {', '.join(missing)}. Drop the complete ZIP or select those files.")

                if mode == "topology":
                    report = json.loads((root / "topology.json").read_text(encoding="utf-8"))
                    if not isinstance(report, dict) or report.get("version") != 1 or not isinstance(report.get("cells"), list) or not isinstance(report.get("gaps"), list) or not isinstance(report.get("complete"), bool):
                        raise ValueError("Unsupported topology report schema.")
                    for cell in report["cells"]:
                        if not isinstance(cell, dict) or not isinstance(cell.get("gaps"), list) or not isinstance(cell.get("complete"), bool):
                            raise ValueError("Unsupported topology cell schema.")
                        if cell["complete"] != (not cell["gaps"]):
                            raise ValueError("Topology cell coverage is inconsistent.")
                    if report["complete"] != (bool(report["cells"]) and not report["gaps"] and all(cell["complete"] for cell in report["cells"])):
                        raise ValueError("Topology report coverage is inconsistent.")
                    body = json.dumps(report, sort_keys=True, indent=2) + "\n"
                elif mode in ("address", "pilot"):
                    command = ["python3", "/opt/platform-ipam/address-plan.py",
                               "--inventory", str(root), "--matrix", str(root / "matrix.yaml"),
                               "--protected", str(root / "fixed.yaml"),
                               "--approved-plan", str(root / "approved-plan.json"),
                               "--pilot-scope", str(root / "pilot-scope.json"),
                               "--binary", "/usr/local/bin/platform-ipam"]
                    if "migration.yaml" in uploaded_names:
                        command += ["--migration-plan", str(root / "migration.yaml")]
                    result = subprocess.run(
                        command, cwd=root, env={"PATH": "/usr/local/bin:/usr/bin:/bin"},
                        capture_output=True, text=True, timeout=180, check=False,
                    )
                    if result.returncode not in (0, 3):
                        raise ValueError(result.stderr.strip()[:1000] or "No address plan could be produced.")
                    body = result.stdout
                    report = json.loads(body)
                    if mode == "pilot":
                        (root / "address.json").write_text(body, encoding="utf-8")
                        progress_command = ["/usr/local/bin/platform-ipam", "onboard", "progress",
                                            "--plan", str(root / "migration.yaml"), "--inventory", str(root),
                                            "--matrix", str(root / "matrix.yaml"), "--fixed", str(root / "fixed.yaml"),
                                            "--format", "json"]
                        if "ownership.yaml" in uploaded_names:
                            progress_command += ["--ownership", str(root / "ownership.yaml")]
                        if "allocations.json" in uploaded_names:
                            progress_command += ["--allocations", str(root / "allocations.json")]
                        progress_result = subprocess.run(
                            progress_command, cwd=root, env={"PATH": "/usr/local/bin:/usr/bin:/bin"},
                            capture_output=True, text=True, timeout=60, check=False,
                        )
                        if progress_result.returncode not in (0, 3):
                            raise ValueError(progress_result.stderr.strip()[:1000] or "No migration progress report could be produced.")
                        (root / "progress.json").write_text(progress_result.stdout, encoding="utf-8")
                        assessment_command = ["/usr/local/bin/platform-ipam", "onboard", "assess",
                                              "--inventory", str(root), "--matrix", str(root / "matrix.yaml"),
                                              "--fixed", str(root / "fixed.yaml"), "--format", "json"]
                        if "ownership.yaml" in uploaded_names:
                            assessment_command += ["--ownership", str(root / "ownership.yaml")]
                        assessment_result = subprocess.run(
                            assessment_command, cwd=root, env={"PATH": "/usr/local/bin:/usr/bin:/bin"},
                            capture_output=True, text=True, timeout=60, check=False,
                        )
                        if assessment_result.returncode not in (0, 3):
                            raise ValueError(assessment_result.stderr.strip()[:1000] or "No full overlap assessment could be produced.")
                        (root / "assessment.json").write_text(assessment_result.stdout, encoding="utf-8")
                        overview_command = ["python3", "/opt/platform-ipam/pilot-evidence.py",
                                            "--address", str(root / "address.json"),
                                            "--progress", str(root / "progress.json"),
                                            "--assessment", str(root / "assessment.json"),
                                            "--run", str(root / "run.json"),
                                            "--networks", str(root / "networks.csv")]
                        if "verification.json" in uploaded_names:
                            overview_command += ["--verification", str(root / "verification.json")]
                        if "execution.json" in uploaded_names:
                            overview_command += ["--execution", str(root / "execution.json")]
                        overview = subprocess.run(
                            overview_command,
                            cwd=root, env={"PATH": "/usr/local/bin:/usr/bin:/bin"},
                            capture_output=True, text=True, timeout=30, check=False,
                        )
                        if overview.returncode != 0:
                            raise ValueError(overview.stderr.strip()[:1000] or "No pilot overview could be produced.")
                        body = overview.stdout
                        report = json.loads(body)
                else:
                    command = ["/usr/local/bin/platform-ipam", "onboard", mode]
                    if mode == "progress":
                        command += ["--plan", str(root / "migration.yaml")]
                    command += ["--inventory", str(root), "--format", "json"]
                    for key in OPTION_FLAGS:
                        if INPUT_FILES[key] in uploaded_names:
                            command += ["--" + key, str(root / INPUT_FILES[key])]
                    if mode == "progress" and "allocations.json" in uploaded_names:
                        command += ["--allocations", str(root / INPUT_FILES["allocations"])]
                    result = subprocess.run(
                        command,
                        cwd=root,
                        env={"PATH": "/usr/local/bin:/usr/bin:/bin"},
                        capture_output=True,
                        text=True,
                        timeout=60,
                        check=False,
                    )
                    if result.returncode not in (0, 3):
                        raise ValueError(result.stderr.strip()[:1000] or "No report could be produced.")
                    body = result.stdout
                    report = json.loads(body)
        except (ValueError, BadZipFile, RuntimeError, NotImplementedError,
                json.JSONDecodeError, subprocess.TimeoutExpired, OSError) as error:
            return self.upload_error(request, mode, str(error))

        if request.POST.get("download") == "1":
            response = HttpResponse(body, content_type="application/json")
            response["Content-Disposition"] = f'attachment; filename="platform-ipam-{mode}.json"'
            return response
        snapshot = None
        if mode == "pilot" and request.POST.get("save_snapshot") == "1":
            name = request.POST.get("pilot_name", "").strip()
            existing_id = request.POST.get("pilot_id", "").strip()
            if existing_id:
                try:
                    pilot = MigrationPilot.objects.get(pk=uuid.UUID(existing_id))
                except (ValueError, MigrationPilot.DoesNotExist):
                    return self.upload_error(request, mode, "Selected pilot does not exist.")
                if pilot.owner != (report.get("pilot_owner") or "unknown"):
                    return self.upload_error(request, mode, "Pilot owner differs from the selected pilot. Start a new pilot or correct the input.")
            elif not name or len(name) > 160:
                return self.upload_error(request, mode, "Saved pilot name must be 1 to 160 characters.")
            else:
                pilot = MigrationPilot.objects.create(
                    name=name, owner=report.get("pilot_owner") or "unknown", created_by=request.user)
            snapshot = PilotSnapshot.objects.create(
                name=pilot.name, pilot=pilot, created_by=request.user,
                report_sha256=hashlib.sha256(body.encode("utf-8")).hexdigest(), report=report)
        link_existing_prefixes(report, request)
        if mode == "pilot":
            prepare_pilot_view(report)
        return render(request, self.template_name,
                      {"mode": mode, "report": report, "snapshot": snapshot,
                       "selected_pilot": snapshot.pilot if snapshot else None,
                       "saved_pilots": self.pilot_list()})
