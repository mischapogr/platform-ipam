#!/usr/bin/env python3
"""Exploratory reservation latency through an isolated real API/NetBox stack."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import importlib.util
import ipaddress
import json
import math
import os
from pathlib import Path
import secrets
import socket
import statistics
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
COMPOSE = ROOT / "deploy/compose"
spec = importlib.util.spec_from_file_location("coordinated_restore", Path(__file__).with_name("run-coordinated-restore.py"))
pair_contract = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pair_contract)


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, help="Write synthetic measurements; never includes credentials")
    args = parser.parse_args()
    project = "platform-ipam-reservation-scale-test-" + secrets.token_hex(5)
    image = "platform-ipam-reservation-scale:" + project.rsplit("-", 1)[-1]
    env = os.environ.copy()
    env.update(COMPOSE_PROJECT_NAME=project, PLATFORM_DB_USER="scale", PLATFORM_DB_NAME="scale",
               PLATFORM_DB_PASSWORD=secrets.token_urlsafe(24), IPAM_ENVIRONMENT="development",
               IPAM_AUTH_MODE="local", IPAM_AWS_MODE="fake", IPAM_LOCAL_SUBJECT="developer",
               IPAM_LOCAL_TOKEN=secrets.token_urlsafe(32), IPAM_OPS_TOKEN=secrets.token_urlsafe(32),
               IPAM_OPERATOR_TOKEN=secrets.token_urlsafe(32), IPAM_NETBOX_URL="http://netbox:8080",
               IPAM_NETBOX_TOKEN=secrets.token_hex(20), IPAM_OIDC_ISSUER="", IPAM_OIDC_AUDIENCE="",
               IPAM_CONFIG_FILE="/etc/platform-ipam/config/pools.yaml", IPAM_LISTEN_ADDR=":8080",
               IPAM_IDENTITY_FILE="/etc/platform-ipam/identity/identities.yaml",
               NETBOX_DB_PASSWORD=secrets.token_urlsafe(24), NETBOX_REDIS_PASSWORD=secrets.token_urlsafe(24),
               NETBOX_CACHE_PASSWORD=secrets.token_urlsafe(24), NETBOX_SECRET_KEY=secrets.token_hex(32),
               NETBOX_API_TOKEN_PEPPER=secrets.token_hex(32), NETBOX_SUPERUSER_NAME="scale-admin",
               NETBOX_SUPERUSER_PASSWORD=secrets.token_urlsafe(32), NETBOX_E2E_VIEWER_TOKEN=secrets.token_hex(20),
               NETBOX_E2E_MAINTAINER_TOKEN=secrets.token_hex(20), NETBOX_UI_PASSWORD=secrets.token_urlsafe(32))
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    env["IPAM_API_PORT"] = str(port)
    with tempfile.TemporaryDirectory(prefix="platform-ipam-reservation-scale-") as directory:
        private = Path(directory)
        original_policy = (COMPOSE / "fixtures/pools.yaml").read_text()
        if original_policy.count("max_unreclaimed_allocations_per_tenant: 32") != 1:
            raise AssertionError("Reviewed development quota fixture changed")
        policy = original_policy.replace("max_unreclaimed_allocations_per_tenant: 32",
                                         "max_unreclaimed_allocations_per_tenant: 64")
        policy = policy.replace("policy_version: development-1", "policy_version: scale-local-1")
        policy_file = private / "pools.yaml"
        policy_file.write_text(policy)
        services = {name: {"image": image, "volumes": [str(policy_file) + ":/etc/platform-ipam/config/pools.yaml:ro"]}
                    for name in ("api", "worker", "migrate")}
        services["netbox"] = {"healthcheck": {"start_period": "600s"}}
        overlay = private / "scale.json"
        overlay.write_text(json.dumps({"services": services}))
        overlay.chmod(0o600)
        argv = ["docker", "compose", "--project-name", project, "--env-file", str(COMPOSE / ".env.example"),
                "-f", str(COMPOSE / "compose.yaml"), "-f", str(COMPOSE / "compose.netbox.yaml"), "-f", str(overlay)]

        def execute(*parts, data=None, timeout=240):
            process = subprocess.run(argv + list(parts), cwd=COMPOSE, env=env, input=data,
                                     capture_output=True, timeout=timeout)
            if process.returncode:
                raise RuntimeError(f"Compose {parts[0]} failed with exit {process.returncode}")
            return process.stdout

        def sql(service, database, query):
            user = "scale" if service == "platform-db" else "netbox"
            return execute("exec", "-T", service, "psql", "-U", user, "-d", database,
                           "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", data=query.encode()).decode().strip()

        def api(path, method="GET", body=None, token=None, key=None):
            headers = {}
            if token:
                headers["Authorization"] = "Bearer " + token
            if key:
                headers["Idempotency-Key"] = key.replace("/", "-")
            if body is not None:
                headers["Content-Type"] = "application/json"
            request = urllib.request.Request(f"http://127.0.0.1:{port}" + path, method=method, headers=headers,
                        data=json.dumps(body).encode() if body is not None else None)
            try:
                response = urllib.request.urlopen(request, timeout=60)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return response.status, json.loads(response.read()), response.headers.get("Retry-After")

        def reserve(key):
            started = time.perf_counter()
            polls, retries = 0, 0
            retry_codes = {}
            backoff_seconds = 0.0
            body = {"allocation_key": key, "scope": "vpc", "environment": "development",
                    "region": "eu-central-1", "account_id": "000000000000", "prefix_length": 22,
                    "description": "Synthetic reservation latency measurement", "labels": {}}
            deadline = time.monotonic() + 240
            while time.monotonic() < deadline:
                status, allocation, retry_after = api("/v1/allocations", "POST", body, env["IPAM_LOCAL_TOKEN"], key)
                if status in (200, 201):
                    break
                if status == 202:
                    operation_id = allocation["id"]
                    while time.monotonic() < deadline:
                        polls += 1
                        status, operation, _ = api("/v1/operations/" + operation_id, token=env["IPAM_LOCAL_TOKEN"])
                        if status != 200 or operation["status"] == "FAILED":
                            raise AssertionError("Measured reservation operation failed")
                        if operation["status"] == "SUCCEEDED":
                            status, allocation, _ = api("/v1/allocations/" + operation["allocation_id"], token=env["IPAM_LOCAL_TOKEN"])
                            if status != 200:
                                raise AssertionError("Measured committed reservation is unreadable")
                            break
                        time.sleep(2)
                    else:
                        raise AssertionError("Measured reservation operation timed out")
                    break
                if status != 503:
                    raise AssertionError(f"Measured reservation returned unexpected status {status}")
                retries += 1
                code = allocation.get("error", {}).get("code", "missing_code")
                retry_codes[code] = retry_codes.get(code, 0) + 1
                delay = float(retry_after) if retry_after is not None else 2.0
                if delay < 0 or not math.isfinite(delay):
                    raise AssertionError("Invalid numeric Retry-After from measured API")
                backoff_seconds += delay
                time.sleep(delay)
            else:
                raise AssertionError("Measured reservation did not settle")
            duration_ms = (time.perf_counter() - started) * 1000
            if allocation["state"] != "RESERVED" or allocation["policy_version"] != "scale-local-1":
                raise AssertionError("Measurement did not use the isolated committed-reservation policy")
            return {"key": key, "id": allocation["id"], "cidr": allocation["cidr"],
                    "duration_ms": round(duration_ms, 3), "operation_polls": polls, "retry_503": retries,
                    "retry_codes": retry_codes, "retry_503_backoff_seconds": backoff_seconds}

        try:
            print("Building real application and initializing " + project, flush=True)
            execute("build", "api", timeout=600)
            execute("up", "-d", "--wait", "--wait-timeout", "900", "netbox", "platform-db", timeout=960)
            execute("run", "--rm", "--no-deps", "netbox-bootstrap")
            execute("run", "--rm", "--no-deps", "migrate")
            execute("run", "--rm", "--no-deps", "seed")
            execute("up", "-d", "--wait", "--wait-timeout", "180", "api", "worker", "netbox-worker")
            image_ids = {}
            for name in ("api", "worker", "platform-db", "netbox", "netbox-db", "netbox-redis"):
                container = execute("ps", "-q", name).decode().strip()
                image_ids[name] = subprocess.check_output(["docker", "inspect", "--format", "{{.Image}}", container], text=True).strip()
            warmup = reserve("scale/warmup")
            records, phases = [warmup], []
            for clients in (1, 4, 8):
                barrier = threading.Barrier(clients)
                resident_before = len(records)

                def client(index):
                    barrier.wait(timeout=30)
                    return [reserve(f"scale/c{clients}/w{index}/r{attempt}") for attempt in range(4)]

                started = time.perf_counter()
                with ThreadPoolExecutor(max_workers=clients) as pool:
                    batch = [item for series in pool.map(client, range(clients)) for item in series]
                elapsed = time.perf_counter() - started
                durations = sorted(item["duration_ms"] for item in batch)
                retry_codes = {}
                for item in batch:
                    for code, count in item["retry_codes"].items():
                        retry_codes[code] = retry_codes.get(code, 0) + count
                phases.append({"clients": clients, "requests": len(batch), "resident_before": resident_before,
                    "resident_after": resident_before + len(batch), "elapsed_seconds": round(elapsed, 3),
                    "median_ms": round(statistics.median(durations), 3), "p95_ms": durations[math.ceil(0.95 * len(durations)) - 1],
                    "max_ms": max(durations), "reservations_per_second": round(len(batch) / elapsed, 3),
                    "operation_polls": sum(item["operation_polls"] for item in batch),
                    "retry_503": sum(item["retry_503"] for item in batch), "retry_codes": retry_codes,
                    "retry_503_backoff_seconds": sum(item["retry_503_backoff_seconds"] for item in batch)})
                records.extend(batch)
                print(json.dumps(phases[-1], sort_keys=True), flush=True)
            for original in records:
                replay = reserve(original["key"])
                if (replay["id"], replay["cidr"]) != (original["id"], original["cidr"]):
                    raise AssertionError("Measured reservation retry changed identity or CIDR")
            path = "/v1/allocations/" + warmup["id"]
            if api(path)[0] != 401 or api(path, token=env["IPAM_OPS_TOKEN"])[0] != 404:
                raise AssertionError("Measured load changed tenant isolation")
            execute("stop", *pair_contract.WRITERS)
            for service, database in (("platform-db", "scale"), ("netbox-db", "netbox")):
                if sql(service, database, "SELECT current_database();") != database:
                    raise AssertionError("Measurement reached the wrong database")
            ledger = {table: json.loads(sql("platform-db", "scale",
                f"SELECT COALESCE(jsonb_agg(row_data ORDER BY row_data::text), '[]'::jsonb) "
                f"FROM (SELECT to_jsonb(t) AS row_data FROM {table} t) q;")) for table in pair_contract.TABLES}
            prefixes = json.loads(sql("netbox-db", "netbox",
                "SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id), '[]'::jsonb) FROM ipam_prefix t;"))
            pair = {"ledger": ledger, "prefixes": prefixes}
            pair_contract.validate_pair(pair)
            ids = {item["id"] for item in records}
            if len(ids) != 53 or ids != {row["id"] for row in ledger["allocations"]}:
                raise AssertionError("Concurrent measurement duplicated or lost reservations")
            networks = [ipaddress.ip_network(item["cidr"]) for item in records]
            if any(a.overlaps(b) for index, a in enumerate(networks) for b in networks[index + 1:]):
                raise AssertionError("Concurrent measured reservations overlap")
            if len(ledger["audit_events"]) != 106 or len(ledger["idempotency_requests"]) != 53:
                raise AssertionError("Measured retries duplicated audit evidence or retry records")
            if any(len(ledger[table]) != 53 for table in ("allocation_keys", "holds", "operations")):
                raise AssertionError("Measured load left duplicate or orphan operation/key/hold rows")
            sources = [Path(__file__).resolve(), Path(pair_contract.__file__), ROOT / "Dockerfile", ROOT / "go.mod", ROOT / "go.sum",
                       COMPOSE / "compose.yaml", COMPOSE / "compose.netbox.yaml", COMPOSE / "seed-netbox.py",
                       COMPOSE / "fixtures/pools.yaml", COMPOSE / "fixtures/identities.yaml", COMPOSE / "fixtures/cloud.json"]
            result = {"status": "PASSED", "completed_at": datetime.now(timezone.utc).isoformat(), "project": project,
                "phases": phases, "samples": records[1:], "warmup": warmup, "image_ids": image_ids,
                "host": {"cpu_count": os.cpu_count(), "architecture": os.uname().machine},
                "fixture": {"policy_version": "scale-local-1", "quota": 64, "policy_sha256": hashlib.sha256(policy.encode()).hexdigest(),
                            "api_replicas": 1, "worker_replicas": 1, "requests_per_client": 4,
                            "poll_interval_seconds": 2, "retry_policy": "honor_numeric_retry_after_seconds",
                            "retry_503_fallback_seconds": 2},
                "verified_rows": {table: len(rows) for table, rows in ledger.items()}, "verified_prefixes": len(prefixes),
                "verified_pair_sha256": hashlib.sha256(json.dumps(pair, sort_keys=True).encode()).hexdigest(),
                "checks": ["53_distinct_disjoint_committed_reservations", "netbox_markers_holds_and_permanent_keys",
                           "all_exact_retries_preserve_identity", "106_audit_events_and_53_retry_records", "tenant_isolation"],
                "source_sha256": {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sources},
                "limits": ["exploratory_single_run_small_samples", "growing_resident_counts_between_phases",
                           "one_tenant_account_region_pool", "development_compose_only", "fake_aws", "no_real_oidc",
                           "not_customer_capacity_or_latency_acceptance", "no_discovery_or_replica_scaling_measurement"]}
            runtime_sources = [p for base in (ROOT / "cmd", ROOT / "internal")
                               for p in base.rglob("*.go") if not p.name.endswith("_test.go")]
            result["runtime_go_source_sha256"] = {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest()
                                                   for p in sorted(runtime_sources)}
        finally:
            execute("down", "--volumes", "--remove-orphans")
            ownership = "label=com.docker.compose.project=" + project
            volumes = subprocess.check_output(["docker", "volume", "ls", "-q", "--filter", ownership], text=True).splitlines()
            if volumes:
                subprocess.run(["docker", "volume", "rm", *volumes], capture_output=True, check=True)
            if subprocess.check_output(["docker", "ps", "-aq", "--filter", ownership]).strip() or subprocess.check_output(
                    ["docker", "volume", "ls", "-q", "--filter", ownership]).strip():
                raise RuntimeError("Owned scale measurement resources remain after cleanup")
            subprocess.run(["docker", "image", "rm", image], capture_output=True, check=False)
        result["cleanup"] = "isolated_project_removed"
        if args.out:
            args.out.write_text(json.dumps(result, sort_keys=True, indent=2) + "\n")
        print("PASSED: reservation measurements and persisted-state checks; isolated project removed", flush=True)


if __name__ == "__main__":
    run()
