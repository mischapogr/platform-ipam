#!/usr/bin/env python3
"""Restore a quiesced real NetBox/ledger pair in an isolated development stack."""

import argparse
from datetime import datetime, timezone
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
COMPOSE = ROOT / "deploy/compose"
TABLES = ("allocations", "allocation_keys", "holds", "audit_events", "idempotency_requests",
          "operations", "observations", "coverage", "findings", "operation_barriers", "platform_ledger_schema")
WRITERS = ("api", "worker", "netbox-worker", "netbox")


def validate_pair(pair):
    """Validate the restored allocation/hold/key/prefix identity before admission."""
    allocations = pair["ledger"]["allocations"]
    prefixes = {str(row["id"]): row for row in pair["prefixes"]}
    holds = {row["allocation_id"]: row for row in pair["ledger"]["holds"]}
    keys = {(row["tenant_id"], row["allocation_key"]): row for row in pair["ledger"]["allocation_keys"]}
    expected = set()
    for row in allocations:
        allocation = row["payload"]
        if not allocation["committed"] or allocation["state"] == "RELEASED":
            raise AssertionError("Rehearsal expected committed, unreclaimed allocations")
        expected.add(allocation["id"])
        prefix = prefixes.get(allocation["inventory_id"])
        if prefix is None or prefix["prefix"] != allocation["cidr"]:
            raise AssertionError("Restored allocation lacks its exact NetBox prefix")
        fields = prefix["custom_field_data"]
        for field, key in (("platform_allocation_id", "id"), ("platform_allocation_key", "allocation_key"),
                           ("platform_tenant_id", "tenant_id"), ("platform_pool_id", "pool_id")):
            if fields.get(field) != allocation[key]:
                raise AssertionError("Restored inventory marker mismatch")
        hold = holds.get(allocation["id"])
        key = keys.get((allocation["tenant_id"], allocation["allocation_key"]))
        if not hold or not hold["committed"] or hold["cidr"] != allocation["cidr"]:
            raise AssertionError("Restored durable hold differs")
        if not key or key["retired"] or key["allocation_id"] != allocation["id"]:
            raise AssertionError("Restored permanent key differs")
    observed = [row["custom_field_data"].get("platform_allocation_id") for row in prefixes.values()
                if row["custom_field_data"].get("platform_allocation_id")]
    if len(observed) != len(expected) or set(observed) != expected:
        raise AssertionError("Restored NetBox contains an orphan or duplicate allocation marker")


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, help="Write compact evidence JSON; never retains database dumps")
    args = parser.parse_args()
    project = "platform-ipam-joint-restore-test-" + secrets.token_hex(5)
    image = "platform-ipam-joint-restore:" + project.rsplit("-", 1)[-1]
    env = os.environ.copy()
    env.update(COMPOSE_PROJECT_NAME=project, PLATFORM_DB_USER="joint", PLATFORM_DB_NAME="joint",
               PLATFORM_DB_PASSWORD=secrets.token_urlsafe(24), IPAM_ENVIRONMENT="development",
               IPAM_AUTH_MODE="local", IPAM_AWS_MODE="fake", IPAM_LOCAL_SUBJECT="developer",
               IPAM_LOCAL_TOKEN=secrets.token_urlsafe(32), IPAM_OPS_TOKEN=secrets.token_urlsafe(32),
               IPAM_OPERATOR_TOKEN=secrets.token_urlsafe(32), IPAM_NETBOX_URL="http://netbox:8080",
               IPAM_NETBOX_TOKEN=secrets.token_hex(20), IPAM_OIDC_ISSUER="", IPAM_OIDC_AUDIENCE="",
               IPAM_CONFIG_FILE="/etc/platform-ipam/config/pools.yaml", IPAM_LISTEN_ADDR=":8080",
               IPAM_IDENTITY_FILE="/etc/platform-ipam/identity/identities.yaml",
               NETBOX_DB_PASSWORD=secrets.token_urlsafe(24), NETBOX_REDIS_PASSWORD=secrets.token_urlsafe(24),
               NETBOX_CACHE_PASSWORD=secrets.token_urlsafe(24), NETBOX_SECRET_KEY=secrets.token_hex(32),
               NETBOX_API_TOKEN_PEPPER=secrets.token_hex(32), NETBOX_SUPERUSER_NAME="joint-admin",
               NETBOX_SUPERUSER_PASSWORD=secrets.token_urlsafe(32), NETBOX_E2E_VIEWER_TOKEN=secrets.token_hex(20),
               NETBOX_E2E_MAINTAINER_TOKEN=secrets.token_hex(20), NETBOX_UI_PASSWORD=secrets.token_urlsafe(32))
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    env["IPAM_API_PORT"] = str(port)
    ledger_db, netbox_db = "joint", "netbox"
    checks = []
    with tempfile.TemporaryDirectory(prefix="platform-ipam-joint-restore-") as directory:
        private = Path(directory)
        overlay = private / "joint.json"

        def write_overlay():
            dsn = f"postgresql://joint:{env['PLATFORM_DB_PASSWORD']}@platform-db:5432/{ledger_db}?sslmode=disable"
            services = {name: {"image": image, "environment": {"IPAM_DATABASE_URL": dsn}}
                        for name in ("api", "worker", "migrate")}
            services["netbox"] = {"environment": {"DB_NAME": netbox_db}, "healthcheck": {"start_period": "600s"}}
            services["netbox-worker"] = {"environment": {"DB_NAME": netbox_db}}
            services["netbox-bootstrap"] = {"environment": {"DB_NAME": netbox_db}}
            overlay.write_text(json.dumps({"services": services}))
            overlay.chmod(0o600)

        write_overlay()
        argv = ["docker", "compose", "--project-name", project, "--env-file", str(COMPOSE / ".env.example"),
                "-f", str(COMPOSE / "compose.yaml"), "-f", str(COMPOSE / "compose.netbox.yaml"), "-f", str(overlay)]

        def execute(*parts, data=None, timeout=240):
            result = subprocess.run(argv + list(parts), cwd=COMPOSE, env=env, input=data,
                                    capture_output=True, timeout=timeout)
            if result.returncode:
                raise RuntimeError(f"Compose {parts[0]} failed with exit {result.returncode}")
            return result.stdout

        def sql(service, database, query):
            user = "joint" if service == "platform-db" else "netbox"
            return execute("exec", "-T", service, "psql", "-U", user, "-d", database,
                           "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", data=query.encode()).decode().strip()

        def read_pair():
            for service, database in (("platform-db", ledger_db), ("netbox-db", netbox_db)):
                if sql(service, database, "SELECT current_database();") != database:
                    raise AssertionError("Restore check reached the wrong database")
            ledger = {table: json.loads(sql("platform-db", ledger_db,
                      f"SELECT COALESCE(jsonb_agg(row_data ORDER BY row_data::text), '[]'::jsonb) "
                      f"FROM (SELECT to_jsonb(t) AS row_data FROM {table} t) q;")) for table in TABLES}
            prefixes = json.loads(sql("netbox-db", netbox_db,
                "SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id), '[]'::jsonb) FROM ipam_prefix t;"))
            return {"ledger": ledger, "prefixes": prefixes}

        def api(path, method="GET", body=None, token=None, headers=None):
            request_headers = dict(headers or {})
            if token:
                request_headers["Authorization"] = "Bearer " + token
            if body is not None:
                request_headers["Content-Type"] = "application/json"
            request = urllib.request.Request(f"http://127.0.0.1:{port}" + path, method=method,
                        headers=request_headers, data=json.dumps(body).encode() if body is not None else None)
            try:
                response = urllib.request.urlopen(request, timeout=40)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return response.status, json.loads(response.read())

        def reserve(key):
            body = {"allocation_key": key, "scope": "vpc", "environment": "development",
                    "region": "eu-central-1", "account_id": "000000000000", "prefix_length": 22,
                    "description": "Local coordinated restore", "labels": {}}
            headers = {"Idempotency-Key": key.replace("/", "-")}
            deadline = time.monotonic() + 150
            while time.monotonic() < deadline:
                status, result = api("/v1/allocations", "POST", body, env["IPAM_LOCAL_TOKEN"], headers)
                if status in (200, 201):
                    return result
                if status == 202:
                    operation_id = result["id"]
                    while time.monotonic() < deadline:
                        status, operation = api("/v1/operations/" + operation_id, token=env["IPAM_LOCAL_TOKEN"])
                        if status != 200 or operation["status"] == "FAILED":
                            raise AssertionError("Reservation operation failed")
                        if operation["status"] == "SUCCEEDED":
                            status, allocation = api("/v1/allocations/" + operation["allocation_id"], token=env["IPAM_LOCAL_TOKEN"])
                            if status != 200:
                                raise AssertionError("Committed reservation is unreadable")
                            return allocation
                        time.sleep(1)
                    break
                if status != 503:
                    raise AssertionError(f"Reservation returned unexpected status {status}")
                time.sleep(2)
            raise AssertionError("Reservation did not settle using the same request identity")

        try:
            print("Building real application image for " + project, flush=True)
            execute("build", "api", timeout=600)
            print("Initializing isolated NetBox and ledger databases", flush=True)
            execute("up", "-d", "--wait", "--wait-timeout", "900", "netbox", "platform-db", timeout=960)
            execute("run", "--rm", "--no-deps", "netbox-bootstrap")
            execute("run", "--rm", "--no-deps", "migrate")
            execute("run", "--rm", "--no-deps", "seed")
            execute("up", "-d", "--wait", "--wait-timeout", "180", "api", "worker", "netbox-worker")
            first = reserve("joint/first")
            second = reserve("joint/second")
            if first["id"] == second["id"] or ipaddress.ip_network(first["cidr"]).overlaps(ipaddress.ip_network(second["cidr"])):
                raise AssertionError("Distinct reservations share an identity or range")
            patch_body = {"description": "Coordinated restore audit evidence"}
            patch_headers = {"If-Match": '"' + str(first["revision"]) + '"', "Idempotency-Key": "joint-patch-first"}
            status, first = api("/v1/allocations/" + first["id"], "PATCH", patch_body, env["IPAM_LOCAL_TOKEN"], patch_headers)
            if status != 200:
                raise AssertionError("Metadata audit mutation failed")
            checks.append("real_api_reservations_netbox_commit_and_audit")
            execute("stop", *WRITERS)
            cutoff = datetime.now(timezone.utc).isoformat()
            baseline = read_pair()
            validate_pair(baseline)
            actions = [row["payload"]["Action"] for row in baseline["ledger"]["audit_events"]]
            if actions.count("RESERVE_PLANNED") != 2 or actions.count("RESERVE_COMMITTED") != 2 or actions.count("PATCH") != 1:
                raise AssertionError("Expected API reservation/metadata audit evidence is absent")
            checks.append("all_writers_quiesced_and_pair_verified")
            print("Two committed reservations and matching NetBox markers verified; writers paused", flush=True)
            backups = {}
            for name, service, source, target, user in (
                    ("ledger", "platform-db", ledger_db, "joint_restored", "joint"),
                    ("netbox", "netbox-db", netbox_db, "netbox_joint_restored", "netbox")):
                data = execute("exec", "-T", service, "pg_dump", "-U", user, "-d", source, "--format=custom")
                dump = private / (name + ".dump")
                dump.write_bytes(data)
                dump.chmod(0o600)
                execute("exec", "-T", service, "createdb", "-U", user, target)
                execute("exec", "-T", service, "pg_restore", "-U", user, "--exit-on-error", "--no-owner", "-d", target, data=data)
                backups[name] = {"source": source, "restored": target, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}
            ledger_db, netbox_db = "joint_restored", "netbox_joint_restored"
            restored = read_pair()
            validate_pair(restored)
            if restored != baseline:
                raise AssertionError("Restored ledger/prefix rows differ from the quiesced pair")
            checks.append("both_restored_databases_match_quiesced_pair")
            inventory_id = baseline["ledger"]["allocations"][0]["payload"]["inventory_id"]
            prefix = next(row for row in baseline["prefixes"] if str(row["id"]) == inventory_id)
            sql("netbox-db", netbox_db, "UPDATE ipam_prefix SET custom_field_data = jsonb_set(custom_field_data, "
                f"'{{platform_allocation_id}}', '\"deliberate-mismatch\"') WHERE id={int(inventory_id)};")
            try:
                validate_pair(read_pair())
            except AssertionError as error:
                if str(error) != "Restored inventory marker mismatch":
                    raise
            else:
                raise AssertionError("Mismatched restore pair was admitted by preflight")
            original_fields = json.dumps(prefix["custom_field_data"]).replace("'", "''")
            sql("netbox-db", netbox_db, f"UPDATE ipam_prefix SET custom_field_data='{original_fields}' WHERE id={int(inventory_id)};")
            if read_pair() != baseline:
                raise AssertionError("Mismatch rehearsal did not restore the original prefix")
            checks.append("inventory_marker_mismatch_refused_before_admission")
            write_overlay()
            execute("up", "-d", "--no-deps", "--wait", "--wait-timeout", "240", "netbox")
            execute("run", "--rm", "--no-deps", "migrate")
            execute("up", "-d", "--no-deps", "--wait", "api")
            for original in (first, second):
                replay = reserve(original["allocation_key"])
                if (replay["id"], replay["cidr"]) != (original["id"], original["cidr"]):
                    raise AssertionError("Restored reservation retry selected a new identity or CIDR")
            status, replay = api("/v1/allocations/" + first["id"], "PATCH", patch_body, env["IPAM_LOCAL_TOKEN"], patch_headers)
            if status != 200 or replay["description"] != patch_body["description"]:
                raise AssertionError("Restored metadata retry failed")
            for original in (first, second):
                path = "/v1/allocations/" + original["id"]
                if api(path)[0] != 401 or api(path, token=env["IPAM_OPS_TOKEN"])[0] != 404:
                    raise AssertionError("Restored tenant isolation changed")
            if api("/v1/allocations/" + first["id"], "PATCH", patch_body, env["IPAM_OPERATOR_TOKEN"], patch_headers)[0] != 404:
                raise AssertionError("Restored read-only operator was allowed to write")
            if read_pair() != baseline:
                raise AssertionError("Restored retries changed ledger or inventory history")
            checks.append("restored_reservation_metadata_retry_and_authorization")
            execute("up", "-d", "--no-deps", "--wait", "worker", "netbox-worker")
            third = reserve("joint/after-restore")
            for original in (first, second):
                if ipaddress.ip_network(third["cidr"]).overlaps(ipaddress.ip_network(original["cidr"])):
                    raise AssertionError("New reservation overlaps a restored durable hold")
            execute("stop", *WRITERS)
            resumed = read_pair()
            validate_pair(resumed)
            source_events = {row["id"]: row for row in baseline["ledger"]["audit_events"]}
            resumed_events = {row["id"]: row for row in resumed["ledger"]["audit_events"]}
            if any(resumed_events.get(key) != value for key, value in source_events.items()):
                raise AssertionError("Resuming writers changed retained audit history")
            if len(resumed["ledger"]["allocations"]) != 3:
                raise AssertionError("Retry or recovery duplicated a logical reservation")
            checks.append("resumed_writers_and_disjoint_new_reservation")
            result = {"status": "PASSED", "completed_at": datetime.now(timezone.utc).isoformat(), "project": project,
                      "cutoff": cutoff, "stopped_writers": list(WRITERS), "checks": checks, "backups": backups,
                      "image_id": subprocess.check_output(["docker", "image", "inspect", "--format", "{{.Id}}", image], text=True).strip(),
                      "source_ledger_rows": {table: len(rows) for table, rows in baseline["ledger"].items()},
                      "source_prefix_rows": len(baseline["prefixes"]),
                      "source_pair_sha256": hashlib.sha256(json.dumps(baseline, sort_keys=True).encode()).hexdigest(),
                      "resumed_allocations": [{"id": item["id"], "key": item["allocation_key"], "cidr": item["cidr"]} for item in (first, second, third)],
                      "retained_audit_events": len(source_events), "resumed_audit_events": len(resumed_events),
                      "limits": ["development_compose_only", "fake_aws_observations", "no_real_oidc",
                                 "queue_and_media_volumes_preserved_not_restored", "no_production_cutover"]}
            sources = [Path(__file__).resolve(), ROOT / "Dockerfile", ROOT / "cmd/platform-ipam/main.go",
                       ROOT / "internal/storage/postgres.go", ROOT / "internal/service/service.go",
                       ROOT / "deploy/compose/compose.yaml", ROOT / "deploy/compose/compose.netbox.yaml",
                       ROOT / "deploy/compose/seed-netbox.py", ROOT / "deploy/compose/fixtures/cloud.json"]
            result["source_sha256"] = {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sources}
        finally:
            execute("down", "--volumes", "--remove-orphans")
            subprocess.run(["docker", "image", "rm", image], capture_output=True, check=False)
        result["cleanup"] = "isolated_project_removed"
        if args.out:
            args.out.write_text(json.dumps(result, sort_keys=True, indent=2) + "\n")
        print(json.dumps(result, sort_keys=True), flush=True)


if __name__ == "__main__":
    run()
