#!/usr/bin/env python3
"""Opt-in isolated PostgreSQL dump/restore for operator pilot snapshots."""

import argparse
import base64
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
COMPOSE = ROOT / "deploy/compose"


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, help="Write compact qualification JSON; never retains the database dump")
    args = parser.parse_args()
    project = "platform-ipam-restore-test-" + secrets.token_hex(5)
    image = "platform-ipam-workspace-restore:" + project.rsplit("-", 1)[-1]
    env = os.environ.copy()
    env.update(COMPOSE_PROJECT_NAME=project, NETBOX_UI_USER="restore-operator",
               NETBOX_UI_PASSWORD=secrets.token_urlsafe(32))
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    env["NETBOX_PORT"] = str(port)
    with tempfile.TemporaryDirectory(prefix="platform-ipam-workspace-restore-") as directory:
        private = Path(directory)
        overlay = private / "restore.json"
        netbox_override = {"image": image, "healthcheck": {"start_period": "600s"}}
        overlay.write_text(json.dumps({"services": {
            "netbox": netbox_override, "netbox-worker": {"image": image},
        }}))
        argv = ["docker", "compose", "--project-name", project,
                "--env-file", str(COMPOSE / ".env")]
        for filename in ("compose.yaml", "compose.netbox.yaml", "compose.netbox-workspace.yaml"):
            argv += ["-f", str(COMPOSE / filename)]
        argv += ["-f", str(overlay)]

        def execute(*args, data=None, timeout=300):
            result = subprocess.run(argv + list(args), cwd=COMPOSE, env=env,
                                    input=data, capture_output=True, timeout=timeout)
            if result.returncode:
                # Report bounded stderr only; never print rendered config or env.
                raise RuntimeError(f"Compose {args[0]} failed: " + result.stderr.decode(errors="replace")[-2000:])
            return result.stdout

        def probe(phase):
            source = Path(__file__).with_name("workspace_restore_probe.py").read_text()
            output = execute("exec", "-T", "netbox", "python", "/opt/netbox/netbox/manage.py",
                             "shell", data=(f"REHEARSAL_PHASE = {phase!r}\n" + source).encode())
            rows = [line for line in output.decode().splitlines() if line.startswith("RESTORE_PROBE=")]
            if len(rows) != 1:
                raise RuntimeError("Workspace probe did not return one result")
            return json.loads(rows[0].split("=", 1)[1])

        try:
            print("Building current workspace image for " + project, flush=True)
            execute("build", "netbox", timeout=600)
            print("Starting isolated NetBox dependencies and applying migrations", flush=True)
            execute("up", "-d", "--wait", "--wait-timeout", "900", "netbox", timeout=960)
            before = probe("seed")
            print("Source snapshot, access and corruption checks passed", flush=True)
            # Quiesce the only NetBox writer. No platform API/worker is started.
            execute("stop", "netbox")
            backup = execute("exec", "-T", "netbox-db", "pg_dump", "-U", "netbox",
                             "-d", "netbox", "--format=custom")
            backup_path = private / "netbox.dump"
            backup_path.write_bytes(backup)
            backup_path.chmod(0o600)
            execute("exec", "-T", "netbox-db", "createdb", "-U", "netbox", "netbox_restored")
            execute("exec", "-T", "netbox-db", "pg_restore", "-U", "netbox",
                    "--exit-on-error", "--no-owner", "-d", "netbox_restored", data=backup)
            overlay.write_text(json.dumps({"services": {
                "netbox": {**netbox_override, "environment": {"DB_NAME": "netbox_restored"}},
                "netbox-worker": {"image": image},
            }}))
            execute("up", "-d", "--no-deps", "--wait", "--wait-timeout", "240", "netbox")
            after = probe("check")
            if before != after:
                raise AssertionError("Restored report bytes or provenance differ from source")
            execute("up", "-d", "--wait", "--wait-timeout", "60", "ui-proxy")
            auth = base64.b64encode((env["NETBOX_UI_USER"] + ":" + env["NETBOX_UI_PASSWORD"]).encode()).decode()
            path = f"http://127.0.0.1:{port}/plugins/platform-ipam/?snapshot={after['snapshots'][1]['id']}"
            for authenticated in (False, True):
                headers = {"Authorization": "Basic " + auth} if authenticated else {"X-Remote-User": "restore-operator"}
                try:
                    with urllib.request.urlopen(urllib.request.Request(path, headers=headers), timeout=30) as response:
                        status = response.status
                        html = response.read().decode()
                except urllib.error.HTTPError as error:
                    status = error.code
                if status != (200 if authenticated else 401):
                    raise AssertionError(f"Proxy access check returned {status}")
                if authenticated and "00000000-0000-4000-8000-000000000045" not in html:
                    raise AssertionError("Restored allocation reference missing through proxy")
            result = {"status": "PASSED", "project": project, "image": image,
                              "completed_at": datetime.now(timezone.utc).isoformat(),
                              "image_id": subprocess.check_output(
                                  ["docker", "image", "inspect", "--format", "{{.Id}}", image], text=True).strip(),
                              "backup_bytes": len(backup), "backup_sha256": hashlib.sha256(backup).hexdigest(),
                              "evidence": after, "proxy_checks": ["authenticated_render", "forged_header_denied"],
                              "limits": ["synthetic_snapshot_evidence", "no_platform_ledger_restore",
                                         "no_real_identity_provider", "no_production_retention_policy"]}
            plugin = ROOT / "deploy/compose/netbox/ui/platform_ipam_workspace"
            sources = [Path(__file__).resolve(), Path(__file__).resolve().with_name("workspace_restore_probe.py"),
                       ROOT / "deploy/compose/netbox/ui/Dockerfile"]
            sources += list(plugin.rglob("*.py")) + list(plugin.rglob("*.html"))
            result["source_sha256"] = {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
                                       for path in sorted(sources)}
        finally:
            execute("down", "--volumes", "--remove-orphans")
            subprocess.run(["docker", "image", "rm", image], capture_output=True, check=False)
        result["cleanup"] = "isolated_project_removed"
        encoded = json.dumps(result, sort_keys=True, indent=2) + "\n"
        if args.out:
            args.out.write_text(encoded)
        print(json.dumps(result, sort_keys=True), flush=True)


if __name__ == "__main__":
    run()
