"""End-to-end coverage for package H4, part one, and package H8d, part two:
a stuck RESERVE is no longer silent (H4), and its consumer can cancel it
once the platform itself has declared it stuck (H8d, ADR 0013).

docs/WORK_PLAN.md's H4 block: a pending RESERVE fences its overlap domain
exactly as a pending ADOPT does -- every reservation there answers
503 domain_busy -- and until H4 `recoverReservations`
(internal/service/worker.go) raised nothing at all on an obstacle it could
not clear. `ReservationStuckE2ETest` below is H4's own scenario, UNCHANGED
by this package: seed a pending RESERVE the way test_e2e_adopt.py's test_09
seeds a pending ADOPT (an uncommitted RESERVED allocation plus a PENDING
operation of the reserve type, written with SQL piped to psql, following
_AdoptFixtureCase's own docstring for why SQL seeding stands in for a real
interruption -- `platform-ipam` returns well under the second it would take
to arrange a deterministic kill), at a CIDR where a NetBox prefix was FIRST
created out of band (unmanaged, no import tag) so `Ensure`'s own
"CIDR already occupied in managed VRF" refusal (internal/netbox/client.go)
is what the worker's recovery classifies as a decision, not uncertainty --
then it repairs the ground truth (removes the out-of-band prefix) and shows
the hold commits and the finding resolves on its own. That is still the
*only* supported exit ReservationStuckE2ETest demonstrates: making the
ground truth match.

docs/WORK_PLAN.md's H8 block and ADR 0013: a stuck RESERVE also has a
*second* exit now, the consumer's own `DELETE /v1/operations/{operation_id}`
/ `client cancel`, available only while the platform has itself declared the
hold stuck. `ReservationCancelE2ETest` below is that scenario, seeded with
the same technique and sharing this module's helpers (refactored into
`_ReservationFixtureCase`, mirroring test_e2e_adopt.py's `_AdoptFixtureCase`)
but never touching `ReservationStuckE2ETest`'s own test or its guarantees.

Quota (docs/WORK_PLAN.md's running note, H4's review note): AdoptE2ETest and
AdoptVPCWithSubnetsE2ETest together use 28 of pool_dev_euc1's 32
per-tenant allocation slots, H2c's own module (AdoptAbandonE2ETest) adds one
more (its re-adopted allocation, left committed forever -- "Quota: 1
allocation net" in that class's own docstring), and this module's own
ReservationStuckE2ETest adds one more (the seeded reservation, quarantined
rather than freed by its own cleanup's release -- countTenant still counts a
QUARANTINED row). That is "about 30 of 32" per H4's review note.
ReservationCancelE2ETest below adds exactly ONE MORE permanent slot, the
same shape as its sibling: its own two seeded stuck reservations
(test_01/test_02's main scenario, and test_07's "prefix already created"
case) are both CANCELLED, which -- unlike release -- deletes the row outright
and returns the slot completely (ADR 0013: "a CANCELLED hold frees its
slot"), so neither costs anything lasting; test_05's fresh re-reservation
under the freed key is left committed rather than released, mirroring
AdoptAbandonE2ETest's own precedent of leaving its final allocation
committed. Net for this whole module, both classes: +2 permanent slots,
bringing the suite to about 32 of 32 -- tight, but within budget, and no
module after this one in the alphabetic pattern `test_e2e_[j-z]*.py` is known
to need headroom this package was told to preserve for.

Address space: ReservationStuckE2ETest's stuck CIDR and probe CIDR are the
two /22 slots lent by test_e2e_adopt.py out of its quadrant, 10.64.128.0/18
(SLOT_RESERVATION_STUCK, SLOT_RESERVATION_STUCK_PROBE there). The first
version of that module used a fixed 10.64.64.0/22, believing that quadrant
unclaimed; it is not -- ordinary reserve() calls fill the pool from the
bottom, a reservation already held that /22 by the time this module ran, and
NetBox refused the out-of-band prefix as a duplicate. A fixed CIDR is still
required, because each hold is seeded directly rather than chosen by
`chooseCIDR`, and any out-of-band or pre-created prefix must sit at exactly
that CIDR. ReservationCancelE2ETest uses two more slots of the same lent
quadrant, indices 13 and 14 -- chosen locally in this module (not added to
test_e2e_adopt.py's own SLOT_* registry, which this package does not touch)
because no module calls `slot22` above index 12 as of 2026-09-21 (checked by
grep across tests/e2e/*.py before choosing them).

This module CANNOT be run by the agent that wrote it: another agent owns the
Compose stack while this module is extended in a private copy alongside it.
It is `python3 -m py_compile`'d and read against test_e2e_adopt.py and
harness.py, and the lead (or a later verification pass) runs it against the
real stack.
"""

from __future__ import annotations

import hashlib
import ipaddress
import json
import secrets
import time
import unittest
from datetime import datetime, timedelta, timezone

from harness import ROOT, compose_argv, run, run_key, stack
from test_e2e_adopt import SLOT_RESERVATION_STUCK, SLOT_RESERVATION_STUCK_PROBE, slot22

POOL_ID = "pool_dev_euc1"
DOMAIN_ID = "local-development"
ACCOUNT_ID = "000000000000"
REGION = "eu-central-1"
ENVIRONMENT = "development"
# deploy/compose/fixtures/pools.yaml: overlap_domains[0].inventory_backend.vrf_id.
VRF_ID = 1
CLOUD_FIXTURE = ROOT / "deploy/compose/fixtures/cloud.json"

# Two /22 slots lent by test_e2e_adopt.py out of its own quadrant
# (10.64.128.0/18), rotated per run exactly as that module rotates its own, so
# the two modules can never pick the same CIDR and ordinary reserve() calls --
# which fill the pool from the bottom -- never reach them. One is seeded as the
# stuck reservation; the other is only ever a scratch CIDR for the read-only
# domain-fence probe below (never imported, never reserved).
STUCK_CIDR = slot22(SLOT_RESERVATION_STUCK)
PROBE_CIDR = slot22(SLOT_RESERVATION_STUCK_PROBE)

# Package H8d's own two slots, chosen locally in this module (module
# docstring above explains why they are not added to test_e2e_adopt.py's
# registry). 13: the main cancel scenario (test_01-test_06). 14: the "prefix
# already created" case (test_07, ADR 0013's "fifth route").
SLOT_RESERVATION_CANCEL = 13
SLOT_RESERVATION_CANCEL_PREFIX_CASE = 14
CANCEL_CIDR = slot22(SLOT_RESERVATION_CANCEL)
CANCEL_PREFIX_CIDR = slot22(SLOT_RESERVATION_CANCEL_PREFIX_CASE)

# adopt plan's input columns (internal/adoptcmd), duplicated here rather than
# imported from test_e2e_adopt.py: this module touches no file that package
# owns, and the shape is small enough that a local copy is clearer than a
# cross-module import between independent e2e suites.
ADOPT_COLUMNS = [
    "tenant_id", "allocation_key", "scope", "environment", "region", "account_id",
    "cidr", "resource_id", "netbox_prefix_id", "parent_allocation_key", "availability_zone_id",
]


def network(cidr: str) -> ipaddress.IPv4Network:
    return ipaddress.ip_network(cidr, strict=True)


def _sql_str(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def _adopt_csv(rows: list) -> str:
    lines = [",".join(ADOPT_COLUMNS)]
    for row in rows:
        lines.append(",".join(str(row.get(c, "")) for c in ADOPT_COLUMNS))
    return "\n".join(lines) + "\n"


def _report_json(stdout: str) -> dict:
    """Parse adopt plan|apply's JSON report out of stdout, skipping any
    leading slog JSON lines main.go's config.Load emits before adoptcmd's
    own pretty-printed report -- test_e2e_adopt.py's _report_json does the
    same, for the same reason (its own docstring explains why)."""
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
    return json.loads("\n".join(lines[i:]))


class _ReservationFixtureCase(unittest.TestCase):
    """Shared machinery for both classes in this module: the fake-cloud
    fixture (backed up and restored byte-identically, per class, mirroring
    test_e2e_adopt.py's `_AdoptFixtureCase`), psql access, and a
    wait-without-sleeping helper. Carries no test_* methods itself, so
    unittest's loader contributes nothing from it directly.
    """

    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()
        cls.original_cloud = CLOUD_FIXTURE.read_bytes()
        cls.original_cloud_hash = hashlib.sha256(cls.original_cloud).hexdigest()
        cls.addClassCleanup(cls._restore_cloud_fixture)
        cls.cloud_resources: dict = {}

    @classmethod
    def _restore_cloud_fixture(cls) -> None:
        CLOUD_FIXTURE.write_bytes(cls.original_cloud)
        got = hashlib.sha256(CLOUD_FIXTURE.read_bytes()).hexdigest()
        if got != cls.original_cloud_hash:
            raise AssertionError(
                "deploy/compose/fixtures/cloud.json was not restored byte-identically: "
                f"expected sha256 {cls.original_cloud_hash}, got {got}"
            )

    # -- fake-cloud fixture (only ReservationCancelE2ETest's case (f) uses
    # this; ReservationStuckE2ETest never calls it, unchanged from before
    # this package) ------------------------------------------------------

    def _flush_cloud(self) -> None:
        payload = {
            "domain_id": DOMAIN_ID, "generation": "development-1", "complete": True,
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

    # -- psql --------------------------------------------------------------

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

    # -- waiting: never a blind sleep --------------------------------------

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

    # -- findings ------------------------------------------------------------

    def _findings(self) -> list:
        # limit=200 (the API's maximum, internal/transport/http.go): other
        # e2e modules may have left many rows behind by the time this module
        # runs, and this scenario's own finding must not be missed to a
        # second page an unpaginated default-limit read would not reach.
        return (self.stack.api("GET", "/v1/findings?limit=200").body or {}).get("items", [])

    def _reservation_stuck_finding(self, allocation_id: str):
        for f in self._findings():
            if f.get("allocation_id") == allocation_id and f.get("code") == "reservation_stuck":
                return f
        return None


class ReservationStuckE2ETest(_ReservationFixtureCase):
    """One scenario, one allocation (the module docstring's quota note).

    UNCHANGED by package H8d: this is H4's own scenario -- the only
    supported exit it demonstrates is making the ground truth match. The
    cancel exit ADR 0013 adds is ReservationCancelE2ETest, below.
    """

    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        cls.prefix_id = None
        cls.allocation_id = None
        cls.addClassCleanup(cls._release_allocation)
        cls.addClassCleanup(cls._remove_leftover_prefix)

    @classmethod
    def _release_allocation(cls) -> None:
        """Leave the domain unfenced, whatever happened in the test.

        Class cleanups run last-registered first, so the out-of-band prefix is
        already gone when this runs. A seeded hold that is still pending keeps
        answering every reservation in the domain 503 domain_busy until the
        worker's next pass commits it -- and an uncommitted allocation cannot be
        released (it answers 404). Returning before that commit hands a fenced
        domain to whichever module runs next: the first run of this module
        failed an assertion, skipped its own wait, and broke
        test_e2e_second_identity's setUpClass that way. So wait for the commit,
        then release; and if the hold never commits, say so loudly instead of
        poisoning the rest of the suite in silence.
        """
        if not cls.allocation_id:
            return
        deadline = time.monotonic() + 120.0
        while True:
            response = cls.stack.api("GET", f"/v1/allocations/{cls.allocation_id}")
            if response.status == 200:
                break
            if time.monotonic() >= deadline:
                raise AssertionError(
                    f"the seeded reservation {cls.allocation_id} never committed after its obstacle was "
                    "removed: the development domain may still be fenced (503 domain_busy) for every "
                    "module that runs after this one")
            time.sleep(3)
        cls.stack.api("DELETE", f"/v1/allocations/{cls.allocation_id}")

    @classmethod
    def _remove_leftover_prefix(cls) -> None:
        # Best-effort: the happy path already removes it mid-test (see
        # test_01), so this only fires if that test failed first.
        if cls.prefix_id:
            try:
                cls.stack.netbox_as(cls.stack.netbox_token, "DELETE",
                                     f"/api/ipam/prefixes/{cls.prefix_id}/")
            except Exception:
                pass

    # -- helpers --------------------------------------------------------

    def _domain_fenced(self) -> bool:
        """True while any pending operation in the domain still fences it
        (internal/service/service.go's pendingDomain), probed the way
        test_e2e_adopt.py's test_09 probes the same thing after recovering a
        pending ADOPT: `adopt plan` checks pendingDomain before anything else
        and inside a read-only ledger view, so this never writes and never
        spends a quota slot, unlike a real reservation attempt would once it
        succeeds. PROBE_CIDR is never imported, so a clear domain still
        refuses the plan -- just not with domain_busy."""
        row = {
            "tenant_id": "developer", "allocation_key": run_key("reservation-stuck-domain-probe"),
            "scope": "vpc", "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "cidr": PROBE_CIDR, "resource_id": "vpc-reservation-stuck-domain-probe",
            "netbox_prefix_id": "0",
        }
        path = f"/tmp/{run_key('reservation-stuck-plan-probe')}.csv"
        csv_text = _adopt_csv([row])
        stdout, _ = self.stack.adopt("plan", path, files={path: csv_text}, expect_exit=3)
        report = _report_json(stdout)
        result = report["records"][0]
        self.assertEqual(result["verdict"], "refused", report)
        return result.get("refusal_code") == "domain_busy"

    # -- the scenario -----------------------------------------------------

    def test_01_a_pending_reservation_wedged_by_an_out_of_band_prefix_raises_reservation_stuck(self):
        cls = type(self)
        key = run_key("reservation-stuck")

        # The obstacle: a NetBox prefix at the held CIDR, created directly
        # against the REST API rather than through `onboard import` -- no
        # import tag, no custom fields, exactly what an operator who created
        # a prefix by hand out of band would leave behind. Ensure's own
        # "CIDR already occupied in managed VRF" refusal
        # (internal/netbox/client.go) is what turns this into a *decision*,
        # not uncertainty, for the worker's classification.
        created = self.stack.netbox_as(
            self.stack.netbox_token, "POST", "/api/ipam/prefixes/",
            {"prefix": STUCK_CIDR, "vrf": VRF_ID, "status": "reserved"},
        )
        self.assertEqual(created.status, 201, created.body)
        cls.prefix_id = created.body["id"]

        alloc_id = "alloc_" + secrets.token_hex(16)
        op_id = "op_" + secrets.token_hex(16)
        cls.allocation_id = alloc_id
        # The operation's own timestamp is what internal/service/worker.go's
        # flagAgedReservation measures against lifecycle.maximum_observation
        # _age_seconds (600 in deploy/compose/fixtures/pools.yaml) before it
        # will raise reservation_stuck at all -- the flapping guard docs/
        # FINDINGS.md and docs/WORK_PLAN.md's H4 block both describe, so a
        # freshly timestamped seed would never be flagged inside this test's
        # own polling window. Backdating it here plays the part a real
        # request's own creation time would have played twenty minutes ago.
        seeded_at = datetime.now(timezone.utc) - timedelta(minutes=20)
        now_iso = seeded_at.isoformat()

        # Rows shaped exactly as the first ledger transaction in
        # internal/service/service.go's reserve() writes them
        # (internal/domain/types.go's Allocation/Operation JSON shapes),
        # mirroring test_e2e_adopt.py's test_09 seeding for an ADOPT
        # operation but with type RESERVE and no adoption record.
        allocation_payload = {
            "allocation_key": key, "scope": "vpc", "environment": ENVIRONMENT,
            "region": REGION, "account_id": ACCOUNT_ID, "address_family": "ipv4",
            "prefix_length": network(STUCK_CIDR).prefixlen, "parent_allocation_id": "",
            "availability_zone_id": "", "description": "", "labels": {},
            "id": alloc_id, "tenant_id": "developer", "domain_id": DOMAIN_ID,
            "request_hash": "e2e-seeded-reservation-stuck-hash", "cidr": STUCK_CIDR,
            "pool_id": POOL_ID, "state": "RESERVED", "revision": 1,
            "policy_version": "development-1", "inventory_id": "", "inventory_sync": "PENDING",
            "binding": None, "created_at": now_iso, "updated_at": now_iso,
            "release_requested_at": None, "quarantine_until": None, "last_observed_at": None,
            "release_blockers": [], "committed": False,
        }
        operation_payload = {
            "id": op_id, "type": "RESERVE", "status": "PENDING", "allocation_id": alloc_id,
            "tenant_id": "developer", "domain_id": DOMAIN_ID, "result": None, "error": None,
            "candidate": None, "adoption": None,
            "created_at": now_iso, "updated_at": now_iso,
        }
        sql = (
            f"INSERT INTO allocations (id, tenant_id, allocation_key, committed, payload, created_at) "
            f"VALUES ({_sql_str(alloc_id)}, 'developer', {_sql_str(key)}, false, "
            f"$alloc_json${json.dumps(allocation_payload)}$alloc_json$::jsonb, now());\n"
            f"INSERT INTO operations (id, tenant_id, domain_id, payload) "
            f"VALUES ({_sql_str(op_id)}, 'developer', {_sql_str(DOMAIN_ID)}, "
            f"$op_json${json.dumps(operation_payload)}$op_json$::jsonb);\n"
        )
        self._psql(sql)

        # Fenced: every reservation in the domain answers 503 domain_busy
        # while the pending RESERVE stands (docs/WORK_PLAN.md's H4 block).
        probe_body = {
            "allocation_key": run_key("reservation-stuck-fence-probe"), "scope": "vpc",
            "environment": ENVIRONMENT, "region": REGION, "account_id": ACCOUNT_ID,
            "prefix_length": 22, "description": "", "labels": {},
        }
        fenced = self.stack.api("POST", "/v1/allocations", probe_body,
                                {"Idempotency-Key": f"e2e-{probe_body['allocation_key']}"})
        self.assertEqual(fenced.status, 503, fenced.body)
        self.assertEqual((fenced.body or {}).get("error", {}).get("code"), "domain_busy", fenced.body)

        found = self._wait_until(
            lambda: self._reservation_stuck_finding(alloc_id),
            desc="the worker raises reservation_stuck",
        )
        self.assertEqual(found["severity"], "CRITICAL", found)
        self.assertEqual(found["status"], "OPEN", found)

        # client findings --fail-if-open sees it and fails the gate for the
        # developer tenant the allocation belongs to (package E1).
        self.stack.cli("findings", "--fail-if-open", expect_exit=7)

        # Ground truth is repaired: remove the out-of-band prefix. The next
        # worker pass converges -- the hold commits, the finding resolves,
        # and the domain answers reservations again (docs/WORK_PLAN.md's H4
        # block: "make the ground truth match").
        removed = self.stack.netbox_as(self.stack.netbox_token, "DELETE",
                                        f"/api/ipam/prefixes/{cls.prefix_id}/")
        self.assertIn(removed.status, (200, 204), removed.body)
        cls.prefix_id = None

        def committed():
            response = self.stack.api("GET", f"/v1/allocations/{alloc_id}")
            return response.body if response.status == 200 else None

        allocation = self._wait_until(committed, desc="the worker commits the recovered reservation")
        self.assertEqual(allocation["state"], "RESERVED")
        self.assertEqual(allocation["cidr"], STUCK_CIDR)

        def resolved():
            f = self._reservation_stuck_finding(alloc_id)
            return f is not None and f.get("status") == "RESOLVED"

        self._wait_until(resolved, desc="the finding resolves once the hold commits")

        # The domain answers reservations again. Probed read-only
        # (_domain_fenced, docstring above) rather than with a second real
        # reservation: this module's own budget is exactly one allocation
        # (the seeded hold above), and Release quarantines rather than frees
        # a quota slot (package G3c's review note), so a second create-then-
        # release here would silently double this module's quota cost.
        self._wait_until(lambda: not self._domain_fenced(), desc="the domain stops answering domain_busy")

        # This scenario's own finding stays resolved (other modules' own
        # open findings, if any, are not this module's concern).
        after = self._reservation_stuck_finding(alloc_id)
        self.assertIsNotNone(after, "reservation_stuck disappeared instead of staying RESOLVED")
        self.assertEqual(after["status"], "RESOLVED", after)


class ReservationCancelE2ETest(_ReservationFixtureCase):
    """`DELETE /v1/operations/{operation_id}` / `client cancel` (package H8d,
    ADR 0013), end to end against the real ledger and NetBox.

    Ordered, like test_e2e_adopt.py's `AdoptAbandonE2ETest`: each test_NN
    builds on the last, for the one main scenario (test_01-test_06). test_07
    is a second, independent scenario for ADR 0013's "fifth route" (case (f)
    of the H8d work order): a reservation whose prefix Ensure had already
    created before the hold got stuck.

    Quota: the main scenario's own seeded hold is CANCELLED (test_03), which
    deletes the ledger row outright and returns its quota slot completely --
    unlike Release, which only quarantines. test_05 then reserves again under
    the freed key and is left COMMITTED (not released), the one permanent
    slot this class spends, mirroring AdoptAbandonE2ETest's own precedent.
    test_07's own seeded hold is also cancelled and returns its slot too, so
    it costs nothing lasting either. Net for this whole class: +1 permanent
    slot (module docstring's accounting).
    """

    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        # (allocation_id, operation_id) pairs this class has seeded via SQL,
        # for the safety-net cleanup below -- every pending hold this class
        # creates must not still be fencing the domain when the class ends,
        # whatever test failed along the way (mirrors
        # ReservationStuckE2ETest's own _release_allocation philosophy: say
        # so loudly rather than poisoning the rest of the suite in silence).
        cls.seeded: list = []
        cls.foreign_prefix_id = None
        # Registered first, so it runs LAST (class cleanups run in reverse): the
        # out-of-band prefix is what the tests assert a cancel leaves untouched, and once they are over it is
        # this class's own litter. The suite's reset clears managed and
        # imported prefixes only, and the lent slots rotate with the run id, so
        # an unmanaged prefix left behind would sit in some later run's adopt
        # slot on a long-lived development stack.
        cls.addClassCleanup(cls._remove_foreign_prefix)
        cls.addClassCleanup(cls._ensure_nothing_left_pending)

    @classmethod
    def _remove_foreign_prefix(cls) -> None:
        if cls.foreign_prefix_id is None:
            return
        gone = cls.stack.netbox_as(cls.stack.netbox_token, "DELETE",
                                   f"/api/ipam/prefixes/{cls.foreign_prefix_id}/")
        if gone.status not in (204, 404):
            raise AssertionError(
                f"the out-of-band prefix {cls.foreign_prefix_id} could not be removed "
                f"(HTTP {gone.status}); it will occupy a rotating slot of a later run")

    @classmethod
    def _ensure_nothing_left_pending(cls) -> None:
        problems = []
        for alloc_id, op_id in cls.seeded:
            response = cls.stack.api("GET", f"/v1/allocations/{alloc_id}")
            if response.status == 200:
                continue  # committed -- not this cleanup's concern
            if response.status == 404:
                continue  # already cancelled and gone
            # 409 allocation_pending (or anything else): still pending. Try
            # to cancel it, waiting briefly for the finding if a failed
            # assertion left the test before it opened.
            deadline = time.monotonic() + 90.0
            cancelled = False
            while time.monotonic() < deadline:
                result = cls.stack.api("DELETE", f"/v1/operations/{op_id}")
                if result.status == 200:
                    cancelled = True
                    break
                code = (result.body or {}).get("error", {}).get("code")
                if result.status == 409 and code == "reservation_not_stuck":
                    time.sleep(3)
                    continue
                break
            if not cancelled:
                problems.append((alloc_id, op_id))
        if problems:
            raise AssertionError(
                "the following seeded reservations could not be cancelled during cleanup and may "
                f"still be fencing the domain for the next module: {problems}"
            )

    # -- seeding helpers, shaped like ReservationStuckE2ETest's own test_01
    # but factored so both scenarios in this class can reuse them ----------

    def _seed_pending_reserve(self, *, cidr: str, key: str, alloc_id: str, op_id: str,
                              minutes_old: int = 20) -> None:
        """Seed the two ledger rows internal/service/service.go's reserve()
        writes in its first transaction, backdated so flagAgedReservation's
        age gate (lifecycle.maximum_observation_age_seconds, 600 in
        deploy/compose/fixtures/pools.yaml) is already satisfied --
        ReservationStuckE2ETest's test_01 seeds the same shape inline; this
        is the same technique, factored for reuse by this class's two
        scenarios."""
        seeded_at = datetime.now(timezone.utc) - timedelta(minutes=minutes_old)
        now_iso = seeded_at.isoformat()
        allocation_payload = {
            "allocation_key": key, "scope": "vpc", "environment": ENVIRONMENT,
            "region": REGION, "account_id": ACCOUNT_ID, "address_family": "ipv4",
            "prefix_length": network(cidr).prefixlen, "parent_allocation_id": "",
            "availability_zone_id": "", "description": "", "labels": {},
            "id": alloc_id, "tenant_id": "developer", "domain_id": DOMAIN_ID,
            "request_hash": "e2e-seeded-reservation-cancel-hash", "cidr": cidr,
            "pool_id": POOL_ID, "state": "RESERVED", "revision": 1,
            "policy_version": "development-1", "inventory_id": "", "inventory_sync": "PENDING",
            "binding": None, "created_at": now_iso, "updated_at": now_iso,
            "release_requested_at": None, "quarantine_until": None, "last_observed_at": None,
            "release_blockers": [], "committed": False,
        }
        operation_payload = {
            "id": op_id, "type": "RESERVE", "status": "PENDING", "allocation_id": alloc_id,
            "tenant_id": "developer", "domain_id": DOMAIN_ID, "result": None, "error": None,
            "candidate": None, "adoption": None,
            "created_at": now_iso, "updated_at": now_iso,
        }
        sql = (
            f"INSERT INTO allocations (id, tenant_id, allocation_key, committed, payload, created_at) "
            f"VALUES ({_sql_str(alloc_id)}, 'developer', {_sql_str(key)}, false, "
            f"$alloc_json${json.dumps(allocation_payload)}$alloc_json$::jsonb, now());\n"
            f"INSERT INTO operations (id, tenant_id, domain_id, payload) "
            f"VALUES ({_sql_str(op_id)}, 'developer', {_sql_str(DOMAIN_ID)}, "
            f"$op_json${json.dumps(operation_payload)}$op_json$::jsonb);\n"
        )
        self._psql(sql)
        type(self).seeded.append((alloc_id, op_id))

    def _seed_idempotency_record(self, alloc_id: str, op_id: str, key: str) -> None:
        """The POST idempotency row reserve's first transaction would have
        written (internal/service/service.go's idempotencyID(tenant, method,
        path, key), internal/domain's Idempotency struct) -- mirrors
        test_e2e_adopt.py's own _seed_idempotency_record for ADOPT."""
        method, path = "POST", "POST /v1/allocations"
        request_id = hashlib.sha256(f"developer\x00{method}\x00{path}\x00{key}".encode()).hexdigest()
        payload = {
            "TenantID": "developer", "Method": method, "Path": path, "Key": key,
            "Hash": "e2e-seeded-reservation-cancel-idempotency-hash",
            "AllocationID": alloc_id, "OperationID": op_id,
        }
        sql = (
            f"INSERT INTO idempotency_requests "
            f"(request_id, tenant_id, method, path, request_key, request_hash, payload) "
            f"VALUES ({_sql_str(request_id)}, 'developer', {_sql_str(method)}, {_sql_str(path)}, "
            f"{_sql_str(key)}, 'e2e-seeded-reservation-cancel-idempotency-hash', "
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

    def _prefixes_marked(self, alloc_id: str) -> list:
        """NetBox prefixes carrying alloc_id's marker, filtered locally.

        internal/netbox/client.go's findByMarker comment: "Some NetBox
        releases do not filter custom fields unless the query uses
        cf_<name>. The endpoint may still return all records; filter again
        locally." -- this test does the same, rather than trusting the
        server-side `count` the query string asked for.
        """
        response = self.stack.netbox(f"/api/ipam/prefixes/?cf_platform_allocation_id={alloc_id}&limit=200")
        self.assertEqual(response.status, 200, response.body)
        results = (response.body or {}).get("results", [])
        return [r for r in results if (r.get("custom_fields") or {}).get("platform_allocation_id") == alloc_id]

    # =====================================================================
    # main scenario: seed, refuse-before-open, refuse-other-principals,
    # cancel, repeat converges, the key reserves again, CLI refusal
    # =====================================================================

    def test_01_seeds_a_stuck_reservation_and_refuses_a_cancel_before_the_finding_opens(self):
        cls = type(self)
        cls.key = run_key("reservation-cancel")
        cls.alloc_id = "alloc_" + secrets.token_hex(16)
        cls.op_id = "op_" + secrets.token_hex(16)

        # The obstacle, exactly as ReservationStuckE2ETest's test_01 above:
        # an out-of-band NetBox prefix at the held CIDR, no import tag, no
        # ownership fields.
        created = self.stack.netbox_as(
            self.stack.netbox_token, "POST", "/api/ipam/prefixes/",
            {"prefix": CANCEL_CIDR, "vrf": VRF_ID, "status": "reserved"},
        )
        self.assertEqual(created.status, 201, created.body)
        cls.foreign_prefix_id = created.body["id"]

        self._seed_pending_reserve(cidr=CANCEL_CIDR, key=cls.key, alloc_id=cls.alloc_id, op_id=cls.op_id)
        self._seed_idempotency_record(cls.alloc_id, cls.op_id, cls.key)
        planned_event_id = "evt_" + secrets.token_hex(16)
        seeded_at_iso = datetime.now(timezone.utc).isoformat()
        self._seed_audit_event(planned_event_id, cls.alloc_id, "developer", {
            "ID": planned_event_id, "AllocationID": cls.alloc_id, "TenantID": "developer",
            "Actor": "developer", "Action": "RESERVE_PLANNED",
            "Reason": f"cancel test seed: reservation planned for {CANCEL_CIDR} under key {cls.key!r}",
            "At": seeded_at_iso, "Revision": 1,
        })

        # (a) BEFORE the finding is open: the very next worker pass is what
        # would raise reservation_stuck (the operation's timestamp is
        # already backdated past the age gate), so this has to run
        # immediately after the SQL insert, with no wait in between, to land
        # inside the window before that pass has run. Refused
        # 409 reservation_not_stuck, and nothing changed.
        before = self.stack.api("DELETE", f"/v1/operations/{cls.op_id}")
        self.assertEqual(before.status, 409, before.body)
        self.assertEqual((before.body or {}).get("error", {}).get("code"), "reservation_not_stuck", before.body)

        still_pending = self.stack.api("GET", f"/v1/operations/{cls.op_id}")
        self.assertEqual(still_pending.status, 200, still_pending.body)
        self.assertEqual(still_pending.body.get("status"), "PENDING", still_pending.body)

        # E3 closed the mismatch H8d's review found (docs/WORK_PLAN.md's E3
        # block; ADR 0013's dated 2026-09-21 note): docs/API_V1.md section 5
        # promises 409 allocation_pending for an uncommitted allocation with a
        # known PENDING operation, and internal/service/service.go's Get now
        # builds it -- for the OWNING TENANT only, retryable, with the
        # operation id under error.details.operation_id.
        still_there = self.stack.api("GET", f"/v1/allocations/{cls.alloc_id}")
        self.assertEqual(still_there.status, 409, still_there.body)
        self.assertEqual((still_there.body or {}).get("error", {}).get("code"),
                         "allocation_pending", still_there.body)
        self.assertTrue((still_there.body or {}).get("error", {}).get("retryable"), still_there.body)
        self.assertEqual(
            (still_there.body or {}).get("error", {}).get("details", {}).get("operation_id"),
            cls.op_id, still_there.body,
        )
        # ADR 0011: an operator never sees the hold through a different
        # status than before E3 -- it stays 404, byte-identical (modulo
        # request_id) to an unknown id. ops-observer (eligible for no pool)
        # gets the same tenant-scoped 404 any other tenant would.
        operator_read = self.stack.api("GET", f"/v1/allocations/{cls.alloc_id}", token=self.stack.operator_token)
        self.assertEqual(operator_read.status, 404, operator_read.body)
        self.assertEqual((operator_read.body or {}).get("error", {}).get("code"),
                         "not_found", operator_read.body)
        ops_observer_read = self.stack.api("GET", f"/v1/allocations/{cls.alloc_id}", token=self.stack.ops_token)
        self.assertEqual(ops_observer_read.status, 404, ops_observer_read.body)
        self.assertEqual((ops_observer_read.body or {}).get("error", {}).get("code"),
                         "not_found", ops_observer_read.body)
        self.assertEqual(self._row_count("allocations", f"id = {_sql_str(cls.alloc_id)}"), 1)

        found = self._wait_until(
            lambda: self._reservation_stuck_finding(cls.alloc_id),
            desc="the worker raises reservation_stuck",
        )
        self.assertEqual(found["severity"], "CRITICAL", found)
        self.assertEqual(found["status"], "OPEN", found)
        cls.capacity_before_cancel = self.stack.capacity(POOL_ID)["by_prefix_length"]["22"]["pending"]

    @staticmethod
    def _error_shape(body: dict) -> dict:
        """code + message only: request_id is unique per request by design
        (X-Request-ID, docs/API_V1.md section 1) and would make two answers
        to the very same refusal compare unequal for a reason that has
        nothing to do with what the caller learned."""
        error = (body or {}).get("error") or {}
        return {"code": error.get("code"), "message": error.get("message")}

    def test_02_another_tenant_and_the_operator_get_404(self):
        cls = type(self)
        unknown = self.stack.api("DELETE", "/v1/operations/op_" + "f" * 32)
        self.assertEqual(unknown.status, 404, unknown.body)
        self.assertEqual((unknown.body or {}).get("error", {}).get("code"), "not_found", unknown.body)

        other_tenant = self.stack.api("DELETE", f"/v1/operations/{cls.op_id}", token=self.stack.ops_token)
        self.assertEqual(other_tenant.status, 404, other_tenant.body)
        self.assertEqual(self._error_shape(other_tenant.body), self._error_shape(unknown.body),
                         "another tenant's cancel must leak nothing an unknown id did not")

        operator = self.stack.api("DELETE", f"/v1/operations/{cls.op_id}", token=self.stack.operator_token)
        self.assertEqual(operator.status, 404, operator.body)
        self.assertEqual(self._error_shape(operator.body), self._error_shape(unknown.body),
                         "an operator's cancel must be refused exactly like an unknown id, never granted")

        # Nothing changed: the finding is still open, the operation is
        # still pending.
        still_open = self._reservation_stuck_finding(cls.alloc_id)
        self.assertIsNotNone(still_open)
        self.assertEqual(still_open["status"], "OPEN", still_open)

    def test_03_the_owning_tenant_cancels_through_the_cli(self):
        cls = type(self)
        report = self.stack.cli("cancel", "--operation-id", cls.op_id)

        self.assertTrue(report["fenced"], report)
        self.assertFalse(report["already_fenced"], report)
        self.assertTrue(report["removed"], report)
        self.assertTrue(report["deleted"], report)
        self.assertTrue(report["finding_resolved"], report)
        self.assertEqual(report["allocation"]["id"], cls.alloc_id, report)
        self.assertEqual(report["allocation"]["allocation_key"], cls.key, report)
        self.assertFalse(report["allocation"]["committed"], report)
        self.assertEqual(report["operation"]["id"], cls.op_id, report)
        self.assertEqual(report["operation"]["type"], "RESERVE", report)
        self.assertEqual(report["operation"]["status"], "FAILED", report)

        # No prefix anywhere carries this allocation's marker any more.
        self.assertEqual(self._prefixes_marked(cls.alloc_id), [])

        # The foreign prefix that caused the refusal is UNTOUCHED: this
        # cancel never had a reason to remove ground truth it did not write,
        # and it did not.
        foreign = self.stack.netbox_as(self.stack.netbox_token, "GET",
                                       f"/api/ipam/prefixes/{cls.foreign_prefix_id}/")
        self.assertEqual(foreign.status, 200, foreign.body)
        self.assertEqual(foreign.body.get("prefix"), CANCEL_CIDR, foreign.body)
        self.assertEqual(foreign.body.get("custom_fields", {}).get("platform_allocation_id"), None,
                         "the foreign prefix must not have gained a marker")
        self.assertEqual(foreign.body.get("tags"), [], "the foreign prefix must keep its (empty) tag list")

        # GET /v1/operations/{id} keeps answering the FAILED operation.
        operation = self.stack.api("GET", f"/v1/operations/{cls.op_id}")
        self.assertEqual(operation.status, 200, operation.body)
        self.assertEqual(operation.body.get("status"), "FAILED", operation.body)
        self.assertEqual((operation.body.get("error") or {}).get("code"), "reservation_cancelled", operation.body)

        # The allocation row is gone.
        allocation = self.stack.api("GET", f"/v1/allocations/{cls.alloc_id}")
        self.assertEqual(allocation.status, 404, allocation.body)
        self.assertEqual(self._row_count("allocations", f"id = {_sql_str(cls.alloc_id)}"), 0)

        # The finding is RESOLVED, not merely absent from an OPEN filter,
        # and no longer costs the developer tenant client findings
        # --fail-if-open (test_01 above already showed the OPEN case costs
        # it exit 7; H4's own test does the equivalent check for its exit).
        resolved = self._reservation_stuck_finding(cls.alloc_id)
        self.assertIsNotNone(resolved)
        self.assertEqual(resolved["status"], "RESOLVED", resolved)

        # The POST idempotency record naming this allocation is gone: a
        # retry under the same key must be a fresh reservation, not a
        # replay of the cancelled attempt or a 503 ledger_error (ADR 0013).
        self.assertEqual(
            self._row_count(
                "idempotency_requests",
                f"tenant_id = 'developer' AND method = 'POST' AND request_key = {_sql_str(cls.key)}",
            ),
            0,
        )

        # Both audit events survive the deleted row.
        events = self._audit_events(cls.alloc_id)
        actions = [e["Action"] for e in events]
        self.assertIn("RESERVE_PLANNED", actions, events)
        cancelled = [e for e in events if e["Action"] == "RESERVE_CANCELLED"]
        self.assertEqual(len(cancelled), 1, events)
        self.assertEqual(cancelled[0]["Actor"], "developer", events)

        # Capacity reflects the freed slot: capacity_when_complete could not
        # be used for the "before" reading in test_01 -- internal/service/
        # capacity.go marks `complete: false` for the whole pool while any
        # operation fences its domain (pendingDomain), so a pending hold
        # here made completeness structurally unreachable until this cancel.
        after = self.stack.capacity_when_complete(POOL_ID)
        self.assertLess(after["by_prefix_length"]["22"]["pending"], cls.capacity_before_cancel,
                        "the cancelled hold's /22 must no longer count as pending capacity")

    def test_04_a_repeat_cancel_converges(self):
        cls = type(self)
        again = self.stack.api("DELETE", f"/v1/operations/{cls.op_id}")
        self.assertEqual(again.status, 200, again.body)
        self.assertFalse(again.body["fenced"], again.body)
        self.assertTrue(again.body["already_fenced"], again.body)
        self.assertFalse(again.body["removed"], again.body)
        self.assertTrue(again.body["deleted"], again.body)
        self.assertFalse(again.body["finding_resolved"], again.body)

    def test_05_the_same_key_reserves_again_with_a_new_allocation_id(self):
        """(d): the freed allocation key is reusable. The new hold is left
        committed rather than released -- this class's own quota note
        explains why, mirroring AdoptAbandonE2ETest's own precedent."""
        cls = type(self)
        allocation = self.stack.reserve(cls.key, 22)
        self.assertNotEqual(allocation["id"], cls.alloc_id, allocation)
        self.assertEqual(allocation["allocation_key"], cls.key, allocation)
        self.assertIn(allocation["state"], ("RESERVED", "ACTIVE"), allocation)
        cls.reserved_again_id = allocation["id"]

    def test_06_cli_cancel_refuses_an_unknown_operation_id_with_exit_5(self):
        """(e), the refusal half: one JSON document, exit 5, an error
        envelope. The success half is test_03 above (the same CLI verb,
        exit 0, the cancel report)."""
        unknown_id = "op_" + "0" * 32
        document = self.stack.cli("cancel", "--operation-id", unknown_id, expect_exit=5)
        self.assertEqual((document.get("error") or {}).get("code"), "not_found", document)

    # =====================================================================
    # case (f): the reservation's prefix WAS already created
    # =====================================================================

    def test_07_a_reservation_whose_prefix_was_already_created_is_deleted_by_cancel(self):
        """ADR 0013's "fifth route": Ensure created the prefix, the commit
        was lost, and a foreign cloud resource then appears on the CIDR
        before the worker calls Ensure again -- so recoverReservations's
        pendingReservationObservationSafe check refuses before Ensure is
        ever reached (internal/service/worker.go), and the hold is stuck
        with its prefix already carrying this allocation's own markers.

        Manufactured honestly, without hand-editing the ledger beyond the
        same SQL-seeding technique test_01 above already uses: the prefix is
        created carrying this allocation's own ownership fields in exactly
        the shape internal/netbox/client.go's Ensure POSTs them
        (ownedFields), and a foreign, untagged VPC resource is added to the
        fake-cloud fixture at the same CIDR so the observation guard, not
        the exact-CIDR guard, is what makes the worker classify this as a
        decision.
        """
        cls = type(self)
        cidr = CANCEL_PREFIX_CIDR
        key = run_key("reservation-cancel-prefix-created")
        alloc_id = "alloc_" + secrets.token_hex(16)
        op_id = "op_" + secrets.token_hex(16)

        owned_fields = {
            "platform_allocation_id": alloc_id, "platform_allocation_key": key,
            "platform_operation_id": op_id, "platform_parent_allocation_id": "",
            "platform_tenant_id": "developer", "platform_environment": ENVIRONMENT,
            "platform_pool_id": POOL_ID, "platform_policy_version": "development-1",
            "platform_state": "RESERVED", "platform_aws_account_id": ACCOUNT_ID,
            "platform_aws_region": REGION, "platform_aws_az_id": "",
        }
        created = self.stack.netbox_as(
            self.stack.netbox_token, "POST", "/api/ipam/prefixes/",
            {"prefix": cidr, "vrf": VRF_ID, "status": "reserved", "custom_fields": owned_fields},
        )
        self.assertEqual(created.status, 201, created.body)
        prefix_id = created.body["id"]

        resource_id = "vpc-" + secrets.token_hex(8)
        self._put_resource({
            "account_id": ACCOUNT_ID, "region": REGION, "type": "vpc",
            "id": resource_id, "cidr": cidr, "cidrs": [cidr], "tags": {},
        })

        self._seed_pending_reserve(cidr=cidr, key=key, alloc_id=alloc_id, op_id=op_id)

        found = self._wait_until(
            lambda: self._reservation_stuck_finding(alloc_id),
            desc="the worker raises reservation_stuck via the observation route, "
                 "never having called Ensure",
        )
        self.assertEqual(found["severity"], "CRITICAL", found)
        self.assertEqual(found["status"], "OPEN", found)

        cancelled = self.stack.api("DELETE", f"/v1/operations/{op_id}")
        self.assertEqual(cancelled.status, 200, cancelled.body)
        self.assertTrue(cancelled.body["removed"], cancelled.body)
        self.assertTrue(cancelled.body["deleted"], cancelled.body)
        self.assertTrue(cancelled.body["finding_resolved"], cancelled.body)

        # The prefix Ensure would have created -- and that this test
        # pre-created in exactly that shape -- is DELETED, not merely
        # cleared: a reservation's prefix belongs to nothing else
        # (ADR 0013, internal/service/cancel.go's own header comment).
        gone = self.stack.netbox_as(self.stack.netbox_token, "GET", f"/api/ipam/prefixes/{prefix_id}/")
        self.assertEqual(gone.status, 404, gone.body)
        self.assertEqual(self._prefixes_marked(alloc_id), [])

        self._remove_resource(resource_id)


if __name__ == "__main__":
    unittest.main()
