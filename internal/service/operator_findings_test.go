package service

// Package G3b3 (ADR 0011 stage two) gives an operator the findings view the
// other reads already have, and it is the one read that cannot be the tenant
// filter widened. The occupancy fan-out in worker.go writes one row per
// eligible tenant of every pool in the domain, so one unmanaged resource in a
// domain with two eligible tenants is two stored rows whose public projections
// differ only in an opaque hash: a cross-tenant list would show one problem
// twice, and any count an operator derived from it would be a function of
// tenancy configuration rather than of the estate.
//
// The collapse needs a key the store did not carry, because the fan-out's own
// key is a hash and is not reversible. domain.Finding therefore gains
// ResourceType and ResourceID, set by the fan-out and by nothing else, and the
// operator view groups on domain, code, account, region and that identity.
// These tests assert the fan-out half (what is stored, and that a row written
// before the fields existed repairs itself), the grouping half (what one
// unmanaged resource looks like to an operator, and every case that must NOT
// collapse), and that a tenant's own view did not move.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

// -- fixtures ----------------------------------------------------------------

// unmanagedResource is one cloud resource carrying no platform claim tag, in
// the account and region package G3b2's configuration covers.
func unmanagedResource(id, cidr string) domain.Resource {
	return domain.Resource{
		AccountID: "123456789012", Region: "eu", Type: "vpc", ID: id,
		CIDR: cidr, CIDRs: []string{cidr},
	}
}

// occupancyService builds a service over package G3b2's two-eligible-tenant
// configuration whose observer reports exactly these resources. Nothing is
// seeded: what the ledger holds afterwards is what the fan-out wrote.
//
// It keeps only the first pool. G3b2's second pool, "p-locked", sits in the
// same domain and lists an eligible tenant of its own, so the fan-out -- which
// iterates the domain's POOLS and then each pool's tenants -- would reach three
// tenants for every resource. That is a real case and it has its own test
// below; carrying it in every fixture here would only obscure ADR 0011's
// example, which is two eligible tenants over one resource.
func occupancyService(t *testing.T, now time.Time, resources ...domain.Resource) (*Service, *storage.MemoryLedger, *safetyObserver) {
	t.Helper()
	cfg := operatorConfig()
	cfg.Pools = cfg.Pools[:1]
	ledger := storage.NewMemoryLedger()
	observer := &safetyObserver{observation: safetyObservation(now, resources...)}
	s := New(cfg, ledger, &safetyInventory{}, observer)
	s.SetClock(func() time.Time { return now })
	return s, ledger, observer
}

// tickAt runs one worker pass at a chosen instant, re-stamping the observation
// so it stays fresh. Two passes at two instants are how a real fan-out produces
// rows whose windows differ: a tenant that becomes eligible later has a later
// first_observed_at for a resource that was already there.
func tickAt(t *testing.T, s *Service, observer *safetyObserver, when time.Time) {
	t.Helper()
	s.SetClock(func() time.Time { return when })
	observer.observation = safetyObservation(when, observer.observation.Resources...)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick at %s: %v", when.Format(time.RFC3339), err)
	}
}

// fanOutKey is the key worker.go stores an occupancy finding under. Tests that
// seed a row rather than observing one build it through this helper, so a
// seeded fixture cannot quietly stop looking like the real thing.
func fanOutKey(domainID, tenant string, r domain.Resource) string {
	return findingID("unmanaged_occupancy", domainID+":"+tenant+":"+r.AccountID+":"+r.Region+":"+r.Type+":"+r.ID)
}

// occupancyRow is a stored row shaped exactly as the fan-out writes one.
func occupancyRow(domainID, tenant string, r domain.Resource, status string, first, last time.Time) domain.Finding {
	return domain.Finding{
		ID: fanOutKey(domainID, tenant, r), TenantID: tenant, DomainID: domainID,
		Code: "unmanaged_occupancy", Severity: "WARNING", Status: status,
		AccountID: r.AccountID, Region: r.Region,
		ResourceType: r.Type, ResourceID: r.ID,
		FirstObservedAt: first, LastObservedAt: last,
	}
}

func putFindings(t *testing.T, ledger *storage.MemoryLedger, rows ...domain.Finding) {
	t.Helper()
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		for _, f := range rows {
			st.Findings[f.ID] = f
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func storedFindings(t *testing.T, ledger *storage.MemoryLedger) []domain.Finding {
	t.Helper()
	st, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := []domain.Finding{}
	for _, f := range st.Findings {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func operatorFindingsOf(t *testing.T, s *Service) []domain.Finding {
	t.Helper()
	got, err := s.Findings(context.Background(), operatorPrincipal())
	if err != nil {
		t.Fatalf("operator findings: %v", err)
	}
	return got
}

func findingIDs(fs []domain.Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func marshalString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// -- the fan-out stores the identity -----------------------------------------

// TestTwoEligibleTenantsStoreTwoRowsAndShowAnOperatorOne is ADR 0011's own
// example, end to end through the worker: one unmanaged VPC in a domain whose
// pool lists two eligible tenants. The store holds two rows, because the
// honest domain-level record is deliberately deferred and storage stays linear
// in the number of tenants; the operator sees one.
//
// The two passes are what make the window assertion mean something. Tenant "u"
// becomes eligible between them, so the row for "t" was first seen at the
// earlier instant and both rows were last seen at the later one -- exactly the
// case where taking either member's timestamps verbatim would be wrong.
func TestTwoEligibleTenantsStoreTwoRowsAndShowAnOperatorOne(t *testing.T) {
	first := operatorNow()
	second := first.Add(10 * time.Minute)
	resource := unmanagedResource("vpc-unmanaged", "10.50.0.0/24")
	s, ledger, observer := occupancyService(t, first, resource)

	s.cfg.Pools[0].EligibleTenants = []string{"t"}
	tickAt(t, s, observer, first)
	s.cfg.Pools[0].EligibleTenants = []string{"t", "u"}
	tickAt(t, s, observer, second)

	stored := storedFindings(t, ledger)
	if len(stored) != 2 {
		t.Fatalf("the fan-out stored %d rows, want one per eligible tenant: %#v", len(stored), stored)
	}
	tenants := map[string]bool{}
	for _, f := range stored {
		tenants[f.TenantID] = true
		if f.Code != "unmanaged_occupancy" || f.AllocationID != "" {
			t.Fatalf("unexpected stored finding: %#v", f)
		}
		if f.ResourceType != "vpc" || f.ResourceID != "vpc-unmanaged" {
			t.Fatalf("a stored occupancy row carries no resource identity: %#v", f)
		}
		if f.ID != fanOutKey("d", f.TenantID, resource) {
			t.Fatalf("stored under an unexpected key: %#v", f)
		}
	}
	if !tenants["t"] || !tenants["u"] {
		t.Fatalf("the fan-out did not reach both eligible tenants: %v", tenants)
	}

	got := operatorFindingsOf(t, s)
	if len(got) != 1 {
		t.Fatalf("an operator saw %d rows for one unmanaged resource: %#v", len(got), got)
	}
	row := got[0]

	// The representative is the lexicographically smallest member id, so the id
	// a cursor was cut on is still in the list on the next call.
	smallest := stored[0].ID
	if stored[1].ID < smallest {
		smallest = stored[1].ID
	}
	if row.ID != smallest {
		t.Fatalf("representative id = %s, want the smallest member id %s", row.ID, smallest)
	}
	if !row.FirstObservedAt.Equal(first) {
		t.Fatalf("first_observed_at = %s, want the earliest member's %s", row.FirstObservedAt, first)
	}
	if !row.LastObservedAt.Equal(second) {
		t.Fatalf("last_observed_at = %s, want the latest member's %s", row.LastObservedAt, second)
	}
	if row.Status != "OPEN" || row.Severity != "WARNING" {
		t.Fatalf("status/severity = %s/%s, want OPEN/WARNING", row.Status, row.Severity)
	}
	if row.ResourceType != "vpc" || row.ResourceID != "vpc-unmanaged" || row.DomainID != "d" {
		t.Fatalf("the grouped row lost the identity it was grouped on: %#v", row)
	}
	// A domain-level fact has no recipient tenant. Both members name one, and
	// carrying either here would present an accident of tenancy configuration as
	// a property of the resource observed.
	if row.TenantID != "" {
		t.Fatalf("the grouped row named tenant %q; a domain-level fact has no recipient", row.TenantID)
	}

	// Map iteration is randomized per range, so a representative chosen by
	// iteration order would show up here rather than in production.
	want := marshalString(t, got)
	for i := 0; i < 20; i++ {
		if again := marshalString(t, operatorFindingsOf(t, s)); again != want {
			t.Fatalf("call %d returned a different row:\n want: %s\n  got: %s", i+2, want, again)
		}
	}
}

// TestTwoResourcesInOnePassStayTwoRows is the mistake the resource identity
// exists to prevent. Both rows share code, account, region and both timestamps
// -- everything the stored row exposed before this package -- and differ only
// in the resource they are about.
func TestTwoResourcesInOnePassStayTwoRows(t *testing.T) {
	now := operatorNow()
	left := unmanagedResource("vpc-left", "10.50.0.0/24")
	right := unmanagedResource("vpc-right", "10.51.0.0/24")
	s, ledger, observer := occupancyService(t, now, left, right)
	tickAt(t, s, observer, now)

	if stored := storedFindings(t, ledger); len(stored) != 4 {
		t.Fatalf("stored %d rows, want two resources x two eligible tenants: %#v", len(stored), stored)
	}
	got := operatorFindingsOf(t, s)
	if len(got) != 2 {
		t.Fatalf("an operator saw %d rows for two distinct resources: %#v", len(got), got)
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f.ResourceID] = true
		if f.AccountID != "123456789012" || f.Region != "eu" || f.Code != "unmanaged_occupancy" {
			t.Fatalf("unexpected grouped row: %#v", f)
		}
	}
	if !seen["vpc-left"] || !seen["vpc-right"] {
		t.Fatalf("two resources collapsed into one row: %v", seen)
	}
}

// TestTheSameResourceIDInTwoDomainsStaysTwoRows keeps the routing domain in the
// key. Resource ids are unique per cloud account in practice and not by
// construction here, and two overlap domains are two separate estates: merging
// across them would report one problem where there are two.
//
// Seeded rather than observed: this fixture's observer answers for one domain,
// and what is under test is the grouping key. Every row is built through
// fanOutKey, so it is stored exactly where the fan-out would store it.
func TestTheSameResourceIDInTwoDomainsStaysTwoRows(t *testing.T) {
	now := operatorNow()
	s, ledger, _ := occupancyService(t, now)
	resource := unmanagedResource("vpc-same-id", "10.50.0.0/24")
	putFindings(t, ledger,
		occupancyRow("d", "t", resource, "OPEN", now, now),
		occupancyRow("d-other", "t", resource, "OPEN", now, now),
	)

	got := operatorFindingsOf(t, s)
	if len(got) != 2 {
		t.Fatalf("one resource id in two domains collapsed to %d row(s): %#v", len(got), got)
	}
	domains := map[string]bool{}
	for _, f := range got {
		domains[f.DomainID] = true
	}
	if !domains["d"] || !domains["d-other"] {
		t.Fatalf("the grouped rows lost a domain: %v", domains)
	}
}

// TestAGroupIsOpenIfAnyMemberIs: OPEN is the only direction a gate can be
// wrong in safely. `client findings --fail-if-open` turns "at least one
// returned finding is OPEN" into a non-zero exit, so a group that reported
// RESOLVED while one of its members was open would be a gate that passes on a
// live problem.
func TestAGroupIsOpenIfAnyMemberIs(t *testing.T) {
	now := operatorNow()
	resource := unmanagedResource("vpc-mixed", "10.52.0.0/24")

	for _, tc := range []struct {
		name          string
		first, second string
		want          string
	}{
		{"one resolved and one open", "RESOLVED", "OPEN", "OPEN"},
		{"one open and one resolved", "OPEN", "RESOLVED", "OPEN"},
		{"all resolved", "RESOLVED", "RESOLVED", "RESOLVED"},
		{"all open", "OPEN", "OPEN", "OPEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ledger, _ := occupancyService(t, now)
			putFindings(t, ledger,
				occupancyRow("d", "t", resource, tc.first, now, now),
				occupancyRow("d", "u", resource, tc.second, now, now),
			)
			got := operatorFindingsOf(t, s)
			if len(got) != 1 {
				t.Fatalf("want one grouped row, got %#v", got)
			}
			if got[0].Status != tc.want {
				t.Fatalf("status = %s, want %s", got[0].Status, tc.want)
			}
		})
	}
}

// TestAGroupCarriesTheHighestSeverityAnyMemberHas. The fan-out writes WARNING
// for every member today, so this rule is not reachable through it; it is
// asserted anyway, because the alternative -- taking the representative's
// severity verbatim -- would make a CRITICAL invisible the moment any code ever
// raises the severity of one recipient's row, and the representative is chosen
// by an id hash that has nothing to do with severity.
func TestAGroupCarriesTheHighestSeverityAnyMemberHas(t *testing.T) {
	now := operatorNow()
	resource := unmanagedResource("vpc-severity", "10.53.0.0/24")
	s, ledger, _ := occupancyService(t, now)
	warning := occupancyRow("d", "t", resource, "OPEN", now, now)
	critical := occupancyRow("d", "u", resource, "OPEN", now, now)
	critical.Severity = "CRITICAL"
	putFindings(t, ledger, warning, critical)

	got := operatorFindingsOf(t, s)
	if len(got) != 1 || got[0].Severity != "CRITICAL" {
		t.Fatalf("grouped severity = %#v, want one row at CRITICAL", got)
	}
}

// -- what is never grouped ----------------------------------------------------

// TestAllocationScopedFindingsReachAnOperatorUnmerged. A finding that names an
// allocation is a fact about that allocation, which exactly one tenant holds,
// so there is nothing to collapse and both tenants' rows are visible.
//
// adoption_stuck is the case worth naming: it belongs to an UNCOMMITTED hold
// (ADR 0010 -- a pending ADOPT fences its whole overlap domain until a person
// resolves it, and there is no undo), and an operator is exactly who has to see
// it. The allocation behind it is invisible to Get and List, which keep
// `a.Committed`; the finding is not, because a finding is not an allocation.
func TestAllocationScopedFindingsReachAnOperatorUnmerged(t *testing.T) {
	now := operatorNow()
	ctx := context.Background()
	s, ledger := operatorFixture(t, now, true)
	s.cfg.Lifecycle.ReservationAgeAlertHours = 1

	// Age tenant "t"'s committed reservation past the alert threshold, so the
	// worker raises reservation_aged for it on the next pass.
	if err := ledger.Update(ctx, func(st *domain.State) error {
		aged := st.Allocations["alloc-t-vpc"]
		aged.CreatedAt = now.Add(-48 * time.Hour)
		st.Allocations[aged.ID] = aged
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The uncommitted hold's stuck adoption, raised by the path that raises it.
	if err := s.flagStuckAdoption(ctx, safetyState(t, ledger, "alloc-t-hold")); err != nil {
		t.Fatalf("flag stuck adoption: %v", err)
	}
	if err := s.reconcileAllocations(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	want := map[string]struct{ code, tenant string }{
		findingID("reservation_aged", "alloc-t-vpc"): {"reservation_aged", "t"},
		findingID("adoption_stuck", "alloc-t-hold"):  {"adoption_stuck", "t"},
		// Tenant "u"'s ACTIVE allocation carries no binding in this fixture, so
		// the reconciler reports its resource as missing -- a second tenant's
		// allocation-scoped finding, which is the point here.
		findingID("resource_missing", "alloc-u-vpc"): {"resource_missing", "u"},
	}
	got := operatorFindingsOf(t, s)
	byID := map[string]domain.Finding{}
	for _, f := range got {
		byID[f.ID] = f
	}
	for id, expected := range want {
		f, ok := byID[id]
		if !ok {
			t.Fatalf("an operator did not see the %s finding of tenant %s: %v", expected.code, expected.tenant, findingIDs(got))
		}
		if f.Code != expected.code {
			t.Fatalf("%s: code = %s, want %s", id, f.Code, expected.code)
		}
		// Unmerged means untouched: an allocation-scoped row keeps its tenant,
		// because it is not a domain-level fact and naming its holder is not
		// misleading.
		if f.TenantID != expected.tenant {
			t.Fatalf("%s: tenant = %q, want %q", id, f.TenantID, expected.tenant)
		}
		if f.ResourceType != "" || f.ResourceID != "" {
			t.Fatalf("%s: an allocation-scoped finding carries a resource identity: %#v", id, f)
		}
	}
	// The hold itself stays invisible; only the finding about it is not.
	if _, err := s.Get(ctx, operatorPrincipal(), "alloc-t-hold"); err == nil {
		t.Fatal("the uncommitted hold behind adoption_stuck became readable")
	}
}

// TestTwoAllocationsAreNeverMergedEvenSharingAResource pins the half of
// domainLevelFinding that nothing can reach today: no writer gives an
// allocation-scoped finding a resource identity, so dropping `AllocationID ==
// ""` from that predicate currently changes no behaviour at all -- a mutation
// test found exactly that. The guard is kept, and pinned here, because the day
// a finding names both is the day the operator view would silently merge two
// allocations' problems into one row and drop an allocation id: two allocations
// can claim one resource (that is what multiple_resource_claims reports), and
// everything else about their findings would agree.
//
// The rows below are therefore deliberately a shape this project does not write
// yet. If a later package does start writing it, this test is what says the
// view already handles it.
func TestTwoAllocationsAreNeverMergedEvenSharingAResource(t *testing.T) {
	now := operatorNow()
	resource := unmanagedResource("vpc-contested", "10.61.0.0/24")
	s, ledger, _ := occupancyService(t, now)
	shared := func(id, allocation string) domain.Finding {
		f := occupancyRow("d", "t", resource, "OPEN", now, now)
		f.ID, f.AllocationID = id, allocation
		f.Code, f.Severity = "multiple_resource_claims", "CRITICAL"
		return f
	}
	putFindings(t, ledger,
		shared("finding_claim_one", "alloc-one"),
		shared("finding_claim_two", "alloc-two"),
	)

	got := operatorFindingsOf(t, s)
	if !sameStrings(findingIDs(got), []string{"finding_claim_one", "finding_claim_two"}) {
		t.Fatalf("two allocations' findings were merged: %v", findingIDs(got))
	}
	for _, f := range got {
		if f.AllocationID == "" || f.TenantID == "" {
			t.Fatalf("an allocation-scoped row lost its allocation or tenant: %#v", f)
		}
	}
}

// TestRowsWithoutAResourceIdentityAreNeverMerged. Two rows written before this
// package -- or any future finding that names neither an allocation nor a
// resource -- agree on every field the store did expose. Collapsing them on
// those fields is exactly the merge of two genuinely different resources that
// ADR 0011 forbids in the one view that is supposed to be the truth, so each is
// returned on its own.
func TestRowsWithoutAResourceIdentityAreNeverMerged(t *testing.T) {
	now := operatorNow()
	s, ledger, _ := occupancyService(t, now)
	legacy := []domain.Finding{
		{ID: "finding_legacy_one", TenantID: "t", DomainID: "d", Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN", AccountID: "123456789012", Region: "eu", FirstObservedAt: now, LastObservedAt: now},
		{ID: "finding_legacy_two", TenantID: "u", DomainID: "d", Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN", AccountID: "123456789012", Region: "eu", FirstObservedAt: now, LastObservedAt: now},
	}
	putFindings(t, ledger, legacy...)

	got := operatorFindingsOf(t, s)
	if !sameStrings(findingIDs(got), []string{"finding_legacy_one", "finding_legacy_two"}) {
		t.Fatalf("legacy rows were merged or reordered: %v", findingIDs(got))
	}
	// Untouched, tenant included: nothing is known about them that would justify
	// calling either one a domain-level fact.
	for i, f := range got {
		if f.TenantID != legacy[i].TenantID {
			t.Fatalf("a legacy row's tenant changed: %#v", f)
		}
	}
}

// TestALegacyRowGainsItsIdentityWithinOneWorkerPass is the claim ADR 0011 makes
// instead of a migration -- "findings are resolved and re-opened on every
// reconciliation pass, so the new fields fill themselves in within one worker
// interval" -- asserted rather than assumed. The fan-out writes the identity on
// every pass, not only where it creates the row, which is what makes it true.
func TestALegacyRowGainsItsIdentityWithinOneWorkerPass(t *testing.T) {
	long := operatorNow().Add(-72 * time.Hour)
	now := operatorNow()
	resource := unmanagedResource("vpc-legacy", "10.54.0.0/24")
	s, ledger, observer := occupancyService(t, now, resource)

	// A row exactly as a pre-G3b3 binary left it: the right key, the right
	// window, and no resource identity.
	legacy := occupancyRow("d", "t", resource, "OPEN", long, long)
	legacy.ResourceType, legacy.ResourceID = "", ""
	putFindings(t, ledger, legacy)

	tickAt(t, s, observer, now)

	st, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	repaired, ok := st.Findings[legacy.ID]
	if !ok {
		t.Fatalf("the legacy row disappeared: %v", findingIDs(storedFindings(t, ledger)))
	}
	if repaired.ResourceType != "vpc" || repaired.ResourceID != "vpc-legacy" {
		t.Fatalf("the legacy row did not gain its identity after one pass: %#v", repaired)
	}
	// Repairing the identity must not restate when the problem was first seen.
	if !repaired.FirstObservedAt.Equal(long) {
		t.Fatalf("first_observed_at moved to %s; the finding is the same one, first seen at %s", repaired.FirstObservedAt, long)
	}
	if !repaired.LastObservedAt.Equal(now) || repaired.Status != "OPEN" {
		t.Fatalf("the pass did not re-open the row: %#v", repaired)
	}
	// And from that pass on it groups with the other tenant's row instead of
	// standing alone. Whether it is the group's representative depends on how
	// its id hash happens to sort, so the assertion is on the window: only the
	// repaired row was first seen at `long`, so a group carrying that instant
	// is a group this row is inside.
	got := operatorFindingsOf(t, s)
	if len(got) != 1 {
		t.Fatalf("the repaired row did not join the group: %#v", got)
	}
	if !got[0].FirstObservedAt.Equal(long) || got[0].ResourceID != "vpc-legacy" {
		t.Fatalf("the group does not contain the repaired row: %#v", got[0])
	}
}

// TestASecondPoolInTheDomainStillShowsOneRow is the fan-out's other dimension.
// It iterates the domain's pools and then each pool's eligible tenants, so a
// domain with two pools stores a row for every tenant of either -- three here,
// with package G3b2's "p-locked" and its tenant "nobody" back in the
// configuration. The grouping key names the domain and not the pool, so all
// three collapse into the one row the estate actually contains.
func TestASecondPoolInTheDomainStillShowsOneRow(t *testing.T) {
	now := operatorNow()
	resource := unmanagedResource("vpc-two-pools", "10.60.0.0/24")
	ledger := storage.NewMemoryLedger()
	observer := &safetyObserver{observation: safetyObservation(now, resource)}
	s := New(operatorConfig(), ledger, &safetyInventory{}, observer)
	s.SetClock(func() time.Time { return now })
	tickAt(t, s, observer, now)

	stored := storedFindings(t, ledger)
	tenants := map[string]bool{}
	for _, f := range stored {
		tenants[f.TenantID] = true
	}
	if len(stored) != 3 || !tenants["t"] || !tenants["u"] || !tenants["nobody"] {
		t.Fatalf("stored %d rows for %v, want one per eligible tenant of either pool", len(stored), tenants)
	}
	got := operatorFindingsOf(t, s)
	if len(got) != 1 || got[0].ResourceID != resource.ID || got[0].TenantID != "" {
		t.Fatalf("operator view = %#v, want one tenant-less row for the one resource", got)
	}
}

// TestAnAdoptedResourceRaisesNoOccupancyFindingForAnybody. An adopted network is
// owned in the ledger and untagged in the cloud, because the platform may not
// tag somebody else's resource (ADR 0010), so package F3 exempts exactly the
// resource an adoption's durable record names. The operator view must not be a
// way around that exemption: a finding nobody may see is a finding nobody sees,
// operator included, because there is no row to group.
func TestAnAdoptedResourceRaisesNoOccupancyFindingForAnybody(t *testing.T) {
	now := operatorNow()
	adopted := unmanagedResource("vpc-adopted", "10.55.0.0/24")
	other := unmanagedResource("vpc-not-adopted", "10.56.0.0/24")
	s, ledger, observer := occupancyService(t, now, adopted, other)

	a := operatorAllocation(now, "alloc-adopted", "t", "vpc", adopted.CIDR, domain.Reserved, true)
	a.AccountID, a.Region = adopted.AccountID, adopted.Region
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		st.Allocations[a.ID] = a
		st.Operations["op-adopted"] = domain.Operation{
			ID: "op-adopted", Type: adoptOperation, Status: operationSucceeded,
			AllocationID: a.ID, TenantID: a.TenantID, DomainID: a.DomainID,
			Adoption:  &domain.AdoptionRecord{Operator: "ops:alice", NetworkID: "4242", ResourceID: adopted.ID},
			CreatedAt: now, UpdatedAt: now,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tickAt(t, s, observer, now)

	for _, f := range storedFindings(t, ledger) {
		if f.ResourceID == adopted.ID {
			t.Fatalf("the adopted resource was reported as unmanaged occupancy: %#v", f)
		}
	}
	got := operatorFindingsOf(t, s)
	for _, f := range got {
		if f.ResourceID == adopted.ID {
			t.Fatalf("the adopted resource reached the operator's view: %#v", f)
		}
	}
	// The resource nobody adopted is still reported, so the assertion above is
	// about the exemption and not about a fan-out that stopped working.
	if len(got) != 1 || got[0].ResourceID != other.ID {
		t.Fatalf("operator view = %#v, want exactly the un-adopted resource", got)
	}
}

// -- nothing moved for a tenant ----------------------------------------------

// tenantFindingsGolden is the tenant's view of tenantGoldenRows below, captured
// from the code as it stood BEFORE package G3b3. The rows are shaped as a
// pre-G3b3 ledger holds them -- no resource identity anywhere -- so for that
// ledger the answer must be identical to the byte: same rows, same order, same
// fields, and the two new fields absent because they are omitempty and empty.
const tenantFindingsGolden = `[{"id":"find_alloc_t","tenant_id":"t","domain_id":"d","allocation_id":"alloc-t-vpc","code":"reservation_aged","severity":"WARNING","status":"OPEN","account_id":"123456789012","region":"eu","first_observed_at":"2026-09-19T10:00:00Z","last_observed_at":"2026-09-19T10:00:00Z"},{"id":"find_legacy_t_one","tenant_id":"t","domain_id":"d","code":"unmanaged_occupancy","severity":"WARNING","status":"OPEN","account_id":"123456789012","region":"eu","first_observed_at":"2026-09-19T09:00:00Z","last_observed_at":"2026-09-19T10:00:00Z"},{"id":"find_legacy_t_two","tenant_id":"t","domain_id":"d","code":"unmanaged_occupancy","severity":"WARNING","status":"OPEN","account_id":"123456789012","region":"eu","first_observed_at":"2026-09-19T08:00:00Z","last_observed_at":"2026-09-19T10:00:00Z"}]`

func tenantGoldenRows(now time.Time) []domain.Finding {
	return []domain.Finding{
		{ID: "find_legacy_t_one", TenantID: "t", DomainID: "d", Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN", AccountID: "123456789012", Region: "eu", FirstObservedAt: now.Add(-time.Hour), LastObservedAt: now},
		{ID: "find_legacy_t_two", TenantID: "t", DomainID: "d", Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN", AccountID: "123456789012", Region: "eu", FirstObservedAt: now.Add(-2 * time.Hour), LastObservedAt: now},
		{ID: "find_alloc_t", TenantID: "t", DomainID: "d", AllocationID: "alloc-t-vpc", Code: "reservation_aged", Severity: "WARNING", Status: "OPEN", AccountID: "123456789012", Region: "eu", FirstObservedAt: now, LastObservedAt: now},
		{ID: "find_legacy_u", TenantID: "u", DomainID: "d", Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN", AccountID: "123456789012", Region: "eu", FirstObservedAt: now, LastObservedAt: now},
	}
}

func TestATenantsFindingsViewIsByteIdenticalOnAPreChangeLedger(t *testing.T) {
	now := operatorNow()
	withOperator := []domain.Principal{
		{Subject: "developer", TenantID: "t", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu"}},
		{Subject: "ops:alice", Role: domain.OperatorRole},
	}
	for _, operatorConfigured := range []bool{false, true} {
		s, ledger, _ := occupancyService(t, now)
		s.cfg.Identities = withOperator[:1]
		if operatorConfigured {
			s.cfg.Identities = withOperator
		}
		putFindings(t, ledger, tenantGoldenRows(now)...)

		got, err := s.Findings(context.Background(), safetyPrincipal())
		if err != nil {
			t.Fatalf("tenant findings: %v", err)
		}
		if marshalled := marshalString(t, got); marshalled != tenantFindingsGolden {
			t.Fatalf("a tenant's findings view changed (operator configured = %v):\n want: %s\n  got: %s", operatorConfigured, tenantFindingsGolden, marshalled)
		}
	}
}

// TestATenantsRefreshedViewGainsOnlyTheTwoFields states the other half
// honestly. Once a worker pass has run, the rows a tenant reads DO carry the
// resource identity, because it is stored on the row rather than computed for
// the operator -- what did not change is which rows a tenant gets, in which
// order, and every other field on them. The API response is unaffected either
// way: the projection in internal/transport/http.go does not emit the fields,
// and whether an operator's does is package G3b4's decision.
func TestATenantsRefreshedViewGainsOnlyTheTwoFields(t *testing.T) {
	now := operatorNow()
	resource := unmanagedResource("vpc-refresh", "10.57.0.0/24")
	s, _, observer := occupancyService(t, now, resource)
	tickAt(t, s, observer, now)

	got, err := s.Findings(context.Background(), safetyPrincipal())
	if err != nil {
		t.Fatalf("tenant findings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("tenant findings = %#v, want exactly its own copy of the one resource", got)
	}
	if got[0].TenantID != "t" || got[0].ResourceID != resource.ID {
		t.Fatalf("unexpected tenant row: %#v", got[0])
	}
	stripped := got[0]
	stripped.ResourceType, stripped.ResourceID = "", ""
	strippedWant := occupancyRow("d", "t", resource, "OPEN", now, now)
	strippedWant.ResourceType, strippedWant.ResourceID = "", ""
	if marshalString(t, stripped) != marshalString(t, strippedWant) {
		t.Fatalf("a tenant's row changed beyond the two new fields:\n want: %s\n  got: %s", marshalString(t, strippedWant), marshalString(t, stripped))
	}
	if want := marshalString(t, occupancyRow("d", "t", resource, "OPEN", now, now)); marshalString(t, got[0]) != want {
		t.Fatalf("the tenant's row is not what the fan-out wrote:\n want: %s\n  got: %s", want, marshalString(t, got[0]))
	}
	// Tenant "u" is eligible for the same pool, so it holds its own copy -- the
	// fan-out is unchanged, and this is the storage cost ADR 0011 accepts.
	u, err := s.Findings(context.Background(), tenantUPrincipal())
	if err != nil {
		t.Fatalf("tenant u findings: %v", err)
	}
	if len(u) != 1 || u[0].TenantID != "u" || u[0].ID == got[0].ID {
		t.Fatalf("tenant u's own copy = %#v", u)
	}
}

// -- structural ---------------------------------------------------------------

// TestOnlyTheOccupancyFanOutSetsTheResourceIdentity is this package's sibling of
// F2's "three places" test and G3b2's "write paths never consult the role". The
// fields are trustworthy as a grouping key only because exactly one writer sets
// them: a second writer, with a different notion of what the identity means,
// would make the operator view merge or split rows on evidence nobody checked.
//
// Two properties over the whole of cmd/ and internal/, because either could be
// broken from any call site that imports internal/domain: no assignment to
// .ResourceType or .ResourceID outside the fan-out's file, and no
// domain.Finding composite literal that sets either field.
func TestOnlyTheOccupancyFanOutSetsTheResourceIdentity(t *testing.T) {
	assignments := map[string]int{}
	literals := []string{}
	for _, root := range []string{"../../cmd", "../../internal"} {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			rel := filepath.ToSlash(path)
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && isResourceIdentityField(sel.Sel.Name) {
							assignments[rel]++
						}
					}
				case *ast.CompositeLit:
					if !isFindingLiteral(x.Type) {
						return true
					}
					for _, elt := range x.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); ok && isResourceIdentityField(key.Name) {
							literals = append(literals, rel+": "+key.Name)
						}
					}
				}
				return true
			})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(literals) != 0 {
		t.Fatalf("a domain.Finding literal sets the resource identity outside the fan-out: %v", literals)
	}
	// Two names assigned in one statement is two LHS selectors.
	want := map[string]int{"../../internal/service/worker.go": 2}
	if len(assignments) != len(want) {
		t.Fatalf("resource identity assigned in %v, want only the occupancy fan-out in %v", assignments, want)
	}
	for file, count := range want {
		if assignments[file] != count {
			t.Fatalf("%s assigns the resource identity %d time(s), want %d", file, assignments[file], count)
		}
	}
}

func isResourceIdentityField(name string) bool {
	return name == "ResourceType" || name == "ResourceID"
}

func isFindingLiteral(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "Finding"
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && pkg.Name == "domain" && t.Sel.Name == "Finding"
	}
	return false
}

// TestOnlyOccupancyFindingsCarryAResourceIdentity is the same claim read off
// behaviour instead of source: one pass that raises an allocation-scoped
// finding and an occupancy finding at once, and then every stored row is
// checked both ways round.
func TestOnlyOccupancyFindingsCarryAResourceIdentity(t *testing.T) {
	now := operatorNow()
	resource := unmanagedResource("vpc-structural", "10.58.0.0/24")
	s, ledger, observer := occupancyService(t, now, resource)
	s.cfg.Lifecycle.ReservationAgeAlertHours = 1
	aged := operatorAllocation(now, "alloc-aged", "t", "vpc", "10.59.0.0/24", domain.Reserved, true)
	aged.CreatedAt = now.Add(-48 * time.Hour)
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		st.Allocations[aged.ID] = aged
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tickAt(t, s, observer, now)

	codes := map[string]bool{}
	for _, f := range storedFindings(t, ledger) {
		codes[f.Code] = true
		identified := f.ResourceType != "" || f.ResourceID != ""
		if identified && (f.Code != "unmanaged_occupancy" || f.AllocationID != "") {
			t.Fatalf("a finding outside the occupancy fan-out carries a resource identity: %#v", f)
		}
		if !identified && f.Code == "unmanaged_occupancy" {
			t.Fatalf("an occupancy finding carries no resource identity: %#v", f)
		}
	}
	if !codes["unmanaged_occupancy"] || !codes["reservation_aged"] {
		t.Fatalf("the pass did not raise both kinds of finding: %v", codes)
	}
}

// Account and region are part of what a finding is about: the same resource id
// reported under two accounts, or two regions, is two facts. AWS ids make the
// collision unlikely; the key must not depend on that.
func TestTheSameResourceIDInTwoAccountsOrRegionsStaysSeparate(t *testing.T) {
	now := operatorNow()
	s, ledger, _ := occupancyService(t, now)
	here := unmanagedResource("vpc-same-id", "10.50.0.0/24")
	otherAccount := here
	otherAccount.AccountID = "210987654321"
	otherRegion := here
	otherRegion.Region = "us"
	putFindings(t, ledger,
		occupancyRow("d", "t", here, "OPEN", now, now),
		occupancyRow("d", "t", otherAccount, "OPEN", now, now),
		occupancyRow("d", "t", otherRegion, "OPEN", now, now),
	)
	if got := operatorFindingsOf(t, s); len(got) != 3 {
		t.Fatalf("want three rows for one id under two accounts and two regions, got %d: %#v", len(got), got)
	}
}

// The transport cuts pages on the id, but the order is this function's promise
// too: grouped rows come out of a map, and nothing about a caller of the
// service may depend on which way Go happened to walk it.
func TestAnOperatorsFindingsComeBackInIDOrderEveryTime(t *testing.T) {
	now := operatorNow()
	s, ledger, _ := occupancyService(t, now)
	var rows []domain.Finding
	for i := 0; i < 12; i++ {
		r := unmanagedResource(fmt.Sprintf("vpc-order-%02d", i), fmt.Sprintf("10.60.%d.0/24", i))
		rows = append(rows, occupancyRow("d", "t", r, "OPEN", now, now), occupancyRow("d", "u", r, "OPEN", now, now))
	}
	putFindings(t, ledger, rows...)
	for call := 0; call < 20; call++ {
		ids := findingIDs(operatorFindingsOf(t, s))
		if len(ids) != 12 {
			t.Fatalf("want twelve grouped rows, got %d", len(ids))
		}
		if !sort.StringsAreSorted(ids) {
			t.Fatalf("call %d: rows are not in id order: %v", call, ids)
		}
	}
}
