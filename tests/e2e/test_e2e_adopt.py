"""End-to-end tests for `platform-ipam adopt plan|apply` (package F5).

docs/decisions/0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md and
docs/WORK_PLAN.md package F4: an operator turns a reviewed table of existing
(imported) networks into owned allocations by running `adopt plan|apply`
inside the Compose network, sharing the `api` service's own environment --
never through the platform API or through internal/service directly. This
suite drives that real binary (harness.Stack.adopt, modelled on
Stack.onboard) against the real stack: a reviewed table is written into the
`api` container and `platform-ipam adopt plan|apply` is run against it.

Two classes:

`AdoptE2ETest` is the real, green, ordered scenario -- one TestCase whose
test_NN methods build on each other's state (setUpClass prepares the world
once; addClassCleanup restores it) -- for a VPC that has **no subnets**:
plan, apply, RESERVED with NetBox markers and the import tag/batch/source
kept, no unmanaged_occupancy for it while a decoy still has one, the owning
team's POST replay and a prefix-length conflict, tagging -> ACTIVE, a further
apply replaying and writing nothing, the converse of
`test_netbox_holds_no_managed_prefix_the_api_does_not_know`, release into
QUARANTINED with the key retired, worker recovery of a seeded pending ADOPT
operation, and five cheap read-only refusals.

`AdoptVPCWithSubnetsE2ETest` is the scenario package F5 found impossible and
package F7 made real: a VPC **with** a subnet, through the two runs ADR 0010,
`adopt`'s help text and the adoption runbook have always described. Until the
2026-09-20 amendment to ADR 0010 no ordering of `onboard` + `adopt` could
bring a VPC's subnet under management -- adopting the VPC was refused because
its own subnet was "another network" and "another cloud resource", and
adopting the VPC first made `onboard` refuse the subnet as `overlaps-managed`.
F5 pinned both halves of that gap in a class named for it; the two assertions
are deliberately inverted here, so this class is now the evidence that the
gap is closed. It ends with the refusals that had to survive the amendment,
and with the second half of F7: importing a subnet created *after* its VPC
was adopted.
"""

from __future__ import annotations

import hashlib
import ipaddress
import json
import os
import secrets
import time
import unittest
from datetime import datetime, timezone

from harness import ROOT, compose_argv, run, run_key, stack

POOL_ID = "pool_dev_euc1"
DOMAIN_ID = "local-development"
ACCOUNT_ID = "000000000000"
REGION = "eu-central-1"
ENVIRONMENT = "development"
AZ = "euc1-az1"
CLOUD_FIXTURE = ROOT / "deploy/compose/fixtures/cloud.json"

# This module's own quadrant of the pool (pool_dev_euc1 is 10.64.0.0/16,
# deploy/compose/fixtures/pools.yaml): 10.64.128.0/18, sixteen non-overlapping
# /22 slots. test_e2e_import.py already claims the top quarter
# (10.64.192.0/18) for its own imports and ordinary reserve() calls draw from
# the bottom, so this quarter is free. Rotated per run exactly as
# test_e2e_import.py's slot22 is, so repeated iteration against a stack that
# run-e2e.sh has not reset does not collide with a previous run's occupancy.
_QUARTER_BASE_OCTET = 128  # 10.64.128.0/18
_SLOT_COUNT = 16

# Computed once, at import time, rather than inside a function called
# per-slot: harness.run_key's own fallback (f"local{int(time.time())}" when
# IPAM_E2E_RUN_ID is unset -- a bare `python3 -m unittest` run against the
# already-running stack, exactly how this module is iterated per the work
# order) advances every wall-clock second, so recomputing it on every
# slot22() call let two *different* slot indices, evaluated tens of seconds
# apart over a run this long, land on the *same* CIDR by coincidence -- a
# collision this module can afford to fix (it owns _shift), unlike
# test_e2e_import.py's identically-shaped helper, which does not need to
# because it does not spread its slot22() calls out over a run this long.
_RUN_ID = os.environ.get("IPAM_E2E_RUN_ID") or f"local{int(time.time())}"
_SHIFT = int(hashlib.sha256(("adopt-" + _RUN_ID).encode()).hexdigest(), 16) % _SLOT_COUNT

# Slot assignments, all distinct so the three classes never collide with each
# other's address space: 0 vpc (AdoptE2ETest), 1 recovery vpc, 2/3/4 refusal
# fixtures, 5 decoy, 6 domain-busy probe (never imported), 7 the VPC
# AdoptVPCWithSubnetsE2ETest adopts together with its subnet, 8 the read-only
# VPC that class's surviving refusals are planned against, 9 the VPC
# AdoptAbandonE2ETest seeds as a half-converted stuck adoption and later
# re-adopts under the same key, 10 that class's own domain-busy probe (never
# imported, kept separate from slot 6 even though both are never written to,
# simply so each class's own comments stay locally accurate).
SLOT_VPC = 0
SLOT_RECOVERY = 1
SLOT_REFUSAL_WRONG_PREFIX_ID = 2
SLOT_REFUSAL_NO_RESOURCE = 3
SLOT_REFUSAL_SECOND_PREFIX = 4
SLOT_DECOY = 5
SLOT_DOMAIN_BUSY_PROBE = 6
SLOT_VPC_WITH_SUBNET = 7
SLOT_CHILD_REFUSALS = 8
SLOT_ABANDON = 9
SLOT_ABANDON_DOMAIN_PROBE = 10
# Lent to tests/e2e/test_e2e_reservation_stuck.py, which imports slot22 and
# these two indices from here. The pool has no unclaimed region: ordinary
# reserve() calls fill 10.64.0.0/17 from the bottom (up to the tenant's quota of
# 32 allocations), this module owns 10.64.128.0/18 and test_e2e_import.py owns
# 10.64.192.0/18. A fixed CIDR in the lower half works only until enough
# reservations exist to reach it -- which is how that module first failed.
SLOT_RESERVATION_STUCK = 11
SLOT_RESERVATION_STUCK_PROBE = 12
# Lent to tests/e2e/test_e2e_import.py (docs/WORK_PLAN.md package M9b4, ADR
# 0016's removal): that module's own quadrant (10.64.192.0/18) is fully
# claimed by its sixteen existing tests, and test_e2e_reservation_stuck.py's
# own two slots above (13, 14, chosen locally in that module rather than
# registered here) leave this the one remaining unclaimed index in this
# quadrant. `onboard remove` needs no whole /22 of its own -- one lent slot
# is split into two /24 sub-blocks by the borrower, since ADR 0016 imposes no
# minimum prefix length on an imported occupancy.
SLOT_REMOVAL = 15


def slot22(index: int) -> str:
    i = (index + _SHIFT) % _SLOT_COUNT
    third_octet = _QUARTER_BASE_OCTET + i * 4
    return f"10.64.{third_octet}.0/22"


def what_apply_could_change(prefix: dict) -> dict:
    """The parts of a NetBox prefix a replaying `adopt apply` could touch if it
    wrote, and nothing the worker rewrites on its own.

    `last_updated` looks like the obvious "nothing was written" witness and is
    not one: syncProjections (internal/service/worker.go) PATCHes every
    committed allocation's prefix on EVERY worker pass, thirty seconds apart in
    this stack, refreshing platform_last_observed_at. Two reads that happen to
    straddle a pass differ in `last_updated` although apply wrote nothing --
    which is how this comparison failed once, with the two stamps exactly thirty
    seconds apart (the lead's run, 2026-09-20).
    """
    fields = dict(prefix.get("custom_fields") or {})
    fields.pop("platform_last_observed_at", None)
    status = prefix.get("status")
    return {
        "prefix": prefix.get("prefix"),
        "status": status.get("value") if isinstance(status, dict) else status,
        "tags": sorted(tag.get("slug") for tag in prefix.get("tags") or []),
        "custom_fields": fields,
    }


def network(cidr: str) -> ipaddress.IPv4Network:
    return ipaddress.ip_network(cidr, strict=True)


def first24(vpc_cidr: str) -> str:
    return nth24(vpc_cidr, 0)


def nth24(vpc_cidr: str, index: int) -> str:
    """The index-th /24 inside a /22 VPC. subnet_policy.allowed_prefix_lengths
    in deploy/compose/fixtures/pools.yaml is [24, 26], so a /24 is a legal
    subnet of a /22 pool allocation."""
    return str(list(network(vpc_cidr).subnets(new_prefix=24))[index])


def aws_id(kind: str, key: str) -> str:
    return f"{kind}-{hashlib.sha1(key.encode()).hexdigest()[:17]}"


def _networks_csv(rows: list) -> str:
    headers = ["cidr", "account_id", "region", "name", "type"]
    lines = [",".join(headers)]
    for row in rows:
        lines.append(",".join(str(row.get(h, "")) for h in headers))
    return "\n".join(lines) + "\n"


ADOPT_COLUMNS = [
    "tenant_id", "allocation_key", "scope", "environment", "region", "account_id",
    "cidr", "resource_id", "netbox_prefix_id", "parent_allocation_key", "availability_zone_id",
]


def _adopt_csv(rows: list) -> str:
    lines = [",".join(ADOPT_COLUMNS)]
    for row in rows:
        lines.append(",".join(str(row.get(c, "")) for c in ADOPT_COLUMNS))
    return "\n".join(lines) + "\n"


def _sql_str(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def _strip_slog_prefix(stdout: str) -> str:
    """The slog-JSON-lines-then-report stripping test_09's own docstring
    describes, factored out so a caller that expects NO report (a refusal
    before the service was reached) can assert on the remainder without
    parsing it as JSON. See _report_json below for the full explanation."""
    lines = stdout.splitlines()
    i = 0
    while i < len(lines):
        candidate = lines[i].strip()
        if not (candidate.startswith("{") and candidate.endswith("}")):
            break
        try:
            json.loads(candidate)
        except ValueError:
            break
        i += 1
    return "\n".join(lines[i:])


def _report_json(stdout: str) -> dict:
    """Parse `adopt plan|apply`'s JSON report out of stdout.

    cmd/platform-ipam/main.go sets the process-wide slog default to a JSON
    handler on os.Stdout, and `adopt` shares the api/worker start-up path
    (config.Load, hence the "identity's tenant is eligible for no pool"
    warning for ops-observer) *before* ever reaching adoptcmd -- so stdout
    legitimately carries zero or more single-line JSON log records ahead of
    adoptcmd's own pretty-printed (indented, multi-line) report. Strip only
    lines that stand alone as valid JSON; the report's own opening line is
    a bare "{" and does not. `onboard` dispatches before config.Load ever
    runs, so its own stdout needs no such stripping (plain json.loads, as
    test_e2e_import.py already does).
    """
    return json.loads(_strip_slog_prefix(stdout))


class _AdoptFixtureCase(unittest.TestCase):
    """Shared machinery for both test classes in this module: the fake-cloud
    fixture (backed up and restored byte-identically, per class), `onboard`-
    driven imports, and a wait-without-sleeping helper. Carries no test_*
    methods itself, so unittest's loader contributes nothing from it
    directly.
    """

    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()
        cls.original_cloud = CLOUD_FIXTURE.read_bytes()
        cls.original_cloud_hash = hashlib.sha256(cls.original_cloud).hexdigest()
        cls.addClassCleanup(cls._restore_cloud_fixture)
        cls.addClassCleanup(cls._release_everything)
        # Every cloud resource this class's own scenario currently wants
        # observed, keyed by AWS-style resource id -- the fake cloud adapter
        # (internal/cloud/fake.go) re-reads the whole fixture file on every
        # Observe call, so there is no per-resource API: every edit rewrites
        # the complete list.
        cls.cloud_resources: dict = {}
        # Allocation ids this class adopted or seeded, for best-effort
        # release in cleanup regardless of which test failed first -- kept
        # short on purpose (see each class's own quota note).
        cls.to_release: list = []

    @classmethod
    def _restore_cloud_fixture(cls) -> None:
        CLOUD_FIXTURE.write_bytes(cls.original_cloud)
        got = hashlib.sha256(CLOUD_FIXTURE.read_bytes()).hexdigest()
        if got != cls.original_cloud_hash:
            raise AssertionError(
                f"deploy/compose/fixtures/cloud.json was not restored byte-identically: "
                f"expected sha256 {cls.original_cloud_hash}, got {got}"
            )

    @classmethod
    def _release_everything(cls) -> None:
        for allocation_id in cls.to_release:
            try:
                cls.stack.api("DELETE", f"/v1/allocations/{allocation_id}")
            except Exception:
                pass

    # -- cloud fixture helpers ------------------------------------------

    def _flush_cloud(self) -> None:
        payload = {
            "domain_id": DOMAIN_ID,
            "generation": "development-1",
            "complete": True,
            "resources": list(type(self).cloud_resources.values()),
        }
        tmp = CLOUD_FIXTURE.with_suffix(".json.e2e-tmp")
        tmp.write_text(json.dumps(payload), encoding="utf-8")
        tmp.replace(CLOUD_FIXTURE)

    def _put_resource(self, resource: dict) -> None:
        type(self).cloud_resources[resource["id"]] = resource
        self._flush_cloud()

    def _remove_resource(self, resource_id: str) -> None:
        type(self).cloud_resources.pop(resource_id, None)
        self._flush_cloud()

    def _tag_resource(self, resource_id: str, tags: dict) -> None:
        type(self).cloud_resources[resource_id]["tags"] = tags
        self._flush_cloud()

    # -- import helper (mirrors test_e2e_import.py's _import) -----------

    def _import_network(self, cidr: str, kind: str, name: str, batch: str) -> str:
        """parse -> plan -> apply one network row and return its NetBox prefix id."""
        path = f"/tmp/{batch}.json"
        csv_text = _networks_csv([{"cidr": cidr, "account_id": ACCOUNT_ID, "region": REGION,
                                    "name": name, "type": kind}])
        self.stack.onboard("parse", "-", "--out", path, stdin=csv_text)
        self.stack.onboard("plan", path, "--domain", DOMAIN_ID)
        self.stack.onboard("apply", path, "--domain", DOMAIN_ID, "--batch", batch,
                           "--source", "test_e2e_adopt.py")
        prefixes = self.stack.netbox_prefixes()
        self.assertIn(cidr, prefixes, f"import of {cidr} did not produce a NetBox prefix")
        return str(prefixes[cidr]["id"])

    # -- waiting: never a blind sleep -------------------------------------

    def _open_unmanaged_count(self) -> int:
        findings = (self.stack.api("GET", "/v1/findings").body or {}).get("items", [])
        return len([
            f for f in findings
            if f.get("code") == "unmanaged_occupancy" and f.get("status") == "OPEN"
            and f.get("account_id") == ACCOUNT_ID and f.get("region") == REGION
        ])

    def _wait_until(self, predicate, *, timeout: float = 90.0, interval: float = 3.0, desc: str = ""):
        """Poll predicate (a zero-arg callable) until it returns a truthy
        value, or raise. full_scan_interval_seconds is 30 in
        deploy/compose/fixtures/pools.yaml, so three cycles is a generous
        margin without ever sleeping blindly for minutes."""
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            last = predicate()
            if last:
                return last
            time.sleep(interval)
        raise AssertionError(f"condition not reached within {timeout}s ({desc}); last value: {last!r}")

    # -- adopt plan/apply helpers -----------------------------------------

    def _plan_one(self, row: dict, *, expect_exit: int = 3) -> dict:
        path = f"/tmp/{run_key('adopt-plan-one')}.csv"
        csv_text = _adopt_csv([row])
        stdout, _ = self.stack.adopt("plan", path, files={path: csv_text}, expect_exit=expect_exit)
        return _report_json(stdout)

    def _assert_nothing_written(self, key: str) -> None:
        listed = (self.stack.api("GET", "/v1/allocations").body or {}).get("items", [])
        self.assertNotIn(key, {row["allocation_key"] for row in listed})

    def _psql(self, sql: str, *, tuples_only: bool = False) -> str:
        user = self.stack.env.get("PLATFORM_DB_USER", "platform_ipam")
        db = self.stack.env.get("PLATFORM_DB_NAME", "platform_ipam")
        argv = ["psql", "-q", "-v", "ON_ERROR_STOP=1"]
        if tuples_only:
            argv += ["-t", "-A"]
        argv += ["-U", user, "-d", db]
        result = run(compose_argv("exec", "-T", "platform-db", *argv), stdin=sql)
        if result.returncode != 0:
            raise AssertionError(f"psql failed: {result.stderr[:1200]}\nsql: {sql[:1200]}")
        return result.stdout


class AdoptE2ETest(_AdoptFixtureCase):
    """The real scenario, for a VPC with no subnets. Quota: 2 allocations
    total (the vpc, and test_09's seeded recovery vpc)."""

    # =====================================================================
    # 1. import the VPC, observe it untagged; wait for the worker
    # =====================================================================

    def test_01_import_and_observe_the_vpc(self):
        cls = type(self)
        cls.vpc_cidr = slot22(SLOT_VPC)
        cls.vpc_key = run_key("adopt-vpc")
        cls.vpc_resource_id = aws_id("vpc", cls.vpc_key)
        batch = run_key("adopt-vpc-batch")

        baseline = self._open_unmanaged_count()
        cls.vpc_prefix_id = self._import_network(cls.vpc_cidr, "vpc", "adopt-vpc", batch)
        cls.vpc_import_batch = batch

        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "vpc",
            "id": cls.vpc_resource_id, "cidr": cls.vpc_cidr, "cidrs": [cls.vpc_cidr],
            "tags": {},
        })
        # Also seed a decoy resource elsewhere: a plain untagged, never-
        # imported, never-adopted resource, so test_03 can show unmanaged
        # occupancy is still reported for *something* while it stops being
        # reported for the freshly adopted VPC.
        cls.decoy_cidr = slot22(SLOT_DECOY)
        cls.decoy_resource_id = aws_id("vpc", run_key("adopt-decoy"))
        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "vpc",
            "id": cls.decoy_resource_id, "cidr": cls.decoy_cidr, "cidrs": [cls.decoy_cidr],
            "tags": {},
        })

        self._wait_until(
            lambda: self._open_unmanaged_count() >= baseline + 2,
            desc="worker reports unmanaged_occupancy for the newly observed VPC and decoy",
        )

    # =====================================================================
    # 2. plan: would_adopt; writes nothing
    # =====================================================================

    def test_02_plan_reports_would_adopt_and_writes_nothing(self):
        cls = type(self)
        cls.table_path = f"/tmp/{run_key('adopt-table')}.csv"
        cls.table_row = {
            "tenant_id": "developer", "allocation_key": cls.vpc_key, "scope": "vpc",
            "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "cidr": cls.vpc_cidr, "resource_id": cls.vpc_resource_id,
            "netbox_prefix_id": cls.vpc_prefix_id,
        }
        cls.table_csv = _adopt_csv([cls.table_row])

        stdout, _ = self.stack.adopt("plan", cls.table_path, files={cls.table_path: cls.table_csv})
        report = _report_json(stdout)
        self.assertEqual(len(report["records"]), 1, report)
        self.assertEqual(report["records"][0]["verdict"], "would_adopt", report)

        listed = self.stack.api("GET", "/v1/allocations").body
        keys = {row["allocation_key"] for row in (listed or {}).get("items", [])}
        self.assertNotIn(cls.vpc_key, keys, "plan must write nothing")
        prefixes = self.stack.netbox_prefixes()
        self.assertFalse((prefixes[cls.vpc_cidr].get("custom_fields") or {}).get("platform_allocation_id"),
                         "plan must not touch the NetBox prefix's ownership fields")

    # =====================================================================
    # 3. apply: adopts the vpc
    # =====================================================================

    def test_03_apply_adopts_the_vpc(self):
        cls = type(self)
        stdout, stderr = self.stack.adopt(
            "apply", cls.table_path, "--operator", "e2e-adopt-operator",
            files={cls.table_path: cls.table_csv},
        )
        report = _report_json(stdout)
        self.assertEqual(len(report["outcomes"]), 1, report)
        outcome = report["outcomes"][0]
        self.assertEqual(outcome["status"], "adopted", report)
        cls.vpc_allocation_id = outcome["allocation_id"]
        type(self).to_release.append(cls.vpc_allocation_id)

        allocation = self.stack.api("GET", f"/v1/allocations/{cls.vpc_allocation_id}").body
        self.assertEqual(allocation["state"], "RESERVED")
        self.assertEqual(allocation["cidr"], cls.vpc_cidr)

        prefixes = self.stack.netbox_prefixes()
        fields = prefixes[cls.vpc_cidr].get("custom_fields") or {}
        self.assertEqual(fields.get("platform_allocation_id"), cls.vpc_allocation_id)
        self.assertEqual(fields.get("platform_allocation_key"), cls.vpc_key)
        state = fields.get("platform_state")
        self.assertEqual(state.get("value") if isinstance(state, dict) else state, "RESERVED")
        # The import tag and provenance fields survive adoption (ADR 0010).
        tags = [t["slug"] for t in prefixes[cls.vpc_cidr].get("tags", [])]
        self.assertIn("platform-ipam-imported", tags)
        self.assertEqual(fields.get("platform_import_batch"), cls.vpc_import_batch)
        self.assertEqual(fields.get("platform_import_source"), "test_e2e_adopt.py")

        # No unmanaged_occupancy for the adopted VPC after a fresh worker
        # pass; the decoy still has one. Wait for the count to settle at
        # exactly one member (the decoy) for this account/region, since
        # reconcileAllocations recomputes OPEN/RESOLVED from scratch every
        # pass rather than only adding findings.
        self._wait_until(
            lambda: self._open_unmanaged_count() == 1,
            desc="unmanaged_occupancy clears for the adopted VPC but not the decoy",
        )
        findings = (self.stack.api("GET", "/v1/findings").body or {}).get("items", [])
        critical_open = [f for f in findings if f.get("allocation_id") == cls.vpc_allocation_id
                         and f.get("severity") == "CRITICAL" and f.get("status") == "OPEN"]
        self.assertEqual(critical_open, [], f"unexpected CRITICAL finding(s): {critical_open}")

        self._remove_resource(cls.decoy_resource_id)

    # =====================================================================
    # 4. the owning team's first POST replays; a different prefix length
    #    conflicts
    # =====================================================================

    def test_04_owning_teams_first_post_replays_and_conflicts(self):
        cls = type(self)
        replayed = self.stack.reserve(cls.vpc_key, network(cls.vpc_cidr).prefixlen)
        self.assertEqual(replayed["id"], cls.vpc_allocation_id)
        self.assertEqual(replayed["cidr"], cls.vpc_cidr)

        other_length = 20 if network(cls.vpc_cidr).prefixlen == 22 else 22
        response = self.stack.api("POST", "/v1/allocations", {
            "allocation_key": cls.vpc_key, "scope": "vpc", "environment": ENVIRONMENT,
            "region": REGION, "account_id": ACCOUNT_ID, "prefix_length": other_length,
            "description": "", "labels": {},
        }, {"Idempotency-Key": f"e2e-{cls.vpc_key}-conflict"})
        self.assertEqual(response.status, 409, response.body)
        self.assertEqual((response.body or {}).get("error", {}).get("code"), "allocation_key_conflict")

    # =====================================================================
    # 5. tag the VPC with both platform tags; the worker promotes it to
    #    ACTIVE
    # =====================================================================

    def test_05_tagging_the_vpc_promotes_it_to_active(self):
        cls = type(self)
        self._tag_resource(cls.vpc_resource_id, {
            "platform-ipam:allocation-id": cls.vpc_allocation_id,
            "platform-ipam:allocation-key": cls.vpc_key,
        })

        def active():
            allocation = self.stack.api("GET", f"/v1/allocations/{cls.vpc_allocation_id}").body
            if allocation.get("state") == "ACTIVE" and (allocation.get("binding") or {}).get("verified_at"):
                return allocation
            return None

        allocation = self._wait_until(active, desc="the tagged VPC becomes ACTIVE with a verified binding")
        self.assertEqual(allocation["binding"]["resource_id"], cls.vpc_resource_id)

    # =====================================================================
    # 6. a further apply replays the (now ACTIVE) VPC and writes nothing
    # =====================================================================

    def test_06_a_further_apply_replays_and_writes_nothing(self):
        cls = type(self)

        # test_05 waits for the LEDGER to say ACTIVE. The prefix's projection of
        # that -- status `active` and the binding fields -- is written by the
        # worker's next syncProjections pass, up to thirty seconds later, so a
        # "before" read taken at once can still show `reserved` and the
        # comparison below then blames the replay for the worker's write (how
        # this test failed once, package E3's run, 2026-09-21). Wait for the
        # projection to settle first.
        def projected():
            prefix = self.stack.netbox_prefixes().get(cls.vpc_cidr)
            if prefix and what_apply_could_change(prefix)["status"] == "active":
                return prefix
            return None

        prefix_before = self._wait_until(projected, desc="the worker projects ACTIVE onto the NetBox prefix")

        stdout, _ = self.stack.adopt(
            "apply", cls.table_path, "--operator", "e2e-adopt-operator",
            files={cls.table_path: cls.table_csv},
        )
        report = _report_json(stdout)
        self.assertEqual(len(report["outcomes"]), 1, report)
        outcome = report["outcomes"][0]
        self.assertEqual(outcome["status"], "adopted", report)  # replay
        self.assertEqual(outcome["allocation_id"], cls.vpc_allocation_id)

        allocation = self.stack.api("GET", f"/v1/allocations/{cls.vpc_allocation_id}").body
        self.assertEqual(allocation["state"], "ACTIVE", "a replay must not disturb the allocation's state")

        prefix_after = self.stack.netbox_prefixes()[cls.vpc_cidr]
        self.assertEqual(what_apply_could_change(prefix_before), what_apply_could_change(prefix_after),
                         "a replaying apply must not touch the NetBox prefix")

    # =====================================================================
    # 7. invariants: no unexplained managed prefix, and its converse
    # =====================================================================

    def test_07_netbox_and_the_api_agree_both_ways(self):
        listed = (self.stack.api("GET", "/v1/allocations").body or {}).get("items", [])
        known = {row["cidr"] for row in listed}
        prefixes = self.stack.netbox_prefixes()

        for cidr, row in prefixes.items():
            fields = row.get("custom_fields") or {}
            if not fields.get("platform_allocation_id"):
                continue
            self.assertIn(cidr, known, f"NetBox holds managed prefix {cidr} the API does not list")

        for row in listed:
            prefix = prefixes.get(row["cidr"])
            self.assertIsNotNone(prefix, f"allocation {row['id']} names CIDR {row['cidr']}, absent from NetBox")
            fields = (prefix or {}).get("custom_fields") or {}
            self.assertEqual(fields.get("platform_allocation_id"), row["id"],
                             f"NetBox prefix at {row['cidr']} does not carry allocation {row['id']}'s id")

    # =====================================================================
    # 8. release: the key is retired
    # =====================================================================

    def test_08_release_retires_the_key(self):
        cls = type(self)
        # "The resource must be gone from the observation" is what a full
        # reclaim (not exercised here -- see the comment below) needs;
        # removing it here reflects that an owning team relinquishing a
        # network stops reporting it, and keeps the worker's next pass from
        # trying to re-verify a binding mid-release.
        self._remove_resource(cls.vpc_resource_id)

        vpc_release = self.stack.api("DELETE", f"/v1/allocations/{cls.vpc_allocation_id}")
        self.assertIn(vpc_release.status, (200, 202, 204), vpc_release.body)

        vpc_after = self.stack.api("GET", f"/v1/allocations/{cls.vpc_allocation_id}").body
        self.assertEqual(vpc_after["state"], "QUARANTINED")

        response = self.stack.api("POST", "/v1/allocations", {
            "allocation_key": cls.vpc_key, "scope": "vpc", "environment": ENVIRONMENT,
            "region": REGION, "account_id": ACCOUNT_ID,
            "prefix_length": network(cls.vpc_cidr).prefixlen, "description": "", "labels": {},
        }, {"Idempotency-Key": f"e2e-{cls.vpc_key}-after-release"})
        self.assertEqual(response.status, 409, response.body)
        self.assertEqual((response.body or {}).get("error", {}).get("code"), "allocation_key_retired")
        # Not verified here: full reclamation (the quarantine hold is
        # deploy/compose/fixtures/pools.yaml's quarantine_hours: 168 --
        # 168 hours -- so it cannot be reached inside this suite's time
        # budget). What is shown is exactly "release accepted / quarantined"
        # and the key's permanent retirement.

    # =====================================================================
    # 9. recovery: a seeded pending ADOPT operation, against the real
    #    NetBox and cloud observer
    # =====================================================================

    def test_09_recovery_commits_a_seeded_pending_adopt(self):
        """Chose SQL seeding over really interrupting `adopt apply`
        (both are named as acceptable in the F5 package brief). `adopt` is
        its own short-lived process: to interrupt it between the ledger
        write and the adapter's commit deterministically would mean racing a
        kill signal against a call that, in the fake/dev stack, returns in
        well under a second -- not cheap or reliable to arrange from outside
        the container. Seeding the exact rows the first ledger transaction
        writes (internal/storage/postgres.go's `allocations`/`operations`
        tables, JSON shapes from internal/domain/types.go) is deterministic
        and, since PostgresLedger.Update always reloads the full state from
        those tables before rewriting them, indistinguishable from the
        worker's point of view."""
        cls = type(self)
        recovery_cidr = slot22(SLOT_RECOVERY)
        recovery_key = run_key("adopt-recovery")
        recovery_resource_id = aws_id("vpc", recovery_key)
        recovery_operator = "e2e-adopt-recovery-operator"
        batch = run_key("adopt-recovery-batch")

        prefix_id = self._import_network(recovery_cidr, "vpc", "adopt-recovery", batch)
        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "vpc",
            "id": recovery_resource_id, "cidr": recovery_cidr, "cidrs": [recovery_cidr],
            "tags": {},
        })

        alloc_id = "alloc_" + secrets.token_hex(16)
        op_id = "op_" + secrets.token_hex(16)
        type(self).to_release.append(alloc_id)
        now_iso = datetime.now(timezone.utc).isoformat()

        allocation_payload = {
            "allocation_key": recovery_key, "scope": "vpc", "environment": ENVIRONMENT,
            "region": REGION, "account_id": ACCOUNT_ID, "address_family": "ipv4",
            "prefix_length": network(recovery_cidr).prefixlen, "parent_allocation_id": "",
            "availability_zone_id": "", "description": "", "labels": {},
            "id": alloc_id, "tenant_id": "developer", "domain_id": DOMAIN_ID,
            "request_hash": "e2e-seeded-recovery-hash", "cidr": recovery_cidr,
            "pool_id": POOL_ID, "state": "RESERVED", "revision": 1,
            "policy_version": "development-1", "inventory_id": "", "inventory_sync": "PENDING",
            "binding": None, "created_at": now_iso, "updated_at": now_iso,
            "release_requested_at": None, "quarantine_until": None, "last_observed_at": None,
            "release_blockers": [], "committed": False,
        }
        operation_payload = {
            "id": op_id, "type": "ADOPT", "status": "PENDING", "allocation_id": alloc_id,
            "tenant_id": "developer", "domain_id": DOMAIN_ID, "result": None, "error": None,
            "candidate": None,
            "adoption": {"operator": recovery_operator, "network_id": prefix_id,
                        "resource_id": recovery_resource_id, "import_batch": batch},
            "created_at": now_iso, "updated_at": now_iso,
        }
        self._seed_pending_adopt(alloc_id, recovery_key, allocation_payload, op_id, operation_payload)

        def committed():
            response = self.stack.api("GET", f"/v1/allocations/{alloc_id}")
            return response.body if response.status == 200 else None

        allocation = self._wait_until(committed, desc="the worker's recovery path commits the seeded adoption")
        self.assertEqual(allocation["state"], "RESERVED")
        self.assertEqual(allocation["cidr"], recovery_cidr)

        events = self._audit_events(alloc_id)
        committed_events = [e for e in events if e["Action"] == "ADOPT_COMMITTED"]
        self.assertEqual(len(committed_events), 1, events)
        self.assertEqual(committed_events[0]["Actor"], recovery_operator)

        # The domain's overlap fence (503 domain_busy) is gone now that the
        # pending operation has resolved. Probed with a read-only `plan`
        # call (PlanAdoption checks pendingDomain before pinnedCIDR, exactly
        # as Adopt's own write transaction does) rather than a real
        # reservation, since this class's quota budget is 2 allocations
        # (the vpc, this recovery vpc) and a third would blow it. A made-up,
        # never-imported CIDR still reaches that check first: a lingering
        # fence would answer "domain_busy"; anything else (here,
        # "adoption_refused" -- the reviewed network was never imported)
        # proves the fence is gone.
        probe_row = {
            "tenant_id": "developer", "allocation_key": run_key("adopt-recovery-domain-probe"),
            "scope": "vpc", "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "cidr": slot22(SLOT_DOMAIN_BUSY_PROBE), "resource_id": "vpc-domain-busy-probe",
            "netbox_prefix_id": "0",
        }
        probe_report = self._plan_one(probe_row)
        probe_result = probe_report["records"][0]
        self.assertEqual(probe_result["verdict"], "refused", probe_report)
        self.assertNotEqual(probe_result["refusal_code"], "domain_busy", probe_report)

        self._remove_resource(recovery_resource_id)

    def _seed_pending_adopt(self, alloc_id, key, allocation_payload, op_id, operation_payload) -> None:
        sql = (
            f"INSERT INTO allocations (id, tenant_id, allocation_key, committed, payload, created_at) "
            f"VALUES ({_sql_str(alloc_id)}, 'developer', {_sql_str(key)}, false, "
            f"$alloc_json${json.dumps(allocation_payload)}$alloc_json$::jsonb, now());\n"
            f"INSERT INTO operations (id, tenant_id, domain_id, payload) "
            f"VALUES ({_sql_str(op_id)}, 'developer', {_sql_str(DOMAIN_ID)}, "
            f"$op_json${json.dumps(operation_payload)}$op_json$::jsonb);\n"
        )
        self._psql(sql)

    def _audit_events(self, allocation_id: str) -> list:
        raw = self._psql(
            f"SELECT payload::text FROM audit_events WHERE allocation_id = {_sql_str(allocation_id)} "
            f"ORDER BY id;",
            tuples_only=True,
        )
        return [json.loads(line) for line in raw.splitlines() if line.strip()]

    # =====================================================================
    # Refusals: cheap, read-only, assert exit 3, the refusal code, and that
    # nothing was written.
    # =====================================================================

    def test_10_refusal_wrong_netbox_prefix_id(self):
        cidr = slot22(SLOT_REFUSAL_WRONG_PREFIX_ID)
        key = run_key("adopt-refusal-a")
        batch = run_key("adopt-refusal-a-batch")
        self._import_network(cidr, "vpc", "adopt-refusal-a", batch)
        row = {
            "tenant_id": "developer", "allocation_key": key, "scope": "vpc",
            "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "cidr": cidr, "resource_id": aws_id("vpc", key),
            # A real prefix id, just not the one at this CIDR
            # (type(self).vpc_prefix_id names the VPC from test_01/03).
            "netbox_prefix_id": type(self).vpc_prefix_id,
        }
        report = self._plan_one(row)
        result = report["records"][0]
        self.assertEqual(result["verdict"], "refused", report)
        self.assertEqual(result["refusal_code"], "adoption_refused", report)
        self._assert_nothing_written(key)

    def test_11_refusal_cidr_with_no_observed_resource(self):
        cidr = slot22(SLOT_REFUSAL_NO_RESOURCE)
        key = run_key("adopt-refusal-b")
        batch = run_key("adopt-refusal-b-batch")
        prefix_id = self._import_network(cidr, "vpc", "adopt-refusal-b", batch)
        row = {
            "tenant_id": "developer", "allocation_key": key, "scope": "vpc",
            "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "cidr": cidr, "resource_id": "vpc-never-observed", "netbox_prefix_id": prefix_id,
        }
        report = self._plan_one(row)
        result = report["records"][0]
        self.assertEqual(result["verdict"], "refused", report)
        self.assertEqual(result["refusal_code"], "adoption_refused", report)
        self._assert_nothing_written(key)

    def test_12_refusal_second_imported_prefix_overlaps(self):
        cidr = slot22(SLOT_REFUSAL_SECOND_PREFIX)
        key = run_key("adopt-refusal-c")
        batch = run_key("adopt-refusal-c-batch")
        prefix_id = self._import_network(cidr, "vpc", "adopt-refusal-c-vpc", batch)
        # A second, nested, overlapping imported prefix -- structurally the
        # same shape as internal/service/adopt_test.go's
        # TestAdoptRefusesASecondUnmanagedPrefix.
        self._import_network(first24(cidr), "subnet", "adopt-refusal-c-nested", run_key("adopt-refusal-c-nested"))
        row = {
            "tenant_id": "developer", "allocation_key": key, "scope": "vpc",
            "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "cidr": cidr, "resource_id": "vpc-does-not-matter-either", "netbox_prefix_id": prefix_id,
        }
        report = self._plan_one(row)
        result = report["records"][0]
        self.assertEqual(result["verdict"], "refused", report)
        self.assertEqual(result["refusal_code"], "adoption_refused", report)
        self._assert_nothing_written(key)

    def test_13_refusal_unknown_column(self):
        path = f"/tmp/{run_key('adopt-refusal-d')}.csv"
        csv_text = (
            "tenant_id,allocation_key,scope,environment,region,account_id,cidr,resource_id,"
            "netbox_prefix_id,parent_allocation_key,availability_zone_id,bogus_column\n"
            "developer,x,vpc,development,eu-central-1,000000000000,10.64.255.0/22,vpc-x,1,,,\n"
        )
        stdout, stderr = self.stack.adopt("plan", path, files={path: csv_text}, expect_exit=3)
        self.assertNotIn('"records"', stdout, "an unknown column must fail before any report is printed")
        self.assertIn("unknown column", stderr)

    def test_14_refusal_missing_operator(self):
        # checkArgs validates usage before opening the ledger or the table
        # (internal/adoptcmd/adoptcmd.go), so a nonexistent path is fine here.
        self.stack.adopt("apply", "/tmp/e2e-adopt-missing-operator.csv", expect_exit=2)


class AdoptVPCWithSubnetsE2ETest(_AdoptFixtureCase):
    """The two runs a VPC with subnets takes, end to end (package F7).

    Ordered, like AdoptE2ETest: each test_NN builds on the last. Quota: 2
    allocations (the VPC and one subnet).

    Two of these tests are package F5's `KnownGap...` tests inverted.
    test_02 planned the same VPC-with-an-imported-subnet shape and asserted
    `refused`/`adoption_refused`; it now asserts `would_adopt`. test_08
    imported a subnet nested in an adopted VPC and asserted onboard's
    `overlaps-managed` error; it now asserts the import succeeds with an
    `inside-managed-vpc` note. Everything between them is the procedure the
    runbook's section 6 describes, which had never been run.
    """

    # =====================================================================
    # 1. import a VPC *and* a subnet nested inside it; observe both
    # =====================================================================

    def test_01_import_and_observe_a_vpc_with_one_subnet(self):
        cls = type(self)
        cls.vpc_cidr = slot22(SLOT_VPC_WITH_SUBNET)
        cls.subnet_cidr = first24(cls.vpc_cidr)
        cls.vpc_key = run_key("adopt-parent-vpc")
        cls.subnet_key = run_key("adopt-child-subnet")
        cls.vpc_resource_id = aws_id("vpc", cls.vpc_key)
        cls.subnet_resource_id = aws_id("subnet", cls.subnet_key)
        batch = run_key("adopt-two-run-batch")
        cls.import_batch = batch

        # One import batch, a VPC and its subnet -- which is what an
        # organization inventory really produces, and exactly the input that
        # made the VPC unadoptable before package F7.
        cls.vpc_prefix_id = self._import_network(cls.vpc_cidr, "vpc", "adopt-parent-vpc", batch)
        cls.subnet_prefix_id = self._import_network(cls.subnet_cidr, "subnet", "adopt-child-subnet", batch)

        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "vpc",
            "id": cls.vpc_resource_id, "cidr": cls.vpc_cidr, "cidrs": [cls.vpc_cidr],
            "tags": {},
        })
        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "subnet",
            "id": cls.subnet_resource_id, "cidr": cls.subnet_cidr, "cidrs": [cls.subnet_cidr],
            "parent_id": cls.vpc_resource_id, "zone_id": AZ, "tags": {},
        })

        # _flush_cloud rewrites the whole fixture, so this class's two
        # resources are the only ones observed: the open unmanaged_occupancy
        # count for this account/region is therefore an exact number, not a
        # delta, and it is the number the next three tests watch.
        self._wait_until(
            lambda: self._open_unmanaged_count() == 2,
            desc="the worker reports the VPC and its subnet as unmanaged occupancy",
        )

    # =====================================================================
    # 2. plan: the VPC would adopt; the subnet defers on it
    # =====================================================================

    def test_02_plan_would_adopt_the_vpc_and_defers_its_subnet(self):
        cls = type(self)
        cls.table_path = f"/tmp/{run_key('adopt-two-run-table')}.csv"
        cls.table_csv = _adopt_csv([
            {
                "tenant_id": "developer", "allocation_key": cls.vpc_key, "scope": "vpc",
                "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
                "cidr": cls.vpc_cidr, "resource_id": cls.vpc_resource_id,
                "netbox_prefix_id": cls.vpc_prefix_id,
            },
            {
                "tenant_id": "developer", "allocation_key": cls.subnet_key, "scope": "subnet",
                "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
                "cidr": cls.subnet_cidr, "resource_id": cls.subnet_resource_id,
                "netbox_prefix_id": cls.subnet_prefix_id,
                "parent_allocation_key": cls.vpc_key, "availability_zone_id": AZ,
            },
        ])

        # Package F5's test_before_adoption_a_nested_subnet_blocks_the_vpcs_
        # own_adoption planned this exact shape and asserted refused /
        # adoption_refused. Inverted here.
        stdout, _ = self.stack.adopt("plan", cls.table_path, files={cls.table_path: cls.table_csv})
        report = _report_json(stdout)
        verdicts = {r["allocation_key"]: r for r in report["records"]}
        self.assertEqual(verdicts[cls.vpc_key]["verdict"], "would_adopt", report)
        self.assertEqual(verdicts[cls.vpc_key]["cidr"], cls.vpc_cidr, report)
        self.assertEqual(verdicts[cls.subnet_key]["verdict"], "deferred_on_parent", report)

        self._assert_nothing_written(cls.vpc_key)
        self._assert_nothing_written(cls.subnet_key)
        prefixes = self.stack.netbox_prefixes()
        for cidr in (cls.vpc_cidr, cls.subnet_cidr):
            self.assertFalse((prefixes[cidr].get("custom_fields") or {}).get("platform_allocation_id"),
                             f"plan touched the ownership fields of {cidr}")

    # =====================================================================
    # 3. apply, run one: the VPC is adopted, the subnet waits, exit 3
    # =====================================================================

    def test_03_first_apply_adopts_the_vpc_and_waits_for_the_subnet(self):
        cls = type(self)
        stdout, stderr = self.stack.adopt(
            "apply", cls.table_path, "--operator", "e2e-adopt-operator",
            files={cls.table_path: cls.table_csv}, expect_exit=3,
        )
        report = _report_json(stdout)
        outcomes = {o["allocation_key"]: o for o in report["outcomes"]}
        self.assertEqual(outcomes[cls.vpc_key]["status"], "adopted", report)
        self.assertEqual(outcomes[cls.subnet_key]["status"], "waiting_for_parent", report)
        cls.vpc_allocation_id = outcomes[cls.vpc_key]["allocation_id"]
        cls.to_release.append(cls.vpc_allocation_id)
        self.assertIn("waiting for a parent binding", stderr)

        allocation = self.stack.api("GET", f"/v1/allocations/{cls.vpc_allocation_id}").body
        self.assertEqual(allocation["state"], "RESERVED")
        self.assertEqual(allocation["cidr"], cls.vpc_cidr)

        prefixes = self.stack.netbox_prefixes()
        self.assertEqual((prefixes[cls.vpc_cidr].get("custom_fields") or {}).get("platform_allocation_id"),
                         cls.vpc_allocation_id)
        # The subnet is untouched occupancy: adopting a VPC converts exactly
        # one prefix, never its children.
        self.assertFalse((prefixes[cls.subnet_cidr].get("custom_fields") or {}).get("platform_allocation_id"),
                         "adopting the VPC also converted its subnet's prefix")
        self.assertIn("platform-ipam-imported", [t["slug"] for t in prefixes[cls.subnet_cidr].get("tags", [])])

        # The subnet's unmanaged_occupancy finding stays open -- it is still
        # nobody's -- and the VPC's closes. adoptedResources exempts only the
        # one resource the adoption reviewed (ADR 0010, F3's paragraph).
        self._wait_until(
            lambda: self._open_unmanaged_count() == 1,
            desc="occupancy clears for the adopted VPC and stays open for its subnet",
        )
        findings = (self.stack.api("GET", "/v1/findings").body or {}).get("items", [])
        critical_open = [f for f in findings if f.get("allocation_id") == cls.vpc_allocation_id
                         and f.get("severity") == "CRITICAL" and f.get("status") == "OPEN"]
        self.assertEqual(critical_open, [], f"unexpected CRITICAL finding(s): {critical_open}")

    # =====================================================================
    # 4. the owning team tags the VPC; the worker promotes it to ACTIVE
    # =====================================================================

    def test_04_tagging_the_vpc_promotes_it_to_active(self):
        cls = type(self)
        self._tag_resource(cls.vpc_resource_id, {
            "platform-ipam:allocation-id": cls.vpc_allocation_id,
            "platform-ipam:allocation-key": cls.vpc_key,
        })

        def active():
            allocation = self.stack.api("GET", f"/v1/allocations/{cls.vpc_allocation_id}").body
            if allocation.get("state") == "ACTIVE" and (allocation.get("binding") or {}).get("verified_at"):
                return allocation
            return None

        allocation = self._wait_until(active, desc="the tagged VPC becomes ACTIVE with a verified binding")
        self.assertEqual(allocation["binding"]["resource_id"], cls.vpc_resource_id)

    # =====================================================================
    # 5. apply, run two: the VPC replays and the subnet is adopted, exit 0
    # =====================================================================

    def test_05_second_apply_replays_the_vpc_and_adopts_the_subnet(self):
        cls = type(self)
        stdout, _ = self.stack.adopt(
            "apply", cls.table_path, "--operator", "e2e-adopt-operator",
            files={cls.table_path: cls.table_csv},
        )
        report = _report_json(stdout)
        outcomes = {o["allocation_key"]: o for o in report["outcomes"]}
        self.assertEqual(outcomes[cls.vpc_key]["status"], "adopted", report)  # replay
        self.assertEqual(outcomes[cls.vpc_key]["allocation_id"], cls.vpc_allocation_id, report)
        self.assertEqual(outcomes[cls.subnet_key]["status"], "adopted", report)
        cls.subnet_allocation_id = outcomes[cls.subnet_key]["allocation_id"]
        # Children before parents at release time, or the parent's release is
        # blocked by a live child.
        cls.to_release.insert(0, cls.subnet_allocation_id)

        subnet = self.stack.api("GET", f"/v1/allocations/{cls.subnet_allocation_id}").body
        self.assertEqual(subnet["state"], "RESERVED")
        self.assertEqual(subnet["cidr"], cls.subnet_cidr)
        self.assertEqual(subnet["scope"], "subnet")
        self.assertEqual(subnet["parent_allocation_id"], cls.vpc_allocation_id)
        self.assertEqual(subnet["availability_zone_id"], AZ)

        prefixes = self.stack.netbox_prefixes()
        fields = prefixes[cls.subnet_cidr].get("custom_fields") or {}
        self.assertEqual(fields.get("platform_allocation_id"), cls.subnet_allocation_id)
        self.assertEqual(fields.get("platform_parent_allocation_id"), cls.vpc_allocation_id)
        # Adoption preserves provenance for a subnet exactly as for a VPC.
        self.assertIn("platform-ipam-imported", [t["slug"] for t in prefixes[cls.subnet_cidr].get("tags", [])])
        self.assertEqual(fields.get("platform_import_batch"), cls.import_batch)

        self._wait_until(
            lambda: self._open_unmanaged_count() == 0,
            desc="no unmanaged occupancy is left once both networks are adopted",
        )

    # =====================================================================
    # 6. apply, run three: both replay and nothing is written
    # =====================================================================

    def test_06_a_third_apply_writes_nothing(self):
        cls = type(self)
        before = self.stack.netbox_prefixes()
        stamps = {cidr: what_apply_could_change(before[cidr]) for cidr in (cls.vpc_cidr, cls.subnet_cidr)}

        stdout, _ = self.stack.adopt(
            "apply", cls.table_path, "--operator", "e2e-adopt-operator",
            files={cls.table_path: cls.table_csv},
        )
        report = _report_json(stdout)
        for outcome in report["outcomes"]:
            self.assertEqual(outcome["status"], "adopted", report)

        after = self.stack.netbox_prefixes()
        for cidr, stamp in stamps.items():
            self.assertEqual(stamp, what_apply_could_change(after[cidr]),
                             f"a replaying apply touched the NetBox prefix at {cidr}")

    # =====================================================================
    # 7. the refusals the amendment had to keep: cheap, read-only, no quota
    # =====================================================================

    def test_07_refusals_the_child_exemption_does_not_cover(self):
        """The exemption is evidence-based and narrow: a subnet inside the pin
        is exempt only when the observation says it is the reviewed VPC's own
        child, and an imported prefix inside the pin is exempt only when such
        a child holds exactly that CIDR. Both halves are checked here against
        the real service, through a read-only `plan`."""
        cls = type(self)
        vpc_cidr = slot22(SLOT_CHILD_REFUSALS)
        nested_cidr = first24(vpc_cidr)
        vpc_resource_id = aws_id("vpc", run_key("adopt-refusal-parent"))
        batch = run_key("adopt-child-refusal-batch")

        vpc_prefix_id = self._import_network(vpc_cidr, "vpc", "adopt-refusal-parent", batch)
        self._import_network(nested_cidr, "subnet", "adopt-refusal-nested", batch)
        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "vpc",
            "id": vpc_resource_id, "cidr": vpc_cidr, "cidrs": [vpc_cidr], "tags": {},
        })

        def plan_the_vpc():
            row = {
                "tenant_id": "developer", "allocation_key": run_key("adopt-child-refusal"),
                "scope": "vpc", "environment": ENVIRONMENT, "region": REGION,
                "account_id": ACCOUNT_ID, "cidr": vpc_cidr, "resource_id": vpc_resource_id,
                "netbox_prefix_id": vpc_prefix_id,
            }
            report = self._plan_one(row)
            self.assertEqual(report["records"][0]["verdict"], "refused", report)
            self.assertEqual(report["records"][0]["refusal_code"], "adoption_refused", report)
            self._assert_nothing_written(row["allocation_key"])

        # (a) a stranger's subnet: the right CIDR, the wrong parent.
        stranger_id = aws_id("subnet", run_key("adopt-refusal-stranger"))
        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "subnet",
            "id": stranger_id, "cidr": nested_cidr, "cidrs": [nested_cidr],
            "parent_id": aws_id("vpc", "somebody-elses-vpc"), "zone_id": AZ, "tags": {},
        })
        plan_the_vpc()

        # (b) an orphan imported prefix: the VPC's real child is observed, but
        # somewhere else, so the imported prefix at nested_cidr is accounted
        # for by nothing in the cloud.
        self._remove_resource(stranger_id)
        child_id = aws_id("subnet", run_key("adopt-refusal-orphan"))
        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "subnet",
            "id": child_id, "cidr": nth24(vpc_cidr, 1), "cidrs": [nth24(vpc_cidr, 1)],
            "parent_id": vpc_resource_id, "zone_id": AZ, "tags": {},
        })
        plan_the_vpc()

        self._remove_resource(child_id)
        self._remove_resource(vpc_resource_id)

    # =====================================================================
    # 8. part two: a subnet created after its VPC was adopted is importable
    # =====================================================================

    def test_08_onboard_imports_a_subnet_created_after_the_vpc_was_adopted(self):
        """Package F5's test_after_adoption_onboard_refuses_the_nested_subnet
        asserted `overlaps-managed` here, with no write set. Inverted: a row
        strictly inside a platform-managed **vpc**-scoped allocation is
        importable as unmanaged occupancy, with an informational finding that
        names the parent allocation (internal/onboard's RuleInsideManagedVPC).
        onboard still never opens the ledger -- it tells a managed VPC from a
        managed subnet by the snapshot's platform_parent_allocation_id alone."""
        cls = type(self)
        later_cidr = nth24(cls.vpc_cidr, 1)
        batch = run_key("adopt-later-subnet-batch")
        path = f"/tmp/{batch}.json"
        csv_text = _networks_csv([{"cidr": later_cidr, "account_id": ACCOUNT_ID,
                                   "region": REGION, "name": "adopt-later-subnet", "type": "subnet"}])
        self.stack.onboard("parse", "-", "--out", path, stdin=csv_text)

        stdout, _ = self.stack.onboard("plan", path, "--domain", DOMAIN_ID)
        report = json.loads(stdout)
        self.assertEqual([f for f in report["Findings"] if f["Rule"] == "overlaps-managed"], [], report)
        inside = [f for f in report["Findings"] if f["Rule"] == "inside-managed-vpc"]
        self.assertEqual(len(inside), 1, report)
        self.assertEqual(inside[0]["Level"], "info", report)
        self.assertIn(cls.vpc_allocation_id, inside[0]["Message"], report)
        self.assertEqual([w["CIDR"] for w in report["Writes"]], [later_cidr], report)

        self.stack.onboard("apply", path, "--domain", DOMAIN_ID, "--batch", batch,
                           "--source", "test_e2e_adopt.py")
        prefixes = self.stack.netbox_prefixes()
        self.assertIn(later_cidr, prefixes, "the import wrote no NetBox prefix")
        fields = prefixes[later_cidr].get("custom_fields") or {}
        self.assertFalse(fields.get("platform_allocation_id"),
                         "an import must write occupancy, never ownership")
        self.assertEqual(fields.get("platform_import_batch"), batch)

        # And the managed subnet beside it is still off limits: the exception
        # is for a VPC's space, not for an allocation's own.
        blocked_batch = run_key("adopt-inside-subnet-batch")
        blocked_path = f"/tmp/{blocked_batch}.json"
        inside_subnet = str(next(network(cls.subnet_cidr).subnets(new_prefix=26)))
        self.stack.onboard("parse", "-", "--out", blocked_path, stdin=_networks_csv([
            {"cidr": inside_subnet, "account_id": ACCOUNT_ID, "region": REGION,
             "name": "adopt-inside-managed-subnet", "type": "subnet"}]))
        blocked_stdout, _ = self.stack.onboard("plan", blocked_path, "--domain", DOMAIN_ID,
                                               expect_exit=3)
        blocked = json.loads(blocked_stdout)
        overlaps = [f for f in blocked["Findings"] if f["Rule"] == "overlaps-managed"]
        self.assertEqual(len(overlaps), 1, blocked)
        self.assertIn(cls.subnet_allocation_id, overlaps[0]["Message"], blocked)
        self.assertEqual(blocked["Writes"], None, blocked)


class AdoptAbandonE2ETest(_AdoptFixtureCase):
    """`platform-ipam adopt abandon` (package H2c, ADR 0012), end to end
    against the real ledger and NetBox.

    Ordered, like the two classes above: each test_NN builds on the last.
    Quota: 1 allocation net (the seeded, half-converted hold is deleted by
    the abandon and never counts; the re-adoption under the same key in
    test_05 spends the only slot this class keeps).

    The half-converted hold is seeded, not produced by a real interruption,
    for the same reason package F5 recorded for its own recovery test:
    `adopt` finishes in well under a second, so racing a kill signal against
    it is not deterministic. Seeding the exact rows the first ledger
    transaction writes (internal/storage/postgres.go's allocations/
    operations tables) is indistinguishable from the worker's point of view,
    because PostgresLedger.Update always reloads the full state from those
    tables before rewriting them (test_09_recovery_commits_a_seeded_pending_adopt,
    above, established the same thing for ordinary recovery).

    What makes this hold genuinely *stuck* -- rather than a hold recovery
    would simply finish -- is chosen deliberately, and it is not the shape
    ADR 0012's prose illustrates with (`adoptedTheReviewedObject`). The
    seeded operation's durable `adoption.network_id` names a NetBox prefix
    id that does not exist at the adopted CIDR; internal/service/service.go's
    `reviewedNetwork` requires the *real* prefix's id to equal that recorded
    id before it will treat the prefix as the reviewed one, even when the
    prefix already carries this allocation's own ownership fields (the
    tolerance that lets an ordinary crash converge), so the mismatch makes
    `reviewedNetwork` itself refuse with `adoption_refused` -- before
    `internal/netbox`'s `Adopt` (and therefore `adoptedTheReviewedObject`)
    is ever reached. `recoverAdoption` treats that refusal exactly like
    `adoptedTheReviewedObject` returning false: `flagStuckAdoption`, a
    `CRITICAL` `adoption_stuck` finding, and the pending operation left in
    place. Either route lands on the same practical state ADR 0012 describes
    -- "the ownership fields landed on an object nobody reviewed" -- and the
    prefix this test PATCHes directly (simulating the adapter write an
    interrupted `Adopt` would have made) carries THIS allocation's and THIS
    operation's own markers, which is exactly what `AbandonAdoption`'s
    `findByMarker`-plus-operation-id check requires to clear it.
    """

    # =====================================================================
    # 1. seed a half-converted, genuinely stuck adoption
    # =====================================================================

    def test_01_seed_a_stuck_half_converted_adoption(self):
        cls = type(self)
        cls.cidr = slot22(SLOT_ABANDON)
        cls.key = run_key("adopt-abandon")
        cls.resource_id = aws_id("vpc", cls.key)
        cls.operator = "e2e-adopt-abandon-operator"
        batch = run_key("adopt-abandon-batch")
        cls.import_batch = batch

        cls.prefix_id = self._import_network(cls.cidr, "vpc", "adopt-abandon-vpc", batch)
        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "vpc",
            "id": cls.resource_id, "cidr": cls.cidr, "cidrs": [cls.cidr],
            "tags": {},
        })

        cls.alloc_id = "alloc_" + secrets.token_hex(16)
        cls.op_id = "op_" + secrets.token_hex(16)
        # A NetBox prefix id that cannot exist at this CIDR: internal/netbox's
        # ids are small positive integers, so this is guaranteed distinct from
        # cls.prefix_id without depending on its exact value.
        wrong_network_id = str(int(cls.prefix_id) + 900000)
        now_iso = datetime.now(timezone.utc).isoformat()

        allocation_payload = {
            "allocation_key": cls.key, "scope": "vpc", "environment": ENVIRONMENT,
            "region": REGION, "account_id": ACCOUNT_ID, "address_family": "ipv4",
            "prefix_length": network(cls.cidr).prefixlen, "parent_allocation_id": "",
            "availability_zone_id": "", "description": "", "labels": {},
            "id": cls.alloc_id, "tenant_id": "developer", "domain_id": DOMAIN_ID,
            "request_hash": "e2e-seeded-abandon-hash", "cidr": cls.cidr,
            "pool_id": POOL_ID, "state": "RESERVED", "revision": 1,
            "policy_version": "development-1", "inventory_id": "", "inventory_sync": "PENDING",
            "binding": None, "created_at": now_iso, "updated_at": now_iso,
            "release_requested_at": None, "quarantine_until": None, "last_observed_at": None,
            "release_blockers": [], "committed": False,
        }
        operation_payload = {
            "id": cls.op_id, "type": "ADOPT", "status": "PENDING", "allocation_id": cls.alloc_id,
            "tenant_id": "developer", "domain_id": DOMAIN_ID, "result": None, "error": None,
            "candidate": None,
            "adoption": {
                "operator": cls.operator, "network_id": wrong_network_id,
                "resource_id": cls.resource_id, "import_batch": batch,
                "prior_status": "active", "prior_account_id": ACCOUNT_ID, "prior_region": REGION,
            },
            "created_at": now_iso, "updated_at": now_iso,
        }
        self._seed_pending_adopt(cls.alloc_id, cls.key, allocation_payload, cls.op_id, operation_payload)
        # This class's own idempotency record and ADOPT_PLANNED audit event,
        # matching what reserve's first transaction would have written for a
        # real adoption -- test_09 above seeds only the two ledger rows
        # because its scenario never needs to prove the delete removed them;
        # this class's test_03 does.
        self._seed_idempotency_record(cls.alloc_id, cls.key)
        planned_event_id = "evt_" + secrets.token_hex(16)
        self._seed_audit_event(planned_event_id, cls.alloc_id, "developer", {
            "ID": planned_event_id, "AllocationID": cls.alloc_id, "TenantID": "developer",
            "Actor": cls.operator, "Action": "ADOPT_PLANNED",
            "Reason": f"abandon test seed: adoption planned for {cls.cidr} under key {cls.key!r}",
            "At": now_iso, "Revision": 1,
        })

        # Simulate the adapter write an interrupted Adopt would have made:
        # the ownership fields ownedFields writes for an unbound allocation,
        # plus status: reserved (internal/netbox/adopt.go's own payload
        # shape), landing on the one real prefix at this CIDR.
        self._mark_prefix_owned(cls.prefix_id, cls.alloc_id, cls.key, cls.op_id)
        cls.prefix_fields_after_seed = self.stack.netbox_prefixes()[cls.cidr]

        finding = self._wait_until(
            lambda: self._open_adoption_stuck_finding(cls.alloc_id),
            desc="the worker raises adoption_stuck for the half-converted hold",
        )
        self.assertEqual(finding["severity"], "CRITICAL", finding)
        self.assertEqual(finding["allocation_id"], cls.alloc_id, finding)

        # The overlap domain is fenced: a fresh, never-imported CIDR in the
        # same domain refuses with domain_busy rather than reaching any
        # ground-truth check (mirrors test_09's inverse probe, above).
        probe_row = {
            "tenant_id": "developer", "allocation_key": run_key("adopt-abandon-domain-probe"),
            "scope": "vpc", "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "cidr": slot22(SLOT_ABANDON_DOMAIN_PROBE), "resource_id": "vpc-abandon-domain-busy-probe",
            "netbox_prefix_id": "0",
        }
        probe_report = self._plan_one(probe_row)
        probe_result = probe_report["records"][0]
        self.assertEqual(probe_result["verdict"], "refused", probe_report)
        self.assertEqual(probe_result["refusal_code"], "domain_busy", probe_report)

    # =====================================================================
    # 2. dry run: writes nothing, reports inventory.claim == this_operation
    # =====================================================================

    def test_02_dry_run_reports_the_claim_and_writes_nothing(self):
        cls = type(self)
        before_fields = self.stack.netbox_prefixes()[cls.cidr]
        before_count = self._row_count("allocations", f"id = {_sql_str(cls.alloc_id)}")

        stdout, stderr = self.stack.adopt(
            "abandon", "--allocation-id", cls.alloc_id, "--operator", cls.operator,
            "--reason", "e2e: reviewed record named the wrong NetBox prefix id", "--dry-run",
        )
        report = _report_json(stdout)
        self.assertTrue(report["dry_run"], report)
        self.assertEqual(report["allocation"]["id"], cls.alloc_id, report)
        self.assertFalse(report.get("deleted"), report)
        self.assertFalse(report.get("cleared"), report)
        self.assertEqual(report["inventory"]["claim"], "this_operation", report)
        self.assertEqual(report["inventory"]["network_id"], cls.prefix_id, report)
        self.assertEqual(report["inventory"]["operation_id"], cls.op_id, report)
        self.assertTrue(report.get("would_do"), report)
        self.assertIn("dry run", stderr)

        after_fields = self.stack.netbox_prefixes()[cls.cidr]
        self.assertEqual(before_fields.get("custom_fields"), after_fields.get("custom_fields"),
                         "a dry run must not touch the NetBox prefix")
        after_count = self._row_count("allocations", f"id = {_sql_str(cls.alloc_id)}")
        self.assertEqual(before_count, after_count, "a dry run must not touch the ledger")
        self.assertTrue(self._open_adoption_stuck_finding(cls.alloc_id),
                        "a dry run must not resolve the adoption_stuck finding")

    # =====================================================================
    # 3. abandon for real: clears the prefix, deletes the hold
    # =====================================================================

    def test_03_abandon_clears_the_prefix_and_deletes_the_hold(self):
        cls = type(self)
        before_fields = (cls.prefix_fields_after_seed.get("custom_fields") or {})
        self.assertEqual(before_fields.get("platform_allocation_id"), cls.alloc_id,
                         "the seed did not land the ownership fields it was meant to")

        stdout, stderr = self.stack.adopt(
            "abandon", "--allocation-id", cls.alloc_id, "--operator", cls.operator,
            "--reason", "e2e: reviewed record named the wrong NetBox prefix id",
        )
        report = _report_json(stdout)
        self.assertFalse(report["dry_run"], report)
        self.assertTrue(report["cleared"], report)
        self.assertTrue(report["deleted"], report)
        self.assertTrue(report["finding_resolved"], report)
        self.assertIn("deleted=true", stderr)

        # The prefix lost every ownership field ...
        prefixes = self.stack.netbox_prefixes()
        fields = prefixes[cls.cidr].get("custom_fields") or {}
        for owned_key in (
            "platform_allocation_id", "platform_allocation_key", "platform_operation_id",
            "platform_state", "platform_tenant_id", "platform_environment", "platform_pool_id",
            "platform_policy_version", "platform_parent_allocation_id", "platform_aws_az_id",
        ):
            self.assertFalse(fields.get(owned_key), f"{owned_key} survived the clear: {fields}")
        self.assertEqual(prefixes[cls.cidr]["status"]["value"], "active",
                         "status must be restored from the seeded prior_status")
        # ... but kept the import tag, batch and source, and the AWS account
        # and region the import wrote (seeded as prior_account_id/prior_region).
        tags = [t["slug"] for t in prefixes[cls.cidr].get("tags", [])]
        self.assertIn("platform-ipam-imported", tags)
        self.assertEqual(fields.get("platform_import_batch"), cls.import_batch)
        self.assertEqual(fields.get("platform_import_source"), "test_e2e_adopt.py")
        self.assertEqual(fields.get("platform_aws_account_id"), ACCOUNT_ID)
        self.assertEqual(fields.get("platform_aws_region"), REGION)

        # The allocation is gone: 404 for the developer, and gone from the
        # ledger's own table.
        response = self.stack.api("GET", f"/v1/allocations/{cls.alloc_id}")
        self.assertEqual(response.status, 404, response.body)
        self.assertEqual(self._row_count("allocations", f"id = {_sql_str(cls.alloc_id)}"), 0)

        # The ADOPT idempotency record is gone.
        self.assertEqual(
            self._row_count(
                "idempotency_requests",
                f"tenant_id = 'developer' AND method = 'ADOPT' AND request_key = {_sql_str(cls.key)}",
            ),
            0,
        )

        # Both audit events survive the deleted row.
        events = self._audit_events(cls.alloc_id)
        actions = [e["Action"] for e in events]
        self.assertIn("ADOPT_PLANNED", actions, events)
        abandoned = [e for e in events if e["Action"] == "ADOPT_ABANDONED"]
        self.assertEqual(len(abandoned), 1, events)
        self.assertEqual(abandoned[0]["Actor"], cls.operator, events)

        # The finding is RESOLVED, not merely absent from an OPEN filter.
        self.assertFalse(self._open_adoption_stuck_finding(cls.alloc_id))

        # A fresh reservation in the domain no longer answers domain_busy.
        probe_row = {
            "tenant_id": "developer", "allocation_key": run_key("adopt-abandon-domain-probe-after"),
            "scope": "vpc", "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "cidr": slot22(SLOT_ABANDON_DOMAIN_PROBE), "resource_id": "vpc-abandon-domain-busy-probe-2",
            "netbox_prefix_id": "0",
        }
        probe_report = self._plan_one(probe_row)
        probe_result = probe_report["records"][0]
        self.assertEqual(probe_result["verdict"], "refused", probe_report)
        self.assertNotEqual(probe_result["refusal_code"], "domain_busy", probe_report)

    # =====================================================================
    # 4. a second abandon of the same, now-gone allocation id refuses
    # =====================================================================

    def test_04_second_abandon_of_the_same_id_refuses(self):
        cls = type(self)
        stdout, stderr = self.stack.adopt(
            "abandon", "--allocation-id", cls.alloc_id, "--operator", cls.operator,
            "--reason", "e2e: retry after the first abandon", expect_exit=3,
        )
        document = _report_json(stdout)
        self.assertEqual((document.get("error") or {}).get("code"), "abandon_unknown_allocation", document)
        self.assertNotIn("allocation", document, "a refusal at the fence has no report to print")
        self.assertIn("abandon_unknown_allocation", stderr)
        self.assertEqual(self._row_count("allocations", f"id = {_sql_str(cls.alloc_id)}"), 0)

    # =====================================================================
    # 5. the freed key adopts the same network correctly
    # =====================================================================

    def test_05_adopts_the_same_network_under_the_same_key(self):
        cls = type(self)
        path = f"/tmp/{run_key('adopt-abandon-table')}.csv"
        csv_text = _adopt_csv([{
            "tenant_id": "developer", "allocation_key": cls.key, "scope": "vpc",
            "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "cidr": cls.cidr, "resource_id": cls.resource_id, "netbox_prefix_id": cls.prefix_id,
        }])

        plan_stdout, _ = self.stack.adopt("plan", path, files={path: csv_text})
        plan_report = _report_json(plan_stdout)
        self.assertEqual(plan_report["records"][0]["verdict"], "would_adopt", plan_report)

        apply_stdout, _ = self.stack.adopt(
            "apply", path, "--operator", cls.operator, files={path: csv_text},
        )
        apply_report = _report_json(apply_stdout)
        outcome = apply_report["outcomes"][0]
        self.assertEqual(outcome["status"], "adopted", apply_report)
        cls.new_alloc_id = outcome["allocation_id"]
        self.assertNotEqual(cls.new_alloc_id, cls.alloc_id,
                            "re-adoption under the freed key must be a fresh allocation, not a replay")
        type(self).to_release.append(cls.new_alloc_id)

        # Settle before handing off to whatever test class runs next in this
        # file. reconcileAllocations recomputes unmanaged_occupancy's
        # OPEN/RESOLVED status only on the worker's own periodic tick, not
        # synchronously with this class's writes -- and while this class's
        # allocation was genuinely stuck (test_01-03), the worker did flag
        # cls.resource_id itself as unmanaged_occupancy for one tick (a
        # correct answer: a *stuck*, definitively-refused adoption is not a
        # "recovering" one, so F3's suppression does not apply to it). That
        # finding is stale the instant this apply commits (the resource once
        # once more matches a committed adoption's own durable record, which
        # F3 does suppress), but nothing here forces the worker to recompute
        # it before this method returns. Left unsettled, a sibling test class
        # in this same file that shares ACCOUNT_ID/REGION for its own
        # baseline-relative counting (AdoptE2ETest.test_01) can capture its
        # baseline while this stale finding is still counted, then watch the
        # net open count rise by only 1 instead of 2 when this finding
        # resolves on the same tick its own two new ones appear -- a real
        # interaction, reproduced deterministically and root-caused against
        # the live findings table before this wait was added, not a system
        # timing guess.
        self._wait_until(
            lambda: not self._resource_has_open_unmanaged_finding(cls.resource_id),
            desc="the stale unmanaged_occupancy finding from the stuck window resolves after re-adoption",
        )

        allocation = self.stack.api("GET", f"/v1/allocations/{cls.new_alloc_id}").body
        self.assertEqual(allocation["state"], "RESERVED")
        self.assertEqual(allocation["cidr"], cls.cidr)
        # Committed is never in the public JSON (internal/transport/http.go
        # projects no such field); a 200 here already proves it, since Get
        # answers 404 for anything uncommitted (ADR 0012's own list: "Get,
        # List, Release, pendingRecoveryJobs and the commit guard all test
        # Committed").

    # =====================================================================
    # 6. abandoning the now-committed allocation refuses
    # =====================================================================

    def test_06_abandon_of_the_now_committed_allocation_refuses(self):
        cls = type(self)
        # test_05's Adopt call commits synchronously against the real
        # ledger/NetBox (unlike test_09's seeded recovery scenario, this one
        # runs the real write path start to finish), so the allocation is
        # already committed by the time apply returned; the 200 below is the
        # proof (see test_05's own comment -- Committed is never in the
        # public JSON).
        response = self.stack.api("GET", f"/v1/allocations/{cls.new_alloc_id}")
        self.assertEqual(response.status, 200, response.body)

        before_fields = self.stack.netbox_prefixes()[cls.cidr].get("custom_fields") or {}

        stdout, stderr = self.stack.adopt(
            "abandon", "--allocation-id", cls.new_alloc_id, "--operator", cls.operator,
            "--reason", "e2e: must refuse -- this allocation is committed", expect_exit=3,
        )
        self.assertIn("abandon_committed", stderr)
        # A refusal at the fence has no report, but stdout is still one JSON
        # document naming the refusal.
        document = _report_json(stdout)
        self.assertEqual((document.get("error") or {}).get("code"), "abandon_committed", document)
        self.assertNotIn("allocation", document)

        after_fields = self.stack.netbox_prefixes()[cls.cidr].get("custom_fields") or {}
        self.assertEqual(before_fields.get("platform_allocation_id"), cls.new_alloc_id)
        self.assertEqual(after_fields.get("platform_allocation_id"), cls.new_alloc_id,
                         "the refused abandon must not have touched the prefix's markers")

    # =====================================================================
    # 7. usage: no --reason is a usage error, exit 2
    # =====================================================================

    def test_07_abandon_without_reason_is_usage(self):
        self.stack.adopt(
            "abandon", "--allocation-id", "whatever-id-does-not-matter", "--operator", "alice",
            expect_exit=2,
        )

    # -- helpers specific to this class --------------------------------------

    def _seed_pending_adopt(self, alloc_id, key, allocation_payload, op_id, operation_payload) -> None:
        sql = (
            f"INSERT INTO allocations (id, tenant_id, allocation_key, committed, payload, created_at) "
            f"VALUES ({_sql_str(alloc_id)}, 'developer', {_sql_str(key)}, false, "
            f"$alloc_json${json.dumps(allocation_payload)}$alloc_json$::jsonb, now());\n"
            f"INSERT INTO operations (id, tenant_id, domain_id, payload) "
            f"VALUES ({_sql_str(op_id)}, 'developer', {_sql_str(DOMAIN_ID)}, "
            f"$op_json${json.dumps(operation_payload)}$op_json$::jsonb);\n"
        )
        self._psql(sql)

    def _seed_idempotency_record(self, alloc_id: str, key: str) -> None:
        """The ADOPT idempotency row reserve's first transaction would have
        written, matching internal/storage/postgres.go's persistState and
        internal/service/service.go's idempotencyID (tenant, method, path,
        key) -- request_id is the same sha256 hash, request_hash is a
        placeholder (nothing in this test replays the request body), and the
        payload is the minimal shape internal/domain's own Request stores
        under it."""
        method, path = "ADOPT", "ADOPT /v1/allocations"
        import hashlib as _hashlib
        request_id = _hashlib.sha256(f"developer\x00{method}\x00{path}\x00{key}".encode()).hexdigest()
        payload = {
            "TenantID": "developer", "Method": method, "Path": path, "Key": key,
            "Hash": "e2e-seeded-abandon-idempotency-hash", "AllocationID": alloc_id,
        }
        sql = (
            f"INSERT INTO idempotency_requests "
            f"(request_id, tenant_id, method, path, request_key, request_hash, payload) "
            f"VALUES ({_sql_str(request_id)}, 'developer', {_sql_str(method)}, {_sql_str(path)}, "
            f"{_sql_str(key)}, 'e2e-seeded-abandon-idempotency-hash', "
            f"$idem_json${json.dumps(payload)}$idem_json$::jsonb);\n"
        )
        self._psql(sql)

    def _seed_audit_event(self, event_id: str, alloc_id: str, tenant_id: str, payload: dict) -> None:
        sql = (
            f"INSERT INTO audit_events (id, allocation_id, tenant_id, payload) "
            f"VALUES ({_sql_str(event_id)}, {_sql_str(alloc_id)}, {_sql_str(tenant_id)}, "
            f"$evt_json${json.dumps(payload)}$evt_json$::jsonb);\n"
        )
        self._psql(sql)

    def _mark_prefix_owned(self, prefix_id: str, alloc_id: str, key: str, op_id: str) -> None:
        """Simulate the write internal/netbox's Adopt would have made:
        exactly the custom fields ownedFields writes for an unbound
        (unreleased-binding) vpc-scope allocation, plus status: reserved
        (internal/netbox/adopt.go's own payload shape). Issued with the
        adapter's own NetBox token, which is the token internal/netbox's
        Client already writes with -- there is no separate write-capable
        token in this stack."""
        body = {
            "status": "reserved",
            "custom_fields": {
                "platform_allocation_id": alloc_id,
                "platform_allocation_key": key,
                "platform_operation_id": op_id,
                "platform_parent_allocation_id": "",
                "platform_tenant_id": "developer",
                "platform_environment": ENVIRONMENT,
                "platform_pool_id": POOL_ID,
                "platform_policy_version": "development-1",
                "platform_state": "RESERVED",
                "platform_aws_account_id": ACCOUNT_ID,
                "platform_aws_region": REGION,
                "platform_aws_az_id": "",
            },
        }
        response = self.stack.netbox_as(self.stack.netbox_token, "PATCH", f"/api/ipam/prefixes/{prefix_id}/", body)
        self.assertEqual(response.status, 200, f"seeding the half-converted prefix failed: {response.body}")

    def _resource_has_open_unmanaged_finding(self, resource_id: str) -> bool:
        """resource_id is an operator-only field (internal/transport/http.go's
        publicFinding: "never domain_id, resource_type, resource_id or
        tenant_id" for a tenant caller) -- querying with the default
        (developer, tenant) token would make this vacuously False forever, a
        wait that never actually waits. Query as the operator identity
        instead (package G3b1, Stack.operator_token)."""
        findings = (self.stack.api("GET", "/v1/findings", token=self.stack.operator_token).body or {}).get("items", [])
        return any(
            f.get("code") == "unmanaged_occupancy" and f.get("status") == "OPEN"
            and f.get("resource_id") == resource_id
            for f in findings
        )

    def _open_adoption_stuck_finding(self, allocation_id: str):
        findings = (self.stack.api("GET", "/v1/findings").body or {}).get("items", [])
        for f in findings:
            if f.get("code") == "adoption_stuck" and f.get("allocation_id") == allocation_id and f.get("status") == "OPEN":
                return f
        return None

    def _row_count(self, table: str, where: str) -> int:
        raw = self._psql(f"SELECT count(*) FROM {table} WHERE {where};", tuples_only=True)
        return int(raw.strip())

    def _audit_events(self, allocation_id: str) -> list:
        raw = self._psql(
            f"SELECT payload::text FROM audit_events WHERE allocation_id = {_sql_str(allocation_id)} "
            f"ORDER BY id;",
            tuples_only=True,
        )
        return [json.loads(line) for line in raw.splitlines() if line.strip()]


if __name__ == "__main__":
    unittest.main()
