package service

// Adoption is the one path that mints a committed allocation for a network the
// platform did not choose, and it has no undo (ADR 0010). These tests sit at
// the service boundary beside safety_contract_test.go: each asserts both on
// what reached the ledger and on what the inventory double was asked to do,
// because an adoption that refuses correctly but writes anyway, or one that
// commits without ever calling Inventory.Adopt, would look identical from the
// return value alone.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

const (
	adoptedCIDR       = "10.7.0.0/24"
	adoptedNetworkID  = "4242"
	adoptedResourceID = "vpc-adopted"
	adoptingOperator  = "ops:alice"
	adoptedBatch      = "batch-2026-09-18"
)

// adoptedNetwork is the reviewed imported prefix: the one object the snapshot
// overlap exemption may name.
func adoptedNetwork() domain.Network {
	return domain.Network{ID: adoptedNetworkID, CIDR: adoptedCIDR, Imported: true, ImportBatch: adoptedBatch}
}

// adoptedResource is the reviewed cloud resource, untagged because the platform
// cannot tag it: an adopted allocation is born RESERVED for exactly that reason.
func adoptedResource() domain.Resource {
	return domain.Resource{
		Type: "vpc", ID: adoptedResourceID, AccountID: "123456789012", Region: "eu",
		CIDR: adoptedCIDR, CIDRs: []string{adoptedCIDR},
	}
}

func adoptPin() Adoption {
	return Adoption{Operator: adoptingOperator, CIDR: adoptedCIDR, NetworkID: adoptedNetworkID, ResourceID: adoptedResourceID}
}

// adoptService differs from safetyService in one value: the pool allows a
// second prefix length, so a consumer POST under an adopted key can differ in
// the immutable field this path has to refuse.
func adoptService(now time.Time, ledger *storage.MemoryLedger, inventory *safetyInventory, observer *safetyObserver) *Service {
	cfg := safetyConfig()
	cfg.Pools[0].AllowedPrefixLengths = []int{24, 25}
	cfg.SubnetPolicy.AllowedPrefixLengths = []int{26}
	s := New(cfg, ledger, inventory, observer)
	s.SetClock(func() time.Time { return now })
	return s
}

// adoptFixture is the state an operator adopts from: the imported prefix in the
// inventory, the matching resource in the cloud, and an empty ledger.
func adoptFixture(now time.Time, networks []domain.Network, resources []domain.Resource) (*Service, *storage.MemoryLedger, *safetyInventory) {
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{networks: networks, adoptID: adoptedNetworkID}
	observer := &safetyObserver{observation: safetyObservation(now, resources...)}
	return adoptService(now, ledger, inventory, observer), ledger, inventory
}

func adoptReady(now time.Time) (*Service, *storage.MemoryLedger, *safetyInventory) {
	return adoptFixture(now, []domain.Network{adoptedNetwork()}, []domain.Resource{adoptedResource()})
}

// assertNothingWritten is the second half of every refusal: a refused adoption
// leaves no allocation to reconcile, no operation fencing the overlap domain,
// no audit event and no idempotency record, and it never asked the inventory to
// convert anything.
func assertNothingWritten(t *testing.T, ledger *storage.MemoryLedger, inventory *safetyInventory) {
	t.Helper()
	st, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Allocations) != 0 || len(st.Operations) != 0 || len(st.Events) != 0 || len(st.Requests) != 0 {
		t.Fatalf("a refused adoption wrote to the ledger: %d allocations, %d operations, %d events, %d requests",
			len(st.Allocations), len(st.Operations), len(st.Events), len(st.Requests))
	}
	if len(inventory.adopts) != 0 {
		t.Fatalf("a refused adoption called Inventory.Adopt: %#v", inventory.adopts)
	}
}

func ledgerState(t *testing.T, ledger *storage.MemoryLedger) *domain.State {
	t.Helper()
	st, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func allocationEvents(st *domain.State, id string) []domain.Event {
	var out []domain.Event
	for _, e := range st.Events {
		if e.AllocationID == id {
			out = append(out, e)
		}
	}
	return out
}

func TestAdoptCommitsThroughTheInventoryWithTheOperatorAsActor(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)

	a, op, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 || a == nil || op == nil {
		t.Fatalf("adopt: allocation=%#v operation=%#v status=%d err=%v", a, op, status, err)
	}
	if !a.Committed || a.State != domain.Reserved || a.CIDR != adoptedCIDR || a.InventoryID != adoptedNetworkID || a.Binding != nil {
		t.Fatalf("adopted allocation: %#v", *a)
	}
	if a.PrefixLength != 24 || a.PoolID != "p" || a.TenantID != "t" {
		t.Fatalf("adopted allocation did not record the reviewed request: %#v", *a)
	}
	if op.Type != adoptOperation || op.Status != operationSucceeded {
		t.Fatalf("adoption operation: %#v", *op)
	}
	if len(inventory.adopts) != 1 || inventory.adopts[0].CIDR != adoptedCIDR || inventory.adopts[0].ID != a.ID {
		t.Fatalf("Inventory.Adopt was not asked for the reviewed allocation: %#v", inventory.adopts)
	}
	if inventory.ensures != 0 {
		t.Fatalf("adoption called Ensure %d times; Ensure is permanently refused for an adopted CIDR", inventory.ensures)
	}
	events := allocationEvents(ledgerState(t, ledger), a.ID)
	if len(events) != 2 || events[0].Action != "ADOPT_PLANNED" || events[1].Action != "ADOPT_COMMITTED" {
		t.Fatalf("audit trail: %#v", events)
	}
	for _, e := range events {
		if e.Actor != adoptingOperator {
			t.Fatalf("event %s names actor %q, want the operator", e.Action, e.Actor)
		}
	}
	// domain.Event carries no structured details, so the evidence an operator
	// reviewed has to survive in the reason string.
	for _, want := range []string{adoptedNetworkID, adoptedResourceID, adoptedBatch, "g"} {
		if !strings.Contains(events[0].Reason, want) {
			t.Fatalf("ADOPT_PLANNED reason %q omits %q", events[0].Reason, want)
		}
	}
	if got, err := s.Get(context.Background(), safetyPrincipal(), a.ID); err != nil || got.CIDR != adoptedCIDR {
		t.Fatalf("adopted allocation is not readable: %#v %v", got, err)
	}
}

func TestAdoptRefusesASecondUnmanagedPrefix(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	second := domain.Network{ID: "4243", CIDR: "10.7.0.128/25", Imported: true, ImportBatch: adoptedBatch}
	s, ledger, inventory := adoptFixture(now, []domain.Network{adoptedNetwork(), second}, []domain.Resource{adoptedResource()})

	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	assertNothingWritten(t, ledger, inventory)
}

func TestAdoptRefusesASecondObservedResource(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	second := domain.Resource{
		Type: "vpc", ID: "vpc-neighbour", AccountID: "123456789012", Region: "eu",
		CIDR: "10.7.0.128/25", CIDRs: []string{"10.7.0.128/25"},
	}
	s, ledger, inventory := adoptFixture(now, []domain.Network{adoptedNetwork()}, []domain.Resource{adoptedResource(), second})

	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	assertNothingWritten(t, ledger, inventory)
}

// A secondary association is occupancy evidence and never the network an
// allocation issues, so the reviewed resource must hold the adopted CIDR as its
// primary one even when the operator named the right resource id.
func TestAdoptRefusesAResourceWhoseSecondaryCIDRMatches(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	secondary := domain.Resource{
		Type: "vpc", ID: adoptedResourceID, AccountID: "123456789012", Region: "eu",
		CIDR: "10.9.0.0/24", CIDRs: []string{"10.9.0.0/24", adoptedCIDR},
	}
	s, ledger, inventory := adoptFixture(now, []domain.Network{adoptedNetwork()}, []domain.Resource{secondary})

	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	assertNothingWritten(t, ledger, inventory)
}

// Adoption is not a way to claim space nobody can see: the reviewed resource
// has to be in the observation the service already trusts.
func TestAdoptRefusesWhenTheReviewedResourceIsNotObserved(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptFixture(now, []domain.Network{adoptedNetwork()}, nil)

	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	assertNothingWritten(t, ledger, inventory)
}

// The ledger overlap scan gets no exemption at all: a review can speak for an
// imported prefix and an untagged resource, never for an allocation the
// platform itself issued.
func TestAdoptRefusesWhenAnAllocationAlreadyHoldsTheCIDR(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for _, committed := range []bool{true, false} {
		name := "committed_allocation"
		if !committed {
			name = "uncommitted_hold"
		}
		t.Run(name, func(t *testing.T) {
			s, ledger, inventory := adoptReady(now)
			existing := safetyAllocation(now, "alloc-existing", "vpc", adoptedCIDR, "", "")
			if !committed {
				// An uncommitted hold blocks whatever overlap domain it belongs
				// to, exactly as it does for a chosen candidate: until it
				// resolves, nobody knows whose space this is.
				existing.Committed, existing.DomainID = false, "other-domain"
			}
			putSafetyAllocation(t, ledger, existing)

			_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
			safetyAPIError(t, err, 409, "adoption_refused")
			if len(inventory.adopts) != 0 {
				t.Fatalf("a refused adoption called Inventory.Adopt: %#v", inventory.adopts)
			}
			st := ledgerState(t, ledger)
			if len(st.Allocations) != 1 || len(st.Operations) != 0 || len(st.Events) != 0 || len(st.Requests) != 0 {
				t.Fatalf("a refused adoption wrote to the ledger: %d allocations, %d operations, %d events, %d requests",
					len(st.Allocations), len(st.Operations), len(st.Events), len(st.Requests))
			}
		})
	}
}

func TestAdoptRefusesAnythingButTheReviewedImportedNetwork(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	owned := adoptedNetwork()
	owned.Owned = true
	claimed := adoptedNetwork()
	claimed.AllocationID = "alloc-someone-else"
	untagged := adoptedNetwork()
	untagged.Imported = false
	cases := map[string]struct {
		networks []domain.Network
		pin      Adoption
	}{
		"not_imported":                {[]domain.Network{untagged}, adoptPin()},
		"carries_an_ownership_field":  {[]domain.Network{owned}, adoptPin()},
		"carries_an_allocation_id":    {[]domain.Network{claimed}, adoptPin()},
		"another_inventory_id":        {[]domain.Network{adoptedNetwork()}, Adoption{Operator: adoptingOperator, CIDR: adoptedCIDR, NetworkID: "4243", ResourceID: adoptedResourceID}},
		"reviewed_id_at_another_cidr": {[]domain.Network{{ID: adoptedNetworkID, CIDR: "10.7.0.128/25", Imported: true, ImportBatch: adoptedBatch}}, adoptPin()},
		"not_in_the_snapshot":         {nil, adoptPin()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, ledger, inventory := adoptFixture(now, tc.networks, []domain.Resource{adoptedResource()})
			_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), tc.pin)
			safetyAPIError(t, err, 409, "adoption_refused")
			assertNothingWritten(t, ledger, inventory)
		})
	}
}

// The second exemption names one resource and describes it completely, so
// every way of being a different object is a refusal rather than a near miss.
func TestAdoptRefusesAResourceThatIsNotTheReviewedOne(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	elsewhere := adoptedResource()
	elsewhere.ID = "vpc-someone-else"
	inside := adoptedResource()
	inside.CIDR, inside.CIDRs = "10.7.0.0/25", []string{"10.7.0.0/25"}
	claimed := adoptedResource()
	claimed.Tags = map[string]string{"platform-ipam:allocation-id": "alloc-someone-else"}
	wrongAccount := adoptedResource()
	wrongAccount.AccountID = "210987654321"
	wrongType := adoptedResource()
	wrongType.Type = "subnet"
	wrongRegion := adoptedResource()
	wrongRegion.Region = "us"
	for name, resource := range map[string]domain.Resource{
		"another_resource_id":     elsewhere,
		"primary_cidr_is_smaller": inside,
		"already_claimed":         claimed,
		"another_account":         wrongAccount,
		"another_type":            wrongType,
		"another_region":          wrongRegion,
	} {
		t.Run(name, func(t *testing.T) {
			s, ledger, inventory := adoptFixture(now, []domain.Network{adoptedNetwork()}, []domain.Resource{resource})
			_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
			safetyAPIError(t, err, 409, "adoption_refused")
			assertNothingWritten(t, ledger, inventory)
		})
	}
}

// The recorded prefix length must equal the mask of the adopted CIDR, or the
// owning team's own request under that key is refused forever (ADR 0010).
// Excluded space is excluded for an operator too: reviewing a network does not
// review away the pool's own policy.
func TestAdoptRefusesExcludedAddressSpace(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)
	s.cfg.Pools[0].ExcludedCIDRs = []string{"10.7.0.0/23"}
	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	assertNothingWritten(t, ledger, inventory)
}

func TestAdoptRefusesACIDRThatIsNotTheRequestedShape(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		cidr   string
		status int
		code   string
	}{
		"prefix_length_disagrees": {"10.7.0.0/25", 422, "invalid_request"},
		"not_canonical":           {"10.7.0.1/24", 422, "invalid_request"},
		"outside_the_pool":        {"192.168.4.0/24", 422, "policy_violation"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, ledger, inventory := adoptReady(now)
			pin := adoptPin()
			pin.CIDR = tc.cidr
			_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), pin)
			safetyAPIError(t, err, tc.status, tc.code)
			assertNothingWritten(t, ledger, inventory)
		})
	}
}

func TestAdoptRefusesAnIncompleteSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)
	inventory.incomplete = true

	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 503, "dependency_unavailable")
	assertNothingWritten(t, ledger, inventory)
}

func TestAdoptRejectsUntrustworthyCompleteObservations(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for name, observation := range map[string]domain.Observation{
		"missing_started_at": {DomainID: "d", Generation: "g", Complete: true, FinishedAt: now, Resources: []domain.Resource{adoptedResource()}},
		"wrong_generation":   {DomainID: "d", Generation: "old-generation", Complete: true, StartedAt: now.Add(-time.Minute), FinishedAt: now, Resources: []domain.Resource{adoptedResource()}},
		"incomplete":         {DomainID: "d", Generation: "g", Complete: false, StartedAt: now.Add(-time.Minute), FinishedAt: now, Resources: []domain.Resource{adoptedResource()}},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := storage.NewMemoryLedger()
			inventory := &safetyInventory{networks: []domain.Network{adoptedNetwork()}, adoptID: adoptedNetworkID}
			s := adoptService(now, ledger, inventory, &safetyObserver{observation: observation})
			_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
			safetyAPIError(t, err, 503, "dependency_unavailable")
			assertNothingWritten(t, ledger, inventory)
		})
	}
}

func TestAdoptRefusesAPrincipalWithoutATenant(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)
	p := safetyPrincipal()
	p.TenantID = ""

	_, _, _, err := s.Adopt(context.Background(), p, safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 403, "forbidden")
	assertNothingWritten(t, ledger, inventory)
}

// The service cannot verify the operator, so the one thing it can insist on is
// that the audit trail names somebody.
func TestAdoptRefusesWithoutAnOperatorSubject(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)

	anonymous := adoptPin()
	anonymous.Operator = ""
	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), anonymous)
	safetyAPIError(t, err, 422, "invalid_request")
	assertNothingWritten(t, ledger, inventory)
}

// Inventory.Adopt locates the prefix by CIDR and VRF rather than by id, so the
// object it converted is the reviewed one only if the id agrees. The write has
// already happened when it does not: refusing to commit leaves the operation
// pending for a person, which is the only correct outcome for an operation
// with no undo.
func TestAdoptDoesNotCommitWhenTheInventoryConvertedAnotherObject(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)
	inventory.adoptID = "9999"

	a, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_conflict")
	if a != nil {
		t.Fatalf("a refused commit returned an allocation: %#v", *a)
	}
	st := ledgerState(t, ledger)
	if len(st.Allocations) != 1 {
		t.Fatalf("want the durable hold to remain, got %d allocations", len(st.Allocations))
	}
	for id, held := range st.Allocations {
		if held.Committed {
			t.Fatalf("allocation %s committed on a foreign inventory object: %#v", id, held)
		}
		if events := allocationEvents(st, id); len(events) != 1 || events[0].Action != "ADOPT_PLANNED" {
			t.Fatalf("want only the planned event, got %#v", events)
		}
		if _, err := s.Get(context.Background(), safetyPrincipal(), id); err == nil {
			t.Fatal("an uncommitted adoption is visible to GET")
		}
	}
	if len(st.Operations) != 1 {
		t.Fatalf("want one pending operation, got %d", len(st.Operations))
	}
	for _, op := range st.Operations {
		if op.Type != adoptOperation || op.Status != operationPending {
			t.Fatalf("operation must stay pending for a person: %#v", op)
		}
	}
}

func TestAdoptReplayConvergesWithoutASecondAllocation(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)

	first, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 {
		t.Fatalf("first adoption: status=%d err=%v", status, err)
	}
	// A re-run whose mutable fields changed is still a replay: the reviewed
	// record is the whole request, and Adopt drops description and labels.
	rerun := safetyRequest("orders")
	rerun.Description = "re-run after the operator edited their notes"
	rerun.Labels = map[string]string{"reviewed-by": "ops"}
	again, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), rerun, adoptPin())
	if err != nil || status != 200 || again == nil || again.ID != first.ID || again.CIDR != first.CIDR {
		t.Fatalf("replay: allocation=%#v status=%d err=%v", again, status, err)
	}
	st := ledgerState(t, ledger)
	if len(st.Allocations) != 1 || len(inventory.adopts) != 1 {
		t.Fatalf("replay minted more than one adoption: %d allocations, %d inventory calls", len(st.Allocations), len(inventory.adopts))
	}
	if events := allocationEvents(st, first.ID); len(events) != 2 {
		t.Fatalf("replay added audit events: %#v", events)
	}
}

// An allocation key that already names an allocation replays it, and the
// request hash cannot tell the two apart: a consumer request carries no CIDR.
// So the replay has to be compared with the pin, or an operator adopting
// 10.7.0.0/24 under a key the allocator already served elsewhere is told the
// adoption succeeded while nothing was adopted.
func TestAdoptRefusesAKeyThatAlreadyHoldsAnotherCIDR(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)

	reserved, _, status, err := s.Reserve(context.Background(), safetyPrincipal(), safetyRequest("orders"), "consumer-key-1")
	if err != nil || status != 201 || reserved.CIDR == adoptedCIDR {
		t.Fatalf("consumer reservation: allocation=%#v status=%d err=%v", reserved, status, err)
	}
	a, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_conflict")
	if a != nil {
		t.Fatalf("a refused adoption returned an allocation: %#v", *a)
	}
	if len(inventory.adopts) != 0 {
		t.Fatalf("the inventory was asked to adopt: %#v", inventory.adopts)
	}
	st := ledgerState(t, ledger)
	if len(st.Allocations) != 1 {
		t.Fatalf("want only the consumer's allocation, got %d", len(st.Allocations))
	}
	// The refusal comes before the body, so it leaves no adoption record
	// pointing at the consumer's allocation either.
	for id, request := range st.Requests {
		if request.Method == adoptKind.method {
			t.Fatalf("a refused adoption left idempotency record %s: %#v", id, request)
		}
	}
	for _, event := range allocationEvents(st, reserved.ID) {
		if strings.HasPrefix(event.Action, "ADOPT") {
			t.Fatalf("the consumer's allocation gained an adoption event: %#v", event)
		}
	}
}

// The same hole from the other side: a second adoption under an adopted key
// that names a different network must not replay the first as its own success.
func TestAdoptReplayRefusesADifferentReviewedNetwork(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, _, inventory := adoptReady(now)
	if _, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin()); err != nil || status != 201 {
		t.Fatalf("first adoption: status=%d err=%v", status, err)
	}
	for name, change := range map[string]func(*Adoption){
		"another CIDR":         func(p *Adoption) { p.CIDR = "10.7.1.0/24" },
		"another inventory id": func(p *Adoption) { p.NetworkID = "4243" },
	} {
		pin := adoptPin()
		change(&pin)
		a, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), pin)
		safetyAPIError(t, err, 409, "adoption_conflict")
		if a != nil {
			t.Fatalf("%s: a refused adoption returned an allocation: %#v", name, *a)
		}
	}
	if len(inventory.adopts) != 1 {
		t.Fatalf("want one inventory adoption, got %d", len(inventory.adopts))
	}
}

// The point of adopting under the team's own allocation key: their first POST
// is a replay, not a conflict, and it never creates anything in the inventory.
func TestAdoptedAllocationReplaysTheOwningTeamsFirstPOST(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, _, inventory := adoptReady(now)
	adopted, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 {
		t.Fatalf("adopt: status=%d err=%v", status, err)
	}

	// The team's own request: their description and labels, and no account id,
	// which validateRequest resolves to the one the operator supplied.
	team := safetyRequest("orders")
	team.Description = "orders vpc, owned by team orders"
	team.Labels = map[string]string{"service": "orders"}
	got, _, status, err := s.Reserve(context.Background(), safetyPrincipal(), team, "team-http-key")
	if err != nil || status != 200 || got == nil {
		t.Fatalf("the owning team's first POST: allocation=%#v status=%d err=%v", got, status, err)
	}
	if got.ID != adopted.ID || got.CIDR != adoptedCIDR {
		t.Fatalf("replay returned another allocation: %#v", *got)
	}
	if inventory.ensures != 0 {
		t.Fatalf("the replay called Ensure %d times", inventory.ensures)
	}
}

func TestAdoptedAllocationConflictsWhenThePrefixLengthDiffers(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, _, inventory := adoptReady(now)
	if _, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin()); err != nil || status != 201 {
		t.Fatalf("adopt: status=%d err=%v", status, err)
	}

	smaller := safetyRequest("orders")
	smaller.PrefixLength = 25
	_, _, _, err := s.Reserve(context.Background(), safetyPrincipal(), smaller, "team-http-key")
	safetyAPIError(t, err, 409, "allocation_key_conflict")
	if inventory.ensures != 0 {
		t.Fatalf("a conflicting request reached the inventory %d times", inventory.ensures)
	}
}

func TestAdoptedThenReleasedKeyIsRetired(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, _, _ := adoptReady(now)
	a, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 {
		t.Fatalf("adopt: status=%d err=%v", status, err)
	}
	if _, status, err := s.Release(context.Background(), safetyPrincipal(), a.ID); err != nil || status != 202 {
		t.Fatalf("release: status=%d err=%v", status, err)
	}

	_, _, _, err = s.Reserve(context.Background(), safetyPrincipal(), safetyRequest("orders"), "team-http-key")
	safetyAPIError(t, err, 409, "allocation_key_retired")
	// A re-run of the adoption itself is refused the same way: there is no undo.
	_, _, _, err = s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "allocation_key_retired")
}

// Adoption keys its idempotency record by the allocation key, so the record it
// writes must live under a method and path of its own: a team whose
// Idempotency-Key happens to be their allocation key must still be answered.
func TestAdoptionIdempotencyDoesNotOccupyTheConsumerRecord(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, _ := adoptReady(now)
	adopted, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 {
		t.Fatalf("adopt: status=%d err=%v", status, err)
	}

	team := safetyRequest("orders")
	team.Description = "orders vpc"
	got, _, status, err := s.Reserve(context.Background(), safetyPrincipal(), team, "orders")
	if err != nil || status != 200 || got == nil || got.ID != adopted.ID {
		t.Fatalf("POST whose Idempotency-Key is the allocation key: allocation=%#v status=%d err=%v", got, status, err)
	}
	st := ledgerState(t, ledger)
	if len(st.Requests) != 1 {
		t.Fatalf("want only the adoption's own record, got %#v", st.Requests)
	}
	for _, record := range st.Requests {
		if record.Method != "ADOPT" || record.Path != "ADOPT /v1/allocations" || record.Key != "orders" || record.AllocationID != adopted.ID {
			t.Fatalf("adoption idempotency record: %#v", record)
		}
	}
	// The consumer's own key is still free, so a retry of their POST with the
	// mutable fields changed replays rather than colliding with a record whose
	// body was never theirs.
	retry := safetyRequest("orders")
	retry.Description = "orders vpc, second attempt"
	again, _, status, err := s.Reserve(context.Background(), safetyPrincipal(), retry, "orders")
	if err != nil || status != 200 || again == nil || again.ID != adopted.ID {
		t.Fatalf("retry under the consumer's key: allocation=%#v status=%d err=%v", again, status, err)
	}
}

// Adoption adds two overlap exemptions and no more, so a subnet's own parent
// VPC is exempt only on the evidence a reservation uses: a verified parent
// binding. The consequence is a real constraint on the operator sequence --
// parents before subnets is not enough, the parent's resource must already be
// tagged and observed -- and it is here rather than in a third exemption.
func TestAdoptSubnetNeedsItsParentsObservedBinding(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	const subnetCIDR, subnetResourceID, zone = "10.7.0.64/26", "subnet-adopted", "euc1-az1"
	networks := []domain.Network{
		{ID: "4200", CIDR: adoptedCIDR, AllocationID: "alloc-parent"},
		{ID: "4243", CIDR: subnetCIDR, Imported: true, ImportBatch: adoptedBatch},
	}
	parentResource := domain.Resource{
		Type: "vpc", ID: "vpc-parent", AccountID: "123456789012", Region: "eu",
		CIDR: adoptedCIDR, CIDRs: []string{adoptedCIDR},
		Tags: map[string]string{"platform-ipam:allocation-id": "alloc-parent", "platform-ipam:allocation-key": "alloc-parent-key"},
	}
	subnetResource := domain.Resource{
		Type: "subnet", ID: subnetResourceID, AccountID: "123456789012", Region: "eu",
		CIDR: subnetCIDR, CIDRs: []string{subnetCIDR}, ParentID: "vpc-parent", ZoneID: zone,
	}
	request := domain.Request{
		AllocationKey: "orders-subnet", Scope: "subnet", Environment: "prod", Region: "eu",
		AccountID: "123456789012", PrefixLength: 26, ParentAllocationID: "alloc-parent", AvailabilityZoneID: zone,
	}
	pin := Adoption{Operator: adoptingOperator, CIDR: subnetCIDR, NetworkID: "4243", ResourceID: subnetResourceID}

	cases := []struct {
		name                         string
		bound                        bool
		resourceParent, resourceZone string
	}{
		{"parent_binding_not_observed", false, "vpc-parent", zone},
		{"parent_binding_observed", true, "vpc-parent", zone},
		{"subnet_of_another_vpc", true, "vpc-elsewhere", zone},
		{"subnet_in_another_zone", true, "vpc-parent", "euc1-az2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observed := subnetResource
			observed.ParentID, observed.ZoneID = tc.resourceParent, tc.resourceZone
			s, ledger, inventory := adoptFixture(now, networks, []domain.Resource{parentResource, observed})
			parent := safetyAllocation(now, "alloc-parent", "vpc", adoptedCIDR, "", "")
			if tc.bound {
				parent.State = domain.Active
				parent.Binding = &domain.Binding{Provider: "aws", ResourceType: "vpc", ResourceID: "vpc-parent", AccountID: "123456789012", Region: "eu"}
			}
			putSafetyAllocation(t, ledger, parent)
			inventory.adoptID = "4243"

			a, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), request, pin)
			if tc.name != "parent_binding_observed" {
				safetyAPIError(t, err, 409, "adoption_refused")
				if len(inventory.adopts) != 0 {
					t.Fatalf("a refused subnet adoption called Inventory.Adopt: %#v", inventory.adopts)
				}
				if st := ledgerState(t, ledger); len(st.Allocations) != 1 || len(st.Operations) != 0 {
					t.Fatalf("a refused subnet adoption changed the ledger: %d allocations, %d operations", len(st.Allocations), len(st.Operations))
				}
				return
			}
			if err != nil || status != 201 || a == nil || a.CIDR != subnetCIDR || a.InventoryID != "4243" || !a.Committed {
				t.Fatalf("subnet adoption: allocation=%#v status=%d err=%v", a, status, err)
			}
			if a.ParentAllocationID != "alloc-parent" || a.AvailabilityZoneID != zone {
				t.Fatalf("subnet adoption lost its parent or zone: %#v", *a)
			}
		})
	}
}

// --- structural guarantees -------------------------------------------------

// packageFiles parses the non-test sources of this package or one beside it.
func packageFiles(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]*ast.File{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatalf("no sources found in %s", dir)
	}
	return out
}

// A committed allocation comes into being in exactly three places: one
// construction in the shared reservation body and two assignments of
// Committed, one there and one in the worker's recovery. Everything the
// ledger, the API and the safety tests assume is a property of that one path,
// so a fourth place is a defect even when every other test still passes
// (ADR 0010).
func TestCommittedAllocationsComeIntoBeingInThreePlaces(t *testing.T) {
	constructions, assignments, literals := 0, 0, 0
	for name, f := range packageFiles(t, ".") {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if sel, ok := x.Type.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "domain" && sel.Sel.Name == "Allocation" {
						constructions++
						t.Logf("allocation constructed in %s", name)
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := x.Key.(*ast.Ident); ok && key.Name == "Committed" {
					if value, ok := x.Value.(*ast.Ident); ok && value.Name == "true" {
						literals++
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Committed" {
						continue
					}
					assignments++
					value, ok := x.Rhs[i].(*ast.Ident)
					if !ok || value.Name != "true" {
						t.Fatalf("%s assigns something other than true to Committed", name)
					}
					t.Logf("Committed assigned in %s", name)
				}
			}
			return true
		})
	}
	if constructions != 1 || assignments != 2 || literals != 0 {
		t.Fatalf("a committed allocation must come into being in exactly three places: got %d constructions, %d assignments of Committed and %d composite literals setting it true",
			constructions, assignments, literals)
	}
}

// The pin is a pointer parameter on an unexported function with no transport
// binding, so the consumer path cannot pin by accident. That is a compile-time
// fact (ADR 0010); this test is what keeps it one.
func TestConsumerReserveCannotPin(t *testing.T) {
	files := packageFiles(t, ".")
	var body *ast.BlockStmt
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "Reserve" {
				return true
			}
			body = fn.Body
			return false
		})
	}
	if body == nil || len(body.List) != 1 {
		t.Fatalf("Reserve must delegate to the shared body in one statement, got %#v", body)
	}
	ret, ok := body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		t.Fatalf("Reserve must return the shared body's result, got %#v", body.List[0])
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		t.Fatalf("Reserve must call the shared body, got %#v", ret.Results[0])
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "reserve" {
		t.Fatalf("Reserve must call the unexported shared body, got %#v", call.Fun)
	}
	last, ok := call.Args[len(call.Args)-1].(*ast.Ident)
	if !ok || last.Name != "nil" {
		t.Fatalf("Reserve must pass a nil pin, got %#v", call.Args[len(call.Args)-1])
	}

	// Nothing may construct a pin outside a test, and no request or HTTP body
	// may reach one: the transport package does not know the type exists.
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "Adoption" {
				t.Fatalf("%s constructs a pin outside a test", name)
			}
			return true
		})
	}
	for name, f := range packageFiles(t, "../transport") {
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "Adoption" {
				t.Fatalf("transport/%s names the pin type", name)
			}
			return true
		})
	}
}

// The inventory's Adopt has exactly two callers, and the number is the point.
// The first is the shared reservation body, which reaches it only after the
// gates above. The second is the worker's recovery of a pending ADOPT
// operation, which exists because the body's own call can be interrupted: the
// hold it leaves behind fences the whole overlap domain, and nothing else can
// finish it (ADR 0010). Both re-verify the reviewed record before they call and
// compare the returned inventory id with it afterwards. A third caller would be
// a route to ownership that neither reviewed nor recovered anything, so this
// count is raised only together with the argument for raising it, and never
// loosened into a range.
func TestInventoryAdoptHasTwoCallers(t *testing.T) {
	calls := 0
	for name, f := range packageFiles(t, ".") {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Adopt" {
				return true
			}
			calls++
			if receiver, ok := sel.X.(*ast.SelectorExpr); !ok || receiver.Sel.Name != "inventory" {
				t.Fatalf("%s calls Adopt on something other than the inventory port", name)
			}
			return true
		})
	}
	if calls != 2 {
		t.Fatalf("want exactly two Inventory.Adopt calls in the service (the shared body and the worker's recovery), got %d", calls)
	}
}

// The durable record carries what the adoption is about to overwrite on the
// reviewed network, read from the snapshot before the adapter is called, so an
// abandon can put it back (ADR 0012).
func TestAdoptRecordsWhatItIsAboutToOverwrite(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	reviewed := adoptedNetwork()
	reviewed.Status, reviewed.AWSAccountID, reviewed.AWSRegion = "deprecated", "123456789012", "eu"
	s, ledger, _ := adoptFixture(now, []domain.Network{reviewed}, []domain.Resource{adoptedResource()})
	if _, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin()); err != nil || status != 201 {
		t.Fatalf("adoption: status=%d err=%v", status, err)
	}
	st := ledgerState(t, ledger)
	for _, op := range st.Operations {
		if op.Adoption == nil {
			t.Fatalf("the adoption's operation carries no record: %#v", op)
		}
		prior := op.Adoption.Prior()
		if prior.Status != "deprecated" || prior.AccountID != "123456789012" || prior.Region != "eu" {
			t.Fatalf("recorded prior inventory: %#v", prior)
		}
	}
	if len(st.Operations) != 1 {
		t.Fatalf("want one operation, got %d", len(st.Operations))
	}
}
