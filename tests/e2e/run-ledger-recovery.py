#!/usr/bin/env python3
"""Isolated real-binary migration, restart and ledger restore rehearsal."""

import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
COMPOSE = ROOT / "deploy/compose"
TABLES = ("allocations", "allocation_keys", "holds", "audit_events", "idempotency_requests",
          "operations", "observations", "coverage", "findings", "operation_barriers", "platform_ledger_schema")


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, help="Write compact evidence JSON; never retains database dumps")
    parser.add_argument("--upgrade-from", help="Build a reviewed Git commit and test same-schema upgrade/rollback")
    args = parser.parse_args()
    project = "platform-ipam-recovery-test-" + secrets.token_hex(5)
    image = "platform-ipam-recovery:" + project.rsplit("-", 1)[-1]
    previous_image = image + "-previous" if args.upgrade_from else None
    active_image = image
    version_evidence = {}
    env = os.environ.copy()
    env.update(COMPOSE_PROJECT_NAME=project, PLATFORM_DB_USER="recovery", PLATFORM_DB_NAME="recovery",
               PLATFORM_DB_PASSWORD=secrets.token_urlsafe(24), IPAM_ENVIRONMENT="development",
               IPAM_AUTH_MODE="local", IPAM_AWS_MODE="fake", IPAM_LOCAL_SUBJECT="developer",
               IPAM_LOCAL_TOKEN=secrets.token_urlsafe(32), IPAM_OPS_TOKEN=secrets.token_urlsafe(32),
               IPAM_OPERATOR_TOKEN=secrets.token_urlsafe(32), IPAM_NETBOX_URL="http://127.0.0.1:9",
               IPAM_NETBOX_TOKEN="synthetic-unreachable-inventory-token", IPAM_OIDC_ISSUER="", IPAM_OIDC_AUDIENCE="",
               IPAM_CONFIG_FILE="/etc/platform-ipam/config/pools.yaml", IPAM_LISTEN_ADDR=":8080",
               IPAM_IDENTITY_FILE="/etc/platform-ipam/identity/identities.yaml")
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    env["IPAM_API_PORT"] = str(port)
    checks = []
    with tempfile.TemporaryDirectory(prefix="platform-ipam-ledger-recovery-") as directory:
        private = Path(directory)
        overlay = private / "recovery.json"
        database = "recovery"

        def write_overlay(bad_config=False):
            dsn = (f"postgresql://recovery:{env['PLATFORM_DB_PASSWORD']}@platform-db:5432/"
                   f"{database}?sslmode=disable&connect_timeout=2")
            workloads = {"api": {"image": active_image, "environment": {"IPAM_DATABASE_URL": dsn}},
                         "migrate": {"image": active_image, "environment": {"IPAM_DATABASE_URL": dsn}}}
            if bad_config:
                workloads["api"]["environment"]["IPAM_CONFIG_FILE"] = "/missing-reviewed-policy.yaml"
            overlay.write_text(json.dumps({"services": workloads}))
            overlay.chmod(0o600)

        write_overlay()
        argv = ["docker", "compose", "--project-name", project, "--env-file", str(COMPOSE / ".env.example"),
                "-f", str(COMPOSE / "compose.yaml"), "-f", str(overlay)]

        def execute(*parts, data=None, expected=0, timeout=180):
            result = subprocess.run(argv + list(parts), cwd=COMPOSE, env=env, input=data,
                                    capture_output=True, timeout=timeout)
            if (expected == 0 and result.returncode != 0) or (expected == "failure" and result.returncode == 0):
                raise RuntimeError(f"Compose {parts[0]} returned unexpected exit {result.returncode}")
            return result.stdout, result.stderr

        def sql(query):
            output, _ = execute("exec", "-T", "platform-db", "psql", "-U", "recovery", "-d", database,
                                "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", data=query.encode())
            return output.decode().strip()

        def snapshot():
            if sql("SELECT current_database();") != database:
                raise AssertionError("Recovery checks reached the wrong database")
            return {table: json.loads(sql(
                f"SELECT COALESCE(jsonb_agg(row_data ORDER BY row_data::text), '[]'::jsonb) "
                f"FROM (SELECT to_jsonb(t) AS row_data FROM {table} t) q;")) for table in TABLES}

        def request(path, method="GET", data=None, token=None, extra=None):
            headers = dict(extra or {})
            if token:
                headers["Authorization"] = "Bearer " + token
            if data is not None:
                headers["Content-Type"] = "application/json"
            req = urllib.request.Request(f"http://127.0.0.1:{port}" + path, method=method,
                                         headers=headers, data=json.dumps(data).encode() if data is not None else None)
            try:
                response = urllib.request.urlopen(req, timeout=15)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return response.status, json.loads(response.read())

        allocation_path = "/v1/allocations/alloc_recovery_reserved"
        patch_body = {"description": "Authenticated metadata change retained through recovery"}
        patch_headers = {"If-Match": '"1"', "Idempotency-Key": "recovery-metadata-v1"}

        def assert_runtime():
            status, allocation = request(allocation_path, token=env["IPAM_LOCAL_TOKEN"])
            if status != 200 or allocation["revision"] != 2 or allocation["state"] != "RESERVED":
                raise AssertionError("Owning tenant lost its reserved allocation")
            status, replay = request(allocation_path, "PATCH", patch_body, env["IPAM_LOCAL_TOKEN"], patch_headers)
            if status != 200 or replay != allocation:
                raise AssertionError("Metadata retry did not replay the existing revision")
            status, foreign = request(allocation_path, token=env["IPAM_OPS_TOKEN"])
            unknown_status, unknown = request("/v1/allocations/not-present", token=env["IPAM_OPS_TOKEN"])
            foreign_error = {key: value for key, value in foreign.get("error", {}).items() if key != "request_id"}
            unknown_error = {key: value for key, value in unknown.get("error", {}).items() if key != "request_id"}
            if status != 404 or unknown_status != 404 or foreign_error != unknown_error:
                raise AssertionError("Recovery changed foreign-tenant object privacy")
            status, _ = request(allocation_path)
            if status != 401:
                raise AssertionError("Recovery admitted an unauthenticated read")
            status, _ = request(allocation_path, "PATCH", patch_body, env["IPAM_OPERATOR_TOKEN"], patch_headers)
            if status != 404:
                raise AssertionError("Read-only operator modified an allocation")
            if snapshot() != baseline:
                raise AssertionError("Recovery changed durable holds, keys, retry identity or audit history")

        try:
            print("Building real application image for " + project, flush=True)
            execute("build", "api", timeout=600)
            if previous_image:
                commit = subprocess.check_output(
                    ["git", "rev-parse", "--verify", args.upgrade_from + "^{commit}"], cwd=ROOT, text=True).strip()
                archive = subprocess.check_output(["git", "archive", "--format=tar", commit], cwd=ROOT)
                previous_source = private / "previous-source"
                previous_source.mkdir()
                with tarfile.open(fileobj=io.BytesIO(archive)) as contents:
                    contents.extractall(previous_source, filter="data")
                print("Building reviewed baseline " + commit[:12], flush=True)
                built = subprocess.run(["docker", "build", "--target", "release", "-t", previous_image,
                                        str(previous_source)], capture_output=True, timeout=600)
                if built.returncode != 0:
                    raise RuntimeError("Reviewed baseline image build failed")
                image_ids = {name: subprocess.check_output(
                    ["docker", "image", "inspect", "--format", "{{.Id}}", tag], text=True).strip()
                    for name, tag in (("previous", previous_image), ("candidate", image))}
                if image_ids["previous"] == image_ids["candidate"]:
                    raise AssertionError("Upgrade rehearsal requires two distinct image identities")
                version_evidence = {"previous_commit": commit, "previous_source_archive_sha256": hashlib.sha256(archive).hexdigest(),
                                    "candidate_commit": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
                                    "image_ids": image_ids}
                active_image = previous_image
                write_overlay()
            execute("up", "-d", "--wait", "platform-db")
            output, error = execute("run", "--rm", "--no-deps", "api", expected="failure")
            if b"ledger schema is not ready" not in output + error:
                raise AssertionError("Unmigrated API failed for an unexpected reason")
            checks.append("unmigrated_api_refused")
            execute("up", "-d", "--wait", "--wait-timeout", "120", "api")
            print("Real migration completed before API readiness", flush=True)
            now = datetime.now(timezone.utc).isoformat()
            for index, state in enumerate(("RESERVED", "QUARANTINED", "RELEASED")):
                label = state.lower()
                allocation = {"id": "alloc_recovery_" + label, "tenant_id": "developer",
                              "allocation_key": "recovery/" + label, "domain_id": "local-development",
                              "pool_id": "pool_dev_euc1", "scope": "vpc", "environment": "development",
                              "region": "eu-central-1", "account_id": "000000000000", "address_family": "ipv4",
                              "prefix_length": 22, "cidr": f"10.64.{index * 4}.0/22", "state": state,
                              "revision": 1, "committed": True, "created_at": now, "updated_at": now,
                              "inventory_id": "synthetic-" + label, "inventory_sync": "SYNCED",
                              "policy_version": "development-1", "description": "Synthetic recovery fixture",
                              "labels": {}, "release_blockers": []}
                payload = json.dumps(allocation).replace("'", "''")
                sql(f"BEGIN; INSERT INTO allocations(id,tenant_id,allocation_key,committed,payload,created_at) "
                    f"VALUES('{allocation['id']}','developer','{allocation['allocation_key']}',true,'{payload}','{now}'); "
                    f"INSERT INTO allocation_keys VALUES('developer','{allocation['allocation_key']}',"
                    f"'{allocation['id']}',{str(state == 'RELEASED').lower()},'{payload}'); "
                    f"INSERT INTO holds VALUES('hold_{allocation['id']}','{allocation['id']}','developer',"
                    f"'local-development','{allocation['cidr']}',{str(state != 'RELEASED').lower()},'{payload}'); COMMIT;")
            status, _ = request(allocation_path, "PATCH", patch_body, env["IPAM_LOCAL_TOKEN"], patch_headers)
            if status != 200:
                raise AssertionError("Authenticated metadata PATCH failed")
            baseline = snapshot()
            if len(baseline["audit_events"]) != 1 or len(baseline["idempotency_requests"]) != 1:
                raise AssertionError("PATCH did not preserve exactly one event and retry record")
            event = baseline["audit_events"][0]["payload"]
            if event["Action"] != "PATCH" or event["Actor"] != "developer" or event["Revision"] != 2:
                raise AssertionError("Reservation metadata audit provenance differs")
            assert_runtime()
            checks.append("authenticated_patch_and_single_event_retry")
            if previous_image:
                execute("stop", "api")
                active_image = image
                write_overlay()
                execute("run", "--rm", "--no-deps", "migrate")
                execute("up", "-d", "--no-deps", "--wait", "api")
                assert_runtime()
                checks.append("reviewed_binary_upgrade_preserves_schema_ledger_retry_and_authorization")
                print("Reviewed baseline to candidate upgrade passed with unchanged schema and ledger", flush=True)
            execute("kill", "-s", "SIGKILL", "api")
            execute("up", "-d", "--no-deps", "--wait", "api")
            assert_runtime()
            checks.append("api_crash_recovery")
            execute("stop", "platform-db")
            if request("/readyz")[0] != 503:
                raise AssertionError("API remained ready during ledger outage")
            execute("start", "--wait", "platform-db")
            if request("/readyz")[0] != 200:
                raise AssertionError("API did not recover after ledger restart")
            assert_runtime()
            checks.append("database_outage_readiness_and_recovery")
            execute("run", "--rm", "--no-deps", "-e",
                    "IPAM_DATABASE_URL=postgresql://invalid:invalid@platform-db:5432/missing?sslmode=disable&connect_timeout=2",
                    "migrate", expected="failure")
            assert_runtime()
            execute("run", "--rm", "--no-deps", "migrate")
            assert_runtime()
            checks.append("failed_and_idempotent_migration_preserve_ledger")
            write_overlay(bad_config=True)
            execute("up", "-d", "--no-deps", "--force-recreate", "api")
            container, _ = execute("ps", "-a", "-q", "api")
            for _ in range(30):
                state = subprocess.check_output(["docker", "inspect", "--format", "{{.State.Status}}", container.decode().strip()], text=True).strip()
                if state == "exited":
                    break
                time.sleep(1)
            else:
                raise AssertionError("Invalid policy candidate did not fail closed")
            logs, error = execute("logs", "--no-color", "--tail", "5", "api")
            if b"missing-reviewed-policy.yaml" not in logs + error:
                raise AssertionError("Invalid policy candidate failed for an unexpected reason")
            write_overlay()
            execute("up", "-d", "--no-deps", "--wait", "api")
            assert_runtime()
            checks.append("invalid_policy_rollout_and_same_image_rollback")
            print("Crash, outage, migration and policy rollback checks passed", flush=True)
            execute("stop", "api")
            backup, _ = execute("exec", "-T", "platform-db", "pg_dump", "-U", "recovery", "-d", database, "--format=custom")
            dump = private / "ledger.dump"
            dump.write_bytes(backup)
            dump.chmod(0o600)
            execute("exec", "-T", "platform-db", "createdb", "-U", "recovery", "recovery_restored")
            execute("exec", "-T", "platform-db", "pg_restore", "-U", "recovery", "--exit-on-error", "--no-owner", "-d", "recovery_restored", data=backup)
            database = "recovery_restored"
            write_overlay()
            execute("run", "--rm", "--no-deps", "migrate")
            execute("up", "-d", "--no-deps", "--wait", "api")
            assert_runtime()
            checks.append("restored_database_holds_keys_audit_retry_and_authorization")
            if previous_image:
                execute("stop", "api")
                if [row["version"] for row in baseline["platform_ledger_schema"]] != [1]:
                    raise AssertionError("This rehearsal only supports the reviewed version-1 schema")
                sql("UPDATE platform_ledger_schema SET version=999 WHERE version=1;")
                unsupported = snapshot()
                for active_image in (previous_image, image):
                    write_overlay()
                    output, error = execute("run", "--rm", "--no-deps", "api", expected="failure")
                    if b"ledger schema is not ready" not in output + error or snapshot() != unsupported:
                        raise AssertionError("Unsupported schema was admitted or altered by a binary")
                sql("UPDATE platform_ledger_schema SET version=1 WHERE version=999;")
                if snapshot() != baseline:
                    raise AssertionError("Schema refusal check did not preserve the recovered ledger")
                checks.append("both_binaries_refuse_missing_supported_schema_without_mutation")
                active_image = previous_image
                write_overlay()
                execute("run", "--rm", "--no-deps", "migrate")
                execute("up", "-d", "--no-deps", "--wait", "api")
                assert_runtime()
                checks.append("previous_binary_rollback_on_candidate_restored_database")
                execute("stop", "api")
                active_image = image
                write_overlay()
                execute("run", "--rm", "--no-deps", "migrate")
                execute("up", "-d", "--no-deps", "--wait", "api")
                assert_runtime()
                checks.append("candidate_reupgrade_after_previous_binary_rollback")
                print("Both binary schema refusals, restored-ledger rollback and re-upgrade passed", flush=True)
            result = {"status": "PASSED", "completed_at": datetime.now(timezone.utc).isoformat(), "project": project,
                      "source_database": "recovery", "restored_database": database, "checks": checks,
                      "image_id": subprocess.check_output(["docker", "image", "inspect", "--format", "{{.Id}}", image], text=True).strip(),
                      "row_counts": {table: len(rows) for table, rows in baseline.items()},
                      "ledger_sha256": hashlib.sha256(json.dumps(baseline, sort_keys=True).encode()).hexdigest(),
                      "audit_event": event, "backup_bytes": len(backup), "backup_sha256": hashlib.sha256(backup).hexdigest(),
                      "limits": ["synthetic_seeded_allocations", "no_netbox_inventory_restore", "no_real_oidc",
                                 "development_compose_only", "same_binary_configuration_rollback", "no_live_aws"]}
            sources = [Path(__file__).resolve(), ROOT / "Dockerfile", ROOT / "cmd/platform-ipam/main.go",
                       ROOT / "internal/storage/postgres.go", ROOT / "internal/config/config.go",
                       ROOT / "internal/transport/http.go", ROOT / "deploy/compose/compose.yaml"]
            result["source_sha256"] = {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sources}
            if previous_image:
                result["version_rehearsal"] = version_evidence
                result["limits"].remove("same_binary_configuration_rollback")
                result["limits"].extend(["same_schema_binary_upgrade_only", "not_a_forward_schema_migration",
                                         "current_development_configuration_with_both_images"])
                runtime_sources = [ROOT / "go.mod", ROOT / "go.sum", ROOT / "Dockerfile"]
                runtime_sources += [path for base in (ROOT / "cmd", ROOT / "internal")
                                    for path in base.rglob("*.go") if not path.name.endswith("_test.go")]
                result["candidate_runtime_source_sha256"] = {
                    str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(runtime_sources)}
        finally:
            execute("down", "--volumes", "--remove-orphans")
            for tag in (image, previous_image):
                if tag:
                    subprocess.run(["docker", "image", "rm", tag], capture_output=True, check=False)
        result["cleanup"] = "isolated_project_removed"
        if args.out:
            args.out.write_text(json.dumps(result, sort_keys=True, indent=2) + "\n")
        print(json.dumps(result, sort_keys=True), flush=True)


if __name__ == "__main__":
    run()
