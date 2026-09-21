package service

// Package G3b2 (ADR 0011 stage two) grants an operator four reads and nothing
// else. An operator is a principal with no tenant, so every comparison in this
// package refuses it before any new code runs; each grant below is one local
// branch at exactly the comparison it relaxes. These tests assert each grant
// positively, assert that everything not granted is still refused by the
// unchanged check that refuses it, and assert that a tenant's own view did not
// move -- the failure mode this shape is chosen for is an operator seeing too
// little, never a tenant seeing too much.

import (
	"context"
	"encoding/json"
	"go/ast"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

// operatorConfig extends safetyConfig with the two things the grants need to
// be visible: a second eligible tenant in pool "p", and a second pool that no
// identity this fixture knows is eligible for, so "every configured pool" is
// distinguishable from "every pool this caller could reserve from".
func operatorConfig() domain.Config {
	cfg := safetyConfig()
	cfg.Lifecycle = domain.Lifecycle{QuarantineHours: 1, RequiredAbsenceScans: 2, MinScanSpacing: 300, MaxObservationAge: 3600}
	cfg.Pools[0].EligibleTenants = []string{"t", "u"}
	cfg.Pools[0].ExcludedCIDRs = []string{"10.200.0.0/24"}
	cfg.Pools = append(cfg.Pools, domain.Pool{
		ID: "p-locked", DomainID: "d", CIDR: "172.16.0.0/12", AddressFamily: "ipv4",
		Scope: "vpc", Environment: "staging", Region: "eu",
		EligibleTenants: []string{"nobody"}, EligibleAccounts: []string{"999999999999"},
		AllowedPrefixLengths: []int{24},
	})
	return cfg
}

// operatorPrincipal is the whole of the role: a subject and nothing else. No
// tenant, no accounts, no environments, no regions -- internal/config.Validate
// refuses an operator that carries any of them.
func operatorPrincipal() domain.Principal {
	return domain.Principal{Subject: "ops:alice", Role: domain.OperatorRole}
}

func tenantUPrincipal() domain.Principal {
	return domain.Principal{
		Subject: "u:carol", TenantID: "u", Accounts: []string{"123456789012"},
		Environments: []string{"prod"}, Regions: []string{"eu"},
	}
}

func operatorAllocation(now time.Time, id, tenant, scope, cidr, state string, committed bool) domain.Allocation {
	a := safetyAllocation(now, id, scope, cidr, "", "")
	a.TenantID, a.State, a.Committed = tenant, state, committed
	if !committed {
		a.InventoryID, a.InventorySync = "", "PENDING"
	}
	if scope == "subnet" {
		a.PrefixLength = 26
	}
	return a
}

// operatorFixture writes the state every grant is read against: tenant "t"'s
// committed allocation and its UNCOMMITTED hold, and -- when withTenantU --
// tenant "u"'s committed allocation and a subnet-scope row of the same tenant.
// These are raw ledger rows rather than the product of Reserve, so the subnet
// carries no parent and sits in a block of its own; that is deliberate, since
// its only job is to show that dropping the tenant conjunct from capacity's
// lifecycle breakdown left `a.Scope == "vpc"` beside it untouched.
//
// It also records one trusted observation, so capacity can answer complete,
// and one finding per tenant.
func operatorFixture(t *testing.T, now time.Time, withTenantU bool) (*Service, *storage.MemoryLedger) {
	t.Helper()
	ledger := storage.NewMemoryLedger()
	s := New(operatorConfig(), ledger, &safetyInventory{}, &safetyObserver{observation: safetyObservation(now)})
	s.SetClock(func() time.Time { return now })
	rows := []domain.Allocation{
		operatorAllocation(now, "alloc-t-vpc", "t", "vpc", "10.1.0.0/24", domain.Reserved, true),
		operatorAllocation(now, "alloc-t-hold", "t", "vpc", "10.3.0.0/24", domain.Reserved, false),
	}
	findings := []domain.Finding{{ID: "find-t", TenantID: "t", DomainID: "d", Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN", AccountID: "123456789012", Region: "eu", FirstObservedAt: now, LastObservedAt: now}}
	if withTenantU {
		rows = append(rows,
			operatorAllocation(now, "alloc-u-vpc", "u", "vpc", "10.2.0.0/24", domain.Active, true),
			operatorAllocation(now, "alloc-u-subnet", "u", "subnet", "10.9.0.0/26", domain.Reserved, true),
		)
		findings = append(findings, domain.Finding{ID: "find-u", TenantID: "u", DomainID: "d", Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN", AccountID: "123456789012", Region: "eu", FirstObservedAt: now, LastObservedAt: now})
	}
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		for _, a := range rows {
			st.Allocations[a.ID] = a
		}
		for _, f := range findings {
			st.Findings[f.ID] = f
		}
		st.Observations["d"] = []domain.Observation{safetyObservation(now)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, ledger
}

func operatorNow() time.Time { return time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC) }

func allocationIDs(as []domain.Allocation) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// -- the grants --------------------------------------------------------------

// TestOperatorListsCommittedAllocationsAcrossTenants covers both halves of the
// List grant: every tenant's committed allocation is visible, and an
// uncommitted hold is not -- the `a.Committed` conjunct is untouched, so a
// hold is exactly as invisible to an operator as it is to the tenant holding it.
func TestOperatorListsCommittedAllocationsAcrossTenants(t *testing.T) {
	s, _ := operatorFixture(t, operatorNow(), true)
	got, err := s.List(context.Background(), operatorPrincipal())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"alloc-t-vpc", "alloc-u-subnet", "alloc-u-vpc"}
	if !sameStrings(allocationIDs(got), want) {
		t.Fatalf("operator list = %v, want %v (sorted by id, both tenants, no uncommitted hold)", allocationIDs(got), want)
	}
	// The same list, read twice, must come back in the same order: the
	// transport pages by sorting on the id and advancing on `id > cursor`, so
	// an order that varied between calls would skip or repeat rows.
	again, err := s.List(context.Background(), operatorPrincipal())
	if err != nil || !sameStrings(allocationIDs(again), want) {
		t.Fatalf("operator list is not stable between calls: %v (err=%v)", allocationIDs(again), err)
	}
}

func TestOperatorGetsAnyCommittedAllocationButNotAHold(t *testing.T) {
	s, _ := operatorFixture(t, operatorNow(), true)
	for _, id := range []string{"alloc-t-vpc", "alloc-u-vpc", "alloc-u-subnet"} {
		a, err := s.Get(context.Background(), operatorPrincipal(), id)
		if err != nil || a.ID != id {
			t.Fatalf("operator Get(%q) = %#v, err=%v", id, a, err)
		}
	}
	// internal/service/service.go Get: `!a.Committed` is unchanged, so the hold
	// is 404 for the operator exactly as it is for tenant "t" itself.
	_, err := s.Get(context.Background(), operatorPrincipal(), "alloc-t-hold")
	safetyAPIError(t, err, 404, "not_found")
	_, err = s.Get(context.Background(), safetyPrincipal(), "alloc-t-hold")
	safetyAPIError(t, err, 404, "not_found")
	_, err = s.Get(context.Background(), operatorPrincipal(), "alloc-does-not-exist")
	safetyAPIError(t, err, 404, "not_found")
}

func TestOperatorReceivesEveryConfiguredPool(t *testing.T) {
	s, _ := operatorFixture(t, operatorNow(), true)
	pools, err := s.Pools(context.Background(), operatorPrincipal())
	if err != nil {
		t.Fatalf("pools: %v", err)
	}
	ids := []string{}
	for _, pool := range pools {
		ids = append(ids, pool.ID)
	}
	// "p-locked" lists an eligible tenant no identity in this fixture has, so
	// it is in the answer only because an operator gets every configured pool.
	if !sameStrings(ids, []string{"p", "p-locked"}) {
		t.Fatalf("operator pools = %v, want every configured pool", ids)
	}
	// Because the list is not empty, internal/transport/http.go's stage-one
	// refusal (403 no_eligible_pool, decided there on this very list) no longer
	// applies to an operator -- without that handler changing.
	if len(pools) == 0 {
		t.Fatal("an empty pool list would make the transport refuse an operator")
	}
	tenant, err := s.Pools(context.Background(), safetyPrincipal())
	if err != nil || len(tenant) != 1 || tenant[0].ID != "p" {
		t.Fatalf("tenant pools = %#v, err=%v; the grant must not widen a tenant's answer", tenant, err)
	}
}

// TestOperatorCapacityIsTheEstateNotAnEntitlement asserts the two halves of
// the capacity grant separately, because they are different claims. The
// occupied set and `allocatable` were ALREADY domain-wide (ADR 0011: "the same
// number for everybody"), so they must be byte-identical for an operator and
// for an eligible tenant on the same state; only the lifecycle breakdown was
// tenant-scoped, and for an operator it now covers both tenants.
func TestOperatorCapacityIsTheEstateNotAnEntitlement(t *testing.T) {
	now := operatorNow()
	s, _ := operatorFixture(t, now, true)
	ctx := context.Background()

	operator, err := s.Capacity(ctx, operatorPrincipal(), "p")
	if err != nil {
		t.Fatalf("operator capacity: %v", err)
	}
	tenant, err := s.Capacity(ctx, safetyPrincipal(), "p")
	if err != nil {
		t.Fatalf("tenant capacity: %v", err)
	}
	if operator["complete"] != true || tenant["complete"] != true {
		t.Fatalf("fixture did not produce a complete capacity answer: operator=%v tenant=%v", operator["complete"], tenant["complete"])
	}
	if operator["pool_id"] != tenant["pool_id"] || operator["observed_at"] != tenant["observed_at"] {
		t.Fatalf("pool identity or observation time differs: operator=%v tenant=%v", operator, tenant)
	}

	operatorBuckets := operator["by_prefix_length"].(map[string]any)["24"].(map[string]uint64)
	tenantBuckets := tenant["by_prefix_length"].(map[string]any)["24"].(map[string]uint64)

	// The domain-wide half. 10.0.0.0/8 holds 65536 blocks of /24; the occupied
	// set is the excluded 10.200.0.0/24 plus every non-released allocation in
	// the domain irrespective of tenant or commit state -- 10.1.0.0/24,
	// 10.3.0.0/24 (the hold), 10.2.0.0/24 and the /26 inside 10.9.0.0/24.
	if operatorBuckets["allocatable"] != 65531 {
		t.Fatalf("allocatable = %d, want 65531", operatorBuckets["allocatable"])
	}
	if operatorBuckets["allocatable"] != tenantBuckets["allocatable"] {
		t.Fatalf("allocatable differs between an operator and an eligible tenant: %d vs %d", operatorBuckets["allocatable"], tenantBuckets["allocatable"])
	}
	if operatorBuckets["excluded"] != tenantBuckets["excluded"] {
		t.Fatalf("excluded differs between an operator and an eligible tenant: %d vs %d", operatorBuckets["excluded"], tenantBuckets["excluded"])
	}

	// The breakdown. Tenant "t" sees its own reserved allocation and its own
	// hold; the operator sees those plus tenant "u"'s active allocation, and
	// tenant "u"'s subnet-scope row in neither, because `a.Scope == "vpc"` is
	// unchanged.
	for name, want := range map[string]uint64{"reserved": 1, "active": 0, "pending": 1, "quarantined": 0, "excluded": 1} {
		if tenantBuckets[name] != want {
			t.Fatalf("tenant %s = %d, want %d", name, tenantBuckets[name], want)
		}
	}
	for name, want := range map[string]uint64{"reserved": 1, "active": 1, "pending": 1, "quarantined": 0, "excluded": 1} {
		if operatorBuckets[name] != want {
			t.Fatalf("operator %s = %d, want %d (the breakdown must cover both tenants' vpc allocations)", name, operatorBuckets[name], want)
		}
	}
	// The union claim, stated over the data rather than over the numbers: what
	// the operator sees in a category is what tenant "t" sees there plus what
	// tenant "u" sees there.
	u, err := s.Capacity(ctx, tenantUPrincipal(), "p")
	if err != nil {
		t.Fatalf("tenant u capacity: %v", err)
	}
	uBuckets := u["by_prefix_length"].(map[string]any)["24"].(map[string]uint64)
	for _, name := range []string{"reserved", "active", "quarantined", "pending"} {
		if operatorBuckets[name] != tenantBuckets[name]+uBuckets[name] {
			t.Fatalf("operator %s = %d, want tenant t's %d plus tenant u's %d", name, operatorBuckets[name], tenantBuckets[name], uBuckets[name])
		}
	}
}

// TestOperatorCapacitySelectsThePoolByIDAlone: every other conjunct of the
// selection -- tenant eligibility, environment, region, account -- is dropped
// for an operator, and the id is not.
func TestOperatorCapacitySelectsThePoolByIDAlone(t *testing.T) {
	s, _ := operatorFixture(t, operatorNow(), true)
	ctx := context.Background()

	// "p-locked" is eligible for a tenant nobody is, in an environment and for
	// an account no identity here holds.
	locked, err := s.Capacity(ctx, operatorPrincipal(), "p-locked")
	if err != nil {
		t.Fatalf("operator capacity of an unreachable pool: %v", err)
	}
	if locked["pool_id"] != "p-locked" {
		t.Fatalf("pool_id = %v, want p-locked", locked["pool_id"])
	}
	if _, err := s.Capacity(ctx, safetyPrincipal(), "p-locked"); err == nil {
		t.Fatal("an ordinary tenant must still be refused the pool it is not eligible for")
	} else {
		safetyAPIError(t, err, 404, "not_found")
	}

	// An id that names no configured pool is still 404 for an operator: the
	// grant drops the eligibility conjuncts, not the identity of the pool.
	_, err = s.Capacity(ctx, operatorPrincipal(), "p-does-not-exist")
	safetyAPIError(t, err, 404, "not_found")
	_, err = s.Capacity(ctx, operatorPrincipal(), "")
	safetyAPIError(t, err, 404, "not_found")
}

// TestOperatorFindingsOnThisFixtureAreTheTwoRowsItSeeds replaces package G3b2's
// placeholder, which asserted that an operator's findings list stayed empty
// until the grouping key existed. Package G3b3 granted the read, and on THIS
// fixture the grant changes nothing else: its two rows are seeded rather than
// written by the fan-out, so they carry no resource identity and are exactly
// the case the domain view may never merge -- each is returned on its own, with
// its own tenant. The grouping itself, and everything the fan-out writes, is
// covered in operator_findings_test.go.
func TestOperatorFindingsOnThisFixtureAreTheTwoRowsItSeeds(t *testing.T) {
	s, _ := operatorFixture(t, operatorNow(), true)
	got, err := s.Findings(context.Background(), operatorPrincipal())
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	if len(got) != 2 || got[0].ID != "find-t" || got[1].ID != "find-u" {
		t.Fatalf("operator findings = %#v, want both seeded rows, unmerged and sorted by id", got)
	}
	tenant, err := s.Findings(context.Background(), safetyPrincipal())
	if err != nil || len(tenant) != 1 || tenant[0].ID != "find-t" {
		t.Fatalf("tenant findings = %#v, err=%v", tenant, err)
	}
}

// -- what is still refused ---------------------------------------------------

// TestOperatorWritesAndOperationStayRefused names, for each attempt, the check
// that refuses it -- none of which this package touched. The refusal of every
// write rests on "an operator has no tenant" and on nothing else: no write
// path consults the role (TestWritePathsNeverConsultTheRole below is what
// keeps that true), so a later change that helpfully gave an operator a tenant
// would break these tests instead of opening a write.
func TestOperatorWritesAndOperationStayRefused(t *testing.T) {
	now := operatorNow()
	ctx := context.Background()
	p := operatorPrincipal()

	t.Run("Reserve: validateRequest's first gate, `p.TenantID == \"\"`", func(t *testing.T) {
		s, ledger := operatorFixture(t, now, true)
		_, _, _, err := s.Reserve(ctx, p, safetyRequest("operator-reserve"), "operator-reserve-key")
		safetyAPIError(t, err, 403, "forbidden")
		st, e := ledger.Snapshot(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if len(st.Allocations) != 4 || len(st.Operations) != 0 || len(st.Events) != 0 {
			t.Fatalf("a refused reservation wrote to the ledger: %d allocations, %d operations, %d events", len(st.Allocations), len(st.Operations), len(st.Events))
		}
	})

	t.Run("Patch: `a.TenantID != p.TenantID`", func(t *testing.T) {
		s, _ := operatorFixture(t, now, true)
		description := "operator probe"
		_, err := s.Patch(ctx, p, "alloc-t-vpc", &description, nil, 1, "operator-patch-key")
		safetyAPIError(t, err, 404, "not_found")
	})

	t.Run("Bind: `a.TenantID != p.TenantID`", func(t *testing.T) {
		s, _ := operatorFixture(t, now, true)
		binding := domain.Binding{Provider: "aws", ResourceType: "vpc", ResourceID: "vpc-operator", AccountID: "123456789012", Region: "eu"}
		_, _, _, err := s.Bind(ctx, p, "alloc-t-vpc", binding, "operator-bind-key")
		safetyAPIError(t, err, 404, "not_found")
	})

	t.Run("Release: `a.TenantID != p.TenantID`", func(t *testing.T) {
		s, ledger := operatorFixture(t, now, true)
		_, _, err := s.Release(ctx, p, "alloc-t-vpc")
		safetyAPIError(t, err, 404, "not_found")
		if got := safetyState(t, ledger, "alloc-t-vpc"); got.State != domain.Reserved || got.ReleaseRequestedAt != nil {
			t.Fatalf("a refused release changed the allocation: %#v", got)
		}
	})

	t.Run("Operation: `o.TenantID != p.TenantID`, deliberately not granted in v1", func(t *testing.T) {
		s, ledger := operatorFixture(t, now, true)
		if err := ledger.Update(ctx, func(st *domain.State) error {
			st.Operations["op-t"] = domain.Operation{ID: "op-t", Type: reserveOperation, Status: operationPending, AllocationID: "alloc-t-hold", TenantID: "t", DomainID: "d", CreatedAt: now, UpdatedAt: now}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_, err := s.Operation(ctx, p, "op-t")
		safetyAPIError(t, err, 404, "not_found")
		if _, err := s.Operation(ctx, safetyPrincipal(), "op-t"); err != nil {
			t.Fatalf("the operation's own tenant must still be able to read it: %v", err)
		}
	})
}

// -- nothing moved for a tenant ----------------------------------------------

// tenantReads marshals every read a tenant principal can make, so two runs can
// be compared as bytes rather than by a hand-written field list that would
// miss a field added later.
func tenantReads(t *testing.T, s *Service) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	record := func(name string, v any, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatalf("marshal %s: %v", name, e)
		}
		out[name] = string(b)
	}
	list, err := s.List(ctx, safetyPrincipal())
	record("list", list, err)
	a, err := s.Get(ctx, safetyPrincipal(), "alloc-t-vpc")
	record("get", a, err)
	pools, err := s.Pools(ctx, safetyPrincipal())
	record("pools", pools, err)
	findings, err := s.Findings(ctx, safetyPrincipal())
	record("findings", findings, err)
	capacity, err := s.Capacity(ctx, safetyPrincipal(), "p")
	record("capacity", capacity, err)
	buckets := capacity["by_prefix_length"].(map[string]any)["24"].(map[string]uint64)
	record("capacity_breakdown", map[string]uint64{
		"reserved": buckets["reserved"], "active": buckets["active"],
		"quarantined": buckets["quarantined"], "pending": buckets["pending"],
		"excluded": buckets["excluded"],
	}, nil)
	record("capacity_allocatable", buckets["allocatable"], nil)
	return out
}

// TestOperatorGrantsMoveNothingForATenant reads the same tenant principal
// across four states: with and without an operator identity in the
// configuration, and with and without the other tenant's rows in the ledger
// (which is what the operator's grants make visible TO THE OPERATOR).
//
// Adding an operator identity must change nothing at all. Adding the other
// tenant's rows must change nothing a tenant is entitled to -- its list, its
// allocation, its pools, its findings and its own lifecycle breakdown -- and
// must change exactly one thing, `allocatable`, which counted every
// non-released allocation in the overlap domain irrespective of tenant long
// before this package existed and would be unsafe if it did not (ADR 0011).
func TestOperatorGrantsMoveNothingForATenant(t *testing.T) {
	now := operatorNow()
	withOperator := []domain.Principal{
		{Subject: "developer", TenantID: "t", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu"}},
		{Subject: "ops:alice", Role: domain.OperatorRole},
	}
	withoutOperator := withOperator[:1]

	reads := map[string]map[string]string{}
	for _, tenantU := range []bool{false, true} {
		for _, operatorConfigured := range []bool{false, true} {
			s, _ := operatorFixture(t, now, tenantU)
			if operatorConfigured {
				s.cfg.Identities = withOperator
			} else {
				s.cfg.Identities = withoutOperator
			}
			name := map[bool]string{false: "alone", true: "beside-tenant-u"}[tenantU] +
				map[bool]string{false: "/no-operator", true: "/operator-configured"}[operatorConfigured]
			reads[name] = tenantReads(t, s)
		}
	}

	// Dimension one: an operator identity existing changes nothing whatsoever.
	for _, suffix := range []string{"alone", "beside-tenant-u"} {
		without, with := reads[suffix+"/no-operator"], reads[suffix+"/operator-configured"]
		for key := range without {
			if without[key] != with[key] {
				t.Fatalf("%s: %s changed when an operator identity was configured:\n without: %s\n    with: %s", suffix, key, without[key], with[key])
			}
		}
	}

	// Dimension two: the other tenant's data existing changes nothing a tenant
	// is entitled to.
	alone, beside := reads["alone/operator-configured"], reads["beside-tenant-u/operator-configured"]
	for _, key := range []string{"list", "get", "pools", "findings", "capacity_breakdown"} {
		if alone[key] != beside[key] {
			t.Fatalf("%s changed for a tenant when another tenant's rows existed:\n without: %s\n    with: %s", key, alone[key], beside[key])
		}
	}
	// ... and changes `allocatable` by exactly the two /24 blocks those rows
	// occupy, as it always did.
	if alone["capacity_allocatable"] != "65533" || beside["capacity_allocatable"] != "65531" {
		t.Fatalf("allocatable = %s alone and %s beside tenant u; want the domain-wide count to drop by the two blocks tenant u occupies", alone["capacity_allocatable"], beside["capacity_allocatable"])
	}
}

// -- structural ---------------------------------------------------------------

// TestWritePathsNeverConsultTheRole is this package's sibling of
// TestCommittedAllocationsComeIntoBeingInThreePlaces. ADR 0011's whole safety
// argument is that an operator is refused by comparisons that do not know the
// role exists: "four of which must never consult the role at all". A role
// check inside a write would replace that argument with a promise, so the
// function bodies below must contain no call to IsOperator and no reference to
// a Role field -- which is a property of the source, not of any test input.
func TestWritePathsNeverConsultTheRole(t *testing.T) {
	// Reserve and its shared body and adoption entry point are the write ADR
	// 0011 names first; validateRequest is the gate that refuses it; Patch,
	// Bind and Release are the other three.
	want := map[string]bool{
		"Reserve": true, "reserve": true, "Adopt": true, "validateRequest": true,
		"Patch": true, "Bind": true, "Release": true,
	}
	seen := map[string]bool{}
	for name, f := range packageFiles(t, ".") {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !want[fn.Name.Name] || fn.Body == nil {
				return true
			}
			seen[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				sel, ok := m.(*ast.SelectorExpr)
				if ok && (sel.Sel.Name == "IsOperator" || sel.Sel.Name == "Role" || sel.Sel.Name == "OperatorRole") {
					t.Errorf("%s in %s refers to %s: the refusal of a write must rest on the tenant comparison alone (ADR 0011)", fn.Name.Name, name, sel.Sel.Name)
				}
				return true
			})
			return false
		})
	}
	for name := range want {
		if !seen[name] {
			t.Fatalf("%s was not found in this package's sources; the guard is checking nothing", name)
		}
	}
}
