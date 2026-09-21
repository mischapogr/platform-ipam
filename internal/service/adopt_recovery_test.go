package service

// A crashed adoption is the one failure ADR 0010 could not leave to the
// existing recovery: the reserve path filters it out, and the hold it leaves
// behind answers every reservation in its overlap domain with 503 domain_busy
// until something finishes it. These tests assert on both halves of every
// outcome -- what reached the ledger, and what the inventory double was asked
// to do -- because an adoption that commits without ever asking the adapter and
// one that asks it and then refuses to commit are indistinguishable from the
// state afterwards alone.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/netbox"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

// adoptionUnderRecovery is a planned adoption whose adapter call was lost.
type adoptionUnderRecovery struct {
	service    *Service
	ledger     *storage.MemoryLedger
	inventory  *safetyInventory
	observer   *safetyObserver
	allocation domain.Allocation
}

// crashedAdoption leaves exactly what a crash leaves: the durable hold, the
// pending ADOPT operation fencing the domain, the planned audit event, and no
// commit. It gets there through the real planning path rather than by writing
// rows, so the reviewed record the worker recovers from is the one an
// operator's own process would have persisted.
func crashedAdoption(t *testing.T, now time.Time) adoptionUnderRecovery {
	t.Helper()
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{
		networks: []domain.Network{adoptedNetwork()}, adoptID: adoptedNetworkID,
		adoptErr: errors.New("the adapter never answered"),
	}
	observer := &safetyObserver{observation: safetyObservation(now, adoptedResource())}
	s := adoptService(now, ledger, inventory, observer)

	a, op, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 202 || a == nil || a.Committed || op == nil || op.Status != operationPending || op.Type != adoptOperation {
		t.Fatalf("planning an adoption whose adapter call is then lost: allocation=%#v operation=%#v status=%d err=%v", a, op, status, err)
	}
	if op.Adoption == nil || *op.Adoption != reviewedRecord() {
		t.Fatalf("the pending operation does not carry the reviewed record: %#v", op.Adoption)
	}
	// The planning call was not the recovery's. A test that counts adapter
	// calls counts the ones the worker made.
	inventory.adoptErr, inventory.adopts = nil, nil
	return adoptionUnderRecovery{s, ledger, inventory, observer, *a}
}

func reviewedRecord() domain.AdoptionRecord {
	return domain.AdoptionRecord{
		Operator: adoptingOperator, NetworkID: adoptedNetworkID,
		ResourceID: adoptedResourceID, ImportBatch: adoptedBatch,
	}
}

func findingsWithCode(t *testing.T, ledger *storage.MemoryLedger, code string) []domain.Finding {
	t.Helper()
	var out []domain.Finding
	for _, f := range ledgerState(t, ledger).Findings {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

// assertStillPending is the shape every refused recovery has to leave: nothing
// committed, nothing visible, the operation still fencing its domain for a
// person, and no commit event claiming otherwise.
func assertStillPending(t *testing.T, ledger *storage.MemoryLedger, id string) {
	t.Helper()
	st := ledgerState(t, ledger)
	a, ok := st.Allocations[id]
	if !ok || a.Committed || a.InventoryID != "" {
		t.Fatalf("a refused recovery committed the adoption: %#v", a)
	}
	pending := 0
	for _, o := range st.Operations {
		if o.AllocationID == id && o.Type == adoptOperation && o.Status == operationPending {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("want the adoption to stay pending, got %d pending ADOPT operations", pending)
	}
	for _, e := range allocationEvents(st, id) {
		if e.Action == adoptKind.committed {
			t.Fatalf("a refused recovery wrote a commit event: %#v", e)
		}
	}
}

func TestWorkerRecoversAPendingAdoptionInsteadOfWedgingTheDomain(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := crashedAdoption(t, now)
	ctx := context.Background()

	// The whole reason this path exists: until the adoption resolves, the
	// pending operation fences every consumer reservation in its overlap
	// domain.
	_, _, _, err := c.service.Reserve(ctx, safetyPrincipal(), safetyRequest("neighbour"), "neighbour-key")
	safetyAPIError(t, err, 503, "domain_busy")

	if err := c.service.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	committed, err := c.service.Get(ctx, safetyPrincipal(), c.allocation.ID)
	if err != nil || !committed.Committed || committed.State != domain.Reserved || committed.CIDR != adoptedCIDR || committed.InventoryID != adoptedNetworkID || committed.Binding != nil {
		t.Fatalf("recovered adoption: %#v %v", committed, err)
	}
	if len(c.inventory.adopts) != 1 || c.inventory.adopts[0].ID != c.allocation.ID || c.inventory.adopts[0].CIDR != adoptedCIDR {
		t.Fatalf("the worker did not ask the inventory to adopt the reviewed allocation: %#v", c.inventory.adopts)
	}
	if c.inventory.ensures != 0 {
		t.Fatalf("recovery called Ensure %d times; Ensure is permanently refused for an adopted CIDR", c.inventory.ensures)
	}
	st := ledgerState(t, c.ledger)
	events := allocationEvents(st, c.allocation.ID)
	if len(events) != 2 || events[0].Action != "ADOPT_PLANNED" || events[1].Action != "ADOPT_COMMITTED" {
		t.Fatalf("audit trail: %#v", events)
	}
	for _, e := range events {
		if e.Actor != adoptingOperator {
			t.Fatalf("event %s names actor %q, want the operator even though the worker wrote it", e.Action, e.Actor)
		}
	}
	for _, want := range []string{adoptedNetworkID, adoptedResourceID, adoptedBatch, "g"} {
		if !strings.Contains(events[1].Reason, want) {
			t.Fatalf("ADOPT_COMMITTED reason %q omits %q", events[1].Reason, want)
		}
	}
	for _, o := range st.Operations {
		if o.AllocationID == c.allocation.ID && (o.Type != adoptOperation || o.Status != operationSucceeded) {
			t.Fatalf("recovered operation: %#v", o)
		}
	}
	if len(st.Findings) != 0 {
		t.Fatalf("a recovered adoption raised findings: %#v", st.Findings)
	}

	// And the domain is open again, which is the availability half of the fix.
	a, _, status, err := c.service.Reserve(ctx, safetyPrincipal(), safetyRequest("neighbour"), "neighbour-key")
	if err != nil || status != 201 || a == nil {
		t.Fatalf("the overlap domain is still fenced after recovery: allocation=%#v status=%d err=%v", a, status, err)
	}
}

// seedPendingHold writes the durable half of an interrupted operation directly,
// which is the only way to have two of them fencing one domain at once: the
// barrier that makes this package necessary also stops the API from producing
// the situation.
func seedPendingHold(t *testing.T, ledger *storage.MemoryLedger, a domain.Allocation, operationType string, record *domain.AdoptionRecord) {
	t.Helper()
	a.Committed, a.InventoryID, a.InventorySync = false, "", "PENDING"
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		st.Allocations[a.ID] = a
		st.Operations["op-"+a.ID] = domain.Operation{
			ID: "op-" + a.ID, Type: operationType, Status: operationPending, AllocationID: a.ID,
			TenantID: a.TenantID, DomainID: a.DomainID, Adoption: record,
			CreatedAt: a.CreatedAt, UpdatedAt: a.CreatedAt,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The two recoveries are siblings, not alternatives. Ensure is permanently
// refused for a CIDR that already holds a prefix and Adopt refuses to create
// one, so a path that picked up the other's work would not merely fail: it
// would fail forever, with the overlap domain fenced behind it.
func TestRecoveryNeverCrossesTheTwoOperationTypes(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{networks: []domain.Network{adoptedNetwork()}, adoptID: adoptedNetworkID}
	observer := &safetyObserver{observation: safetyObservation(now, adoptedResource())}
	s := adoptService(now, ledger, inventory, observer)

	reserved := safetyAllocation(now, "alloc-reserved", "vpc", "10.8.0.0/24", "", "")
	seedPendingHold(t, ledger, reserved, reserveOperation, nil)
	adopted := safetyAllocation(now, "alloc-adopted", "vpc", adoptedCIDR, "", "")
	record := reviewedRecord()
	seedPendingHold(t, ledger, adopted, adoptOperation, &record)

	if err := s.recoverReservations(ctx); err != nil {
		t.Fatalf("recover reservations: %v", err)
	}
	if len(inventory.adopts) != 0 {
		t.Fatalf("the reserve recovery asked the inventory to adopt: %#v", inventory.adopts)
	}
	if inventory.ensures != 1 {
		t.Fatalf("want one Ensure for the one pending reservation, got %d", inventory.ensures)
	}
	if got := safetyState(t, ledger, adopted.ID); got.Committed {
		t.Fatalf("the reserve recovery committed an adoption: %#v", got)
	}

	if err := s.recoverAdoptions(ctx); err != nil {
		t.Fatalf("recover adoptions: %v", err)
	}
	if inventory.ensures != 1 {
		t.Fatalf("the adoption recovery called Ensure: %d calls in total", inventory.ensures)
	}
	if len(inventory.adopts) != 1 || inventory.adopts[0].ID != adopted.ID {
		t.Fatalf("the adoption recovery asked for the wrong allocation: %#v", inventory.adopts)
	}
	if got := safetyState(t, ledger, adopted.ID); !got.Committed || got.InventoryID != adoptedNetworkID {
		t.Fatalf("adopted allocation after recovery: %#v", got)
	}
	if got := safetyState(t, ledger, reserved.ID); !got.Committed || got.InventoryID != "netbox-"+reserved.ID {
		t.Fatalf("reserved allocation after recovery: %#v", got)
	}
}

// Uncertainty is not a refusal. Without current, complete, trustworthy cloud
// evidence the recovery does nothing at all: it neither converts anything nor
// tells anybody something is wrong, because nothing is known to be.
func TestAdoptionRecoveryDoesNothingWithoutTrustworthyObservation(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for name, observation := range map[string]domain.Observation{
		"incomplete":         {DomainID: "d", Generation: "g", Complete: false, StartedAt: now.Add(-time.Minute), FinishedAt: now, Resources: []domain.Resource{adoptedResource()}},
		"wrong_generation":   {DomainID: "d", Generation: "old-generation", Complete: true, StartedAt: now.Add(-time.Minute), FinishedAt: now, Resources: []domain.Resource{adoptedResource()}},
		"missing_started_at": {DomainID: "d", Generation: "g", Complete: true, FinishedAt: now, Resources: []domain.Resource{adoptedResource()}},
		"stale":              {DomainID: "d", Generation: "g", Complete: true, StartedAt: now.Add(-3 * time.Hour), FinishedAt: now.Add(-2 * time.Hour), Resources: []domain.Resource{adoptedResource()}},
	} {
		t.Run(name, func(t *testing.T) {
			c := crashedAdoption(t, now)
			c.observer.observation = observation

			if err := c.service.recoverAdoptions(context.Background()); err != nil {
				t.Fatalf("recover adoptions: %v", err)
			}
			if len(c.inventory.adopts) != 0 {
				t.Fatalf("recovery asked the inventory to adopt on untrustworthy evidence: %#v", c.inventory.adopts)
			}
			assertStillPending(t, c.ledger, c.allocation.ID)
			if f := findingsWithCode(t, c.ledger, "adoption_stuck"); len(f) != 0 {
				t.Fatalf("uncertainty raised a finding: %#v", f)
			}
		})
	}
}

// An incomplete inventory snapshot is uncertainty of the same kind, and the
// adapter is not asked on it either.
func TestAdoptionRecoveryDoesNothingUnderAnIncompleteSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := crashedAdoption(t, now)
	c.inventory.incomplete = true

	if err := c.service.recoverAdoptions(context.Background()); err != nil {
		t.Fatalf("recover adoptions: %v", err)
	}
	if len(c.inventory.adopts) != 0 {
		t.Fatalf("recovery asked the inventory to adopt on an incomplete snapshot: %#v", c.inventory.adopts)
	}
	assertStillPending(t, c.ledger, c.allocation.ID)
	if f := findingsWithCode(t, c.ledger, "adoption_stuck"); len(f) != 0 {
		t.Fatalf("an incomplete snapshot raised a finding: %#v", f)
	}
}

// The ground the operator reviewed can also move while the operation is
// pending. A complete, current observation that no longer shows the reviewed
// resource alone at the adopted CIDR, or a snapshot whose network is now
// somebody else's, is not uncertainty: it is a question only a person can
// answer, and the adapter is never asked.
func TestAdoptionRecoveryRefusesWhenTheReviewedEvidenceHasChanged(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	claimedNetwork := adoptedNetwork()
	claimedNetwork.AllocationID, claimedNetwork.Owned = "alloc-someone-else", true
	neighbour := domain.Resource{
		Type: "vpc", ID: "vpc-neighbour", AccountID: "123456789012", Region: "eu",
		CIDR: "10.7.0.128/25", CIDRs: []string{"10.7.0.128/25"},
	}
	cases := map[string]struct {
		networks  []domain.Network
		resources []domain.Resource
	}{
		"the reviewed prefix is gone":           {nil, []domain.Resource{adoptedResource()}},
		"the reviewed prefix has another owner": {[]domain.Network{claimedNetwork}, []domain.Resource{adoptedResource()}},
		"the import tag was removed":            {[]domain.Network{{ID: adoptedNetworkID, CIDR: adoptedCIDR}}, []domain.Resource{adoptedResource()}},
		"a second network overlaps it":          {[]domain.Network{adoptedNetwork(), {ID: "4243", CIDR: "10.7.0.128/25", Imported: true}}, []domain.Resource{adoptedResource()}},
		"the reviewed resource is gone":         {[]domain.Network{adoptedNetwork()}, nil},
		"a second resource overlaps it":         {[]domain.Network{adoptedNetwork()}, []domain.Resource{adoptedResource(), neighbour}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := crashedAdoption(t, now)
			c.inventory.networks = tc.networks
			c.observer.observation = safetyObservation(now, tc.resources...)

			if err := c.service.recoverAdoptions(context.Background()); err != nil {
				t.Fatalf("recover adoptions: %v", err)
			}
			if len(c.inventory.adopts) != 0 {
				t.Fatalf("recovery asked the inventory to adopt evidence it had already refused: %#v", c.inventory.adopts)
			}
			assertStillPending(t, c.ledger, c.allocation.ID)
			found := findingsWithCode(t, c.ledger, "adoption_stuck")
			if len(found) != 1 || found[0].Severity != "CRITICAL" || found[0].Status != "OPEN" {
				t.Fatalf("want one open CRITICAL adoption_stuck finding, got %#v", found)
			}
		})
	}
}

// The likeliest crash of all is the one between the adapter's write and the
// commit that was meant to follow it. The prefix then already carries this
// allocation's markers, and the adapter converges on it without writing again;
// the snapshot guard has to recognise that object as this operation's own work
// rather than as somebody else's ownership, or the commit could never happen
// and the domain would stay fenced over a conversion that had already
// succeeded.
func TestAdoptionRecoveryConvergesOnThePrefixItAlreadyConverted(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := crashedAdoption(t, now)
	converted := adoptedNetwork()
	converted.Owned, converted.AllocationID = true, c.allocation.ID
	for _, o := range ledgerState(t, c.ledger).Operations {
		if o.AllocationID == c.allocation.ID {
			converted.OperationID = o.ID
		}
	}
	c.inventory.networks = []domain.Network{converted}

	if err := c.service.recoverAdoptions(context.Background()); err != nil {
		t.Fatalf("recover adoptions: %v", err)
	}
	if len(c.inventory.adopts) != 1 {
		t.Fatalf("want the adapter asked exactly once to converge, got %d calls", len(c.inventory.adopts))
	}
	got := safetyState(t, c.ledger, c.allocation.ID)
	if !got.Committed || got.InventoryID != adoptedNetworkID {
		t.Fatalf("recovery did not converge on the prefix it had already converted: %#v", got)
	}
	if f := findingsWithCode(t, c.ledger, "adoption_stuck"); len(f) != 0 {
		t.Fatalf("converging on its own write raised a finding: %#v", f)
	}
}

// Inventory.Adopt locates the prefix by CIDR and VRF rather than by id, so the
// worker repeats the comparison the operator's own process makes; the reviewed
// id is durable for exactly this. The write has already happened when the
// answer disagrees, and there is no undo, so the only correct outcome is to
// leave it pending and tell a person -- once, however often the pass repeats.
func TestAdoptionRecoveryRefusesAnInventoryObjectThatIsNotTheReviewedOne(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := crashedAdoption(t, now)
	c.inventory.adoptID = "9999"
	ctx := context.Background()

	if err := c.service.recoverAdoptions(ctx); err != nil {
		t.Fatalf("recover adoptions: %v", err)
	}
	assertStillPending(t, c.ledger, c.allocation.ID)
	found := findingsWithCode(t, c.ledger, "adoption_stuck")
	if len(found) != 1 || found[0].Severity != "CRITICAL" || found[0].Status != "OPEN" || found[0].AllocationID != c.allocation.ID || found[0].TenantID != "t" {
		t.Fatalf("want one open CRITICAL adoption_stuck finding on the allocation, got %#v", found)
	}
	if _, err := c.service.Get(ctx, safetyPrincipal(), c.allocation.ID); err == nil {
		t.Fatal("an uncommitted adoption is visible to GET")
	}

	if err := c.service.recoverAdoptions(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if again := findingsWithCode(t, c.ledger, "adoption_stuck"); len(again) != 1 {
		t.Fatalf("a second pass added a second finding: %#v", again)
	}
	assertStillPending(t, c.ledger, c.allocation.ID)
}

// An operation planned before the reviewed record was durable cannot prove
// anything about what the adapter converted, so it is stuck by construction
// rather than adopted on trust.
func TestAdoptionRecoveryRefusesAnOperationWithoutTheReviewedRecord(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{networks: []domain.Network{adoptedNetwork()}, adoptID: adoptedNetworkID}
	s := adoptService(now, ledger, inventory, &safetyObserver{observation: safetyObservation(now, adoptedResource())})
	a := safetyAllocation(now, "alloc-recordless", "vpc", adoptedCIDR, "", "")
	seedPendingHold(t, ledger, a, adoptOperation, nil)

	if err := s.recoverAdoptions(context.Background()); err != nil {
		t.Fatalf("recover adoptions: %v", err)
	}
	if len(inventory.adopts) != 0 {
		t.Fatalf("a recordless adoption reached the adapter: %#v", inventory.adopts)
	}
	assertStillPending(t, ledger, a.ID)
	if found := findingsWithCode(t, ledger, "adoption_stuck"); len(found) != 1 || found[0].Severity != "CRITICAL" {
		t.Fatalf("want one CRITICAL adoption_stuck finding, got %#v", found)
	}
}

// Every refusal the adapter can reach means the object it was asked about is
// not the one that was reviewed, or is not there at all. None of them will
// answer differently on the next pass, so each is somebody's to look at.
func TestEachAdapterRefusalLeavesTheAdoptionPendingWithOneFinding(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for name, refusal := range map[string]error{
		"already_owned":      netbox.ErrAdoptManaged,
		"import_tag_lost":    netbox.ErrAdoptNotImported,
		"prefix_missing":     netbox.ErrAdoptNotFound,
		"conflicting_object": netbox.ErrAdoptConflict,
		"read_back_is_alien": netbox.ErrAdoptReadBack,
	} {
		t.Run(name, func(t *testing.T) {
			c := crashedAdoption(t, now)
			c.inventory.adoptErr = fmt.Errorf("%w: prefix 4242", refusal)

			if err := c.service.recoverAdoptions(context.Background()); err != nil {
				t.Fatalf("recover adoptions: %v", err)
			}
			if len(c.inventory.adopts) != 1 {
				t.Fatalf("want the adapter asked exactly once, got %d calls", len(c.inventory.adopts))
			}
			assertStillPending(t, c.ledger, c.allocation.ID)
			found := findingsWithCode(t, c.ledger, "adoption_stuck")
			if len(found) != 1 || found[0].Severity != "CRITICAL" || found[0].Status != "OPEN" {
				t.Fatalf("want one open CRITICAL adoption_stuck finding, got %#v", found)
			}
		})
	}
}

// A timeout says nothing about the prefix: the patch may have landed or may
// not. Adopt writes nothing until every check has passed and converges on an
// object it already converted, so the operation simply waits for the next pass
// -- and waiting is not a finding, or every flaky minute would look like a
// stuck adoption.
func TestAnUncertainAdapterErrorIsRetriedWithoutAFinding(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for name, uncertain := range map[string]error{
		"deadline_exceeded": context.DeadlineExceeded,
		// The shape the adapter really returns for a lost NetBox call.
		"wrapped_by_the_adapter": fmt.Errorf("uncertain NetBox adoption (operation remains pending): %w", context.DeadlineExceeded),
		// A 5xx is not a timeout, and it is not a decision about the prefix
		// either: the adapter marks it, and recovery must not raise a CRITICAL
		// finding because the inventory had a bad minute.
		"unanswered_5xx":  fmt.Errorf("%w: patching prefix 42: netbox PATCH: HTTP 503", domain.ErrInventoryUncertain),
		"network_timeout": &net.DNSError{Err: "i/o timeout", Name: "netbox", IsTimeout: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := crashedAdoption(t, now)
			c.inventory.adoptErr = uncertain
			ctx := context.Background()

			if err := c.service.recoverAdoptions(ctx); err != nil {
				t.Fatalf("first pass: %v", err)
			}
			assertStillPending(t, c.ledger, c.allocation.ID)
			if f := findingsWithCode(t, c.ledger, "adoption_stuck"); len(f) != 0 {
				t.Fatalf("an uncertain adapter error raised a finding: %#v", f)
			}

			c.inventory.adoptErr = nil
			if err := c.service.recoverAdoptions(ctx); err != nil {
				t.Fatalf("second pass: %v", err)
			}
			got := safetyState(t, c.ledger, c.allocation.ID)
			if !got.Committed || got.InventoryID != adoptedNetworkID {
				t.Fatalf("the next pass did not commit the retried adoption: %#v", got)
			}
			if len(c.inventory.adopts) != 2 {
				t.Fatalf("want the adapter asked once per pass, got %d calls", len(c.inventory.adopts))
			}
		})
	}
}

// A finding raised while the adapter was refusing has to close when the
// adoption finally lands, or an operator who repaired the inventory is left
// reading a CRITICAL about work that is done.
func TestCommittingARecoveredAdoptionResolvesItsFinding(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := crashedAdoption(t, now)
	c.inventory.adoptErr = fmt.Errorf("%w: prefix 4242", netbox.ErrAdoptNotImported)
	ctx := context.Background()

	if err := c.service.recoverAdoptions(ctx); err != nil {
		t.Fatalf("refusing pass: %v", err)
	}
	c.inventory.adoptErr = nil
	if err := c.service.recoverAdoptions(ctx); err != nil {
		t.Fatalf("recovering pass: %v", err)
	}
	found := findingsWithCode(t, c.ledger, "adoption_stuck")
	if len(found) != 1 || found[0].Status != "RESOLVED" {
		t.Fatalf("want the stuck finding resolved by the commit, got %#v", found)
	}
}

// The reviewed record is durable because the process that reviewed it may not
// be the process that finishes the work. The memory ledger clones state by
// marshalling it to JSON and back, which is exactly how the PostgreSQL ledger
// stores an operation -- one jsonb payload per row, decoded into
// domain.Operation on load -- so a field that did not survive this would not
// survive a restart either. The PostgreSQL round trip has a test of its own in
// internal/storage, which runs only against a configured database.
func TestTheReviewedRecordSurvivesALedgerRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := crashedAdoption(t, now)

	records := 0
	for _, o := range ledgerState(t, c.ledger).Operations {
		if o.AllocationID != c.allocation.ID {
			continue
		}
		if o.Adoption == nil {
			t.Fatalf("the reviewed record did not survive the ledger: %#v", o)
		}
		if *o.Adoption != reviewedRecord() {
			t.Fatalf("reloaded record: %#v", *o.Adoption)
		}
		records++
	}
	if records != 1 {
		t.Fatalf("want one pending operation carrying the record, got %d", records)
	}
}

// An adopted allocation is born RESERVED over a resource the platform is not
// allowed to tag, and the reconciler has to treat that as the ordinary wait it
// is: no CRITICAL, no occupancy warning against a network that now has an
// owner, and the existing promotion path the moment the owning team tags it.
func TestAnAdoptedAllocationWaitsWithoutACriticalAndActivatesOnTags(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := crashedAdoption(t, now)
	ctx := context.Background()
	unmanaged := domain.Resource{
		Type: "vpc", ID: "vpc-nobodys", AccountID: "123456789012", Region: "eu",
		CIDR: "10.20.0.0/24", CIDRs: []string{"10.20.0.0/24"},
	}
	c.observer.observation = safetyObservation(now, adoptedResource(), unmanaged)

	if err := c.service.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	adopted, err := c.service.Get(ctx, safetyPrincipal(), c.allocation.ID)
	if err != nil || adopted.State != domain.Reserved || !adopted.Committed {
		t.Fatalf("adopted allocation after reconciliation: %#v %v", adopted, err)
	}
	assertNoOpenCritical(t, c.service, "while the adopted resource is untagged")
	// The exemption is narrow. A resource the ledger owns but cannot tag is not
	// unmanaged occupancy; a resource nothing owns still is, and there is
	// exactly one of those.
	occupancy := findingsWithCode(t, c.ledger, "unmanaged_occupancy")
	if len(occupancy) != 1 || occupancy[0].Status != "OPEN" || occupancy[0].TenantID != "t" {
		t.Fatalf("want occupancy reported for the unowned resource alone, got %#v", occupancy)
	}

	// The owning team tags the VPC with its own provisioning credentials, which
	// is the only way the two platform tags can appear. The existing reconciler
	// then promotes it: no new path, and no second constructor.
	tagged := adoptedResource()
	tagged.Tags = map[string]string{
		"platform-ipam:allocation-id":  c.allocation.ID,
		"platform-ipam:allocation-key": c.allocation.AllocationKey,
	}
	c.observer.observation = safetyObservation(now, tagged, unmanaged)
	if err := c.service.Tick(ctx); err != nil {
		t.Fatalf("tick after tagging: %v", err)
	}
	active, err := c.service.Get(ctx, safetyPrincipal(), c.allocation.ID)
	if err != nil || active.State != domain.Active || active.Binding == nil || active.Binding.ResourceID != adoptedResourceID || active.Binding.VerifiedAt == nil {
		t.Fatalf("tagging did not activate the adopted allocation: %#v %v", active, err)
	}
	verified := 0
	for _, e := range allocationEvents(ledgerState(t, c.ledger), c.allocation.ID) {
		if e.Action == "binding_verified" {
			verified++
		}
	}
	if verified != 1 {
		t.Fatalf("want one binding_verified event, got %d", verified)
	}
	assertNoOpenCritical(t, c.service, "after the owning team tagged the resource")
}

// The exemption belongs to adoptions and to nothing else. An untagged resource
// sitting exactly on an ordinary reservation was never reviewed by anybody, so
// it stays reported: that finding is how its owner learns to tag it, and a
// resource that merely looks like the allocation is not thereby the
// allocation's.
func TestAnUntaggedResourceOnAnOrdinaryReservationIsStillReported(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{}
	observer := &safetyObserver{observation: safetyObservation(now)}
	s := adoptService(now, ledger, inventory, observer)
	ctx := context.Background()

	reserved, _, status, err := s.Reserve(ctx, safetyPrincipal(), safetyRequest("orders"), "consumer-key-1")
	if err != nil || status != 201 {
		t.Fatalf("reservation: status=%d err=%v", status, err)
	}
	untagged := domain.Resource{
		Type: "vpc", ID: "vpc-untagged", AccountID: reserved.AccountID, Region: reserved.Region,
		CIDR: reserved.CIDR, CIDRs: []string{reserved.CIDR},
	}
	observer.observation = safetyObservation(now, untagged)
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	occupancy := findingsWithCode(t, ledger, "unmanaged_occupancy")
	if len(occupancy) != 1 || occupancy[0].Status != "OPEN" {
		t.Fatalf("want the untagged resource reported, got %#v", occupancy)
	}
}

// The same holds for an adoption that never crashed: the record that exempts
// the resource is written by the body, not by the recovery.
func TestADirectlyCommittedAdoptionIsNotReportedAsUnmanagedOccupancy(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, _ := adoptReady(now)
	ctx := context.Background()
	if _, _, status, err := s.Adopt(ctx, safetyPrincipal(), safetyRequest("orders"), adoptPin()); err != nil || status != 201 {
		t.Fatalf("adoption: status=%d err=%v", status, err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if occupancy := findingsWithCode(t, ledger, "unmanaged_occupancy"); len(occupancy) != 0 {
		t.Fatalf("an adopted network was reported as unmanaged occupancy: %#v", occupancy)
	}
	assertNoOpenCritical(t, s, "after a direct adoption")
}

// The exemption names the reviewed resource, not the address space: a second
// untagged resource with the adopted network's exact shape is somebody else's.
func TestASecondResourceShapedLikeTheAdoptedOneIsStillReported(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := crashedAdoption(t, now)
	ctx := context.Background()
	if err := c.service.Tick(ctx); err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	twin := adoptedResource()
	twin.ID = "vpc-twin"
	c.observer.observation = safetyObservation(now, adoptedResource(), twin)
	if err := c.service.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	occupancy := findingsWithCode(t, c.ledger, "unmanaged_occupancy")
	if len(occupancy) != 1 || occupancy[0].Status != "OPEN" {
		t.Fatalf("want the twin reported and the adopted resource not, got %#v", occupancy)
	}
}

// What the operator reviewed was one resource of one shape, unclaimed. The same
// resource id with another primary CIDR, or carrying a claim the ledger does
// not know, is no longer that resource as far as this exemption goes.
func TestTheAdoptedResourceIsReportedOnceItIsNoLongerWhatWasReviewed(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	reshaped := adoptedResource()
	reshaped.CIDR, reshaped.CIDRs = "10.7.0.0/25", []string{"10.7.0.0/25"}
	claimed := adoptedResource()
	claimed.Tags = map[string]string{"platform-ipam:allocation-id": "alloc-nobody-knows"}
	for name, resource := range map[string]domain.Resource{"another_primary_cidr": reshaped, "a_foreign_claim": claimed} {
		t.Run(name, func(t *testing.T) {
			s, ledger, _ := adoptReady(now)
			ctx := context.Background()
			if _, _, status, err := s.Adopt(ctx, safetyPrincipal(), safetyRequest("orders"), adoptPin()); err != nil || status != 201 {
				t.Fatalf("adoption: status=%d err=%v", status, err)
			}
			s.observer.(*safetyObserver).observation = safetyObservation(now, resource)
			if err := s.Tick(ctx); err != nil {
				t.Fatalf("tick: %v", err)
			}
			occupancy := findingsWithCode(t, ledger, "unmanaged_occupancy")
			if len(occupancy) != 1 || occupancy[0].Status != "OPEN" {
				t.Fatalf("want the changed resource reported, got %#v", occupancy)
			}
		})
	}
}

func assertNoOpenCritical(t *testing.T, s *Service, when string) {
	t.Helper()
	findings, err := s.Findings(context.Background(), safetyPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Severity == "CRITICAL" && f.Status == "OPEN" {
			t.Fatalf("an adopted allocation raised a CRITICAL %s: %#v", when, f)
		}
	}
}

// A worker pass and the request that planned the hold are separate processes.
// The worker can flag a pending adoption -- its own adapter call met a 412
// because the request's write landed first, say -- a moment before that request
// commits. Nothing but a commit resolves adoption_stuck, and the worker's
// recovery never runs again for a hold that is no longer pending, so the
// request's own commit has to close it (found while reviewing package H4, which
// fixed the same thing for reservation_stuck).
func TestTheRequestsOwnCommitResolvesAFindingARacingWorkerRaised(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		code  string
		start func(s *Service) (*domain.Allocation, int, error)
	}{
		"adoption": {adoptionStuckCode, func(s *Service) (*domain.Allocation, int, error) {
			a, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
			return a, status, err
		}},
		"reservation": {reservationStuckCode, func(s *Service) (*domain.Allocation, int, error) {
			a, _, status, err := s.Reserve(context.Background(), safetyPrincipal(), safetyRequest("billing"), "consumer-key-1")
			return a, status, err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := storage.NewMemoryLedger()
			racing := &racingInventory{safetyInventory: safetyInventory{networks: []domain.Network{adoptedNetwork()}, adoptID: adoptedNetworkID}}
			observer := &safetyObserver{observation: safetyObservation(now, adoptedResource())}
			s := adoptService(now, ledger, &racing.safetyInventory, observer)
			s.inventory = racing
			// The "worker" strikes while the request is inside its adapter call:
			// the hold is durable, pending, and not yet committed.
			racing.during = func(a domain.Allocation) {
				if err := s.flagStuckHold(context.Background(), a, tc.code); err != nil {
					t.Fatalf("flagging the pending hold: %v", err)
				}
				if open := findingsWithCode(t, ledger, tc.code); len(open) != 1 || open[0].Status != "OPEN" {
					t.Fatalf("the racing worker did not raise %s: %#v", tc.code, open)
				}
			}
			a, status, err := tc.start(s)
			if err != nil || status != 201 || a == nil || !a.Committed {
				t.Fatalf("the request did not commit: allocation=%#v status=%d err=%v", a, status, err)
			}
			for _, f := range findingsWithCode(t, ledger, tc.code) {
				if f.Status != "RESOLVED" {
					t.Fatalf("%s stayed %s after the request's own commit: %#v", tc.code, f.Status, f)
				}
			}
		})
	}
}

// racingInventory runs a callback inside the adapter call, which is the only
// moment a test can stand where a concurrent worker pass stands.
type racingInventory struct {
	safetyInventory
	during func(domain.Allocation)
}

func (r *racingInventory) Adopt(ctx context.Context, a domain.Allocation, operationID string) (string, error) {
	if r.during != nil {
		r.during(a)
	}
	return r.safetyInventory.Adopt(ctx, a, operationID)
}

func (r *racingInventory) Ensure(ctx context.Context, a domain.Allocation, operationID string) (string, error) {
	if r.during != nil {
		r.during(a)
	}
	return r.safetyInventory.Ensure(ctx, a, operationID)
}
