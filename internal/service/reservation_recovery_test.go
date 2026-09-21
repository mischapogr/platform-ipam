package service

// A stuck reservation is the twin defect package H1 found while it argued why
// a pending RESERVE is out of ADR 0012's scope: a pending RESERVE fences its
// overlap domain exactly as a pending ADOPT does, and until package H4
// recoverReservations (internal/service/worker.go) raised nothing at all --
// every consumer reservation there answered 503 domain_busy with no visible
// reason. These tests mirror adopt_recovery_test.go's shape: they assert on
// both halves of every outcome -- what reached the ledger, and what the
// inventory double was asked to do.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

// reservationUnderRecovery is a planned reservation whose adapter call was
// lost -- the RESERVE-side twin of adoptionUnderRecovery.
type reservationUnderRecovery struct {
	service    *Service
	ledger     *storage.MemoryLedger
	inventory  *safetyInventory
	observer   *safetyObserver
	allocation domain.Allocation
}

// crashedReservation leaves exactly what a crash leaves: the durable hold,
// the pending RESERVE operation fencing the domain, the planned audit event,
// and no commit. It goes through the real Reserve path (not seeded rows), so
// the operation's UpdatedAt -- what flagAgedReservation's threshold is
// measured against -- is the one a real request would have persisted.
func crashedReservation(t *testing.T, now time.Time) reservationUnderRecovery {
	t.Helper()
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{ensureErr: errors.New("the adapter never answered")}
	observer := &safetyObserver{observation: safetyObservation(now)}
	s := safetyService(now, ledger, inventory, observer)

	a, op, status, err := s.Reserve(context.Background(), safetyPrincipal(), safetyRequest("stuck-reservation"), "stuck-reservation-key")
	if err != nil || status != 202 || a == nil || a.Committed || op == nil || op.Status != operationPending || op.Type != reserveOperation {
		t.Fatalf("planning a reservation whose adapter call is then lost: allocation=%#v operation=%#v status=%d err=%v", a, op, status, err)
	}
	// The planning call was not the recovery's. A test that counts adapter
	// calls counts the ones the worker made.
	inventory.ensureErr, inventory.ensures = nil, 0
	return reservationUnderRecovery{s, ledger, inventory, observer, *a}
}

// aged moves the worker's clock (and a matching fresh observation) well past
// flagAgedReservation's threshold -- one observation age past the
// reservation's creation time, which is fixed in the operation's UpdatedAt
// and never touched again while the hold stays pending. Refusal tests use
// this so the classification they exercise is not entangled with the
// flapping rule, which has tests of its own.
func (c reservationUnderRecovery) aged(now time.Time, resources ...domain.Resource) {
	when := now.Add(2 * time.Hour)
	c.service.SetClock(func() time.Time { return when })
	c.observer.observation = safetyObservation(when, resources...)
}

// assertReservationStillPending is the shape every refused recovery has to
// leave: nothing committed, nothing visible, the operation still fencing its
// domain for a person, and no commit event claiming otherwise.
func assertReservationStillPending(t *testing.T, ledger *storage.MemoryLedger, id string) {
	t.Helper()
	st := ledgerState(t, ledger)
	a, ok := st.Allocations[id]
	if !ok || a.Committed || a.InventoryID != "" {
		t.Fatalf("a refused recovery committed the reservation: %#v", a)
	}
	pending := 0
	for _, o := range st.Operations {
		if o.AllocationID == id && o.Type == reserveOperation && o.Status == operationPending {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("want the reservation to stay pending, got %d pending RESERVE operations", pending)
	}
	for _, e := range allocationEvents(st, id) {
		if e.Action == reserveKind.committed {
			t.Fatalf("a refused recovery wrote a commit event: %#v", e)
		}
	}
}

func TestWorkerRecoversAPendingReservationInsteadOfWedgingTheDomain(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := crashedReservation(t, now)
	ctx := context.Background()

	// The whole reason this path exists: until the reservation resolves, the
	// pending operation fences every consumer reservation in its overlap
	// domain.
	_, _, _, err := c.service.Reserve(ctx, safetyPrincipal(), safetyRequest("neighbour"), "neighbour-key")
	safetyAPIError(t, err, 503, "domain_busy")

	if err := c.service.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	committed, err := c.service.Get(ctx, safetyPrincipal(), c.allocation.ID)
	if err != nil || !committed.Committed || committed.State != domain.Reserved || committed.CIDR != c.allocation.CIDR || committed.InventoryID == "" {
		t.Fatalf("recovered reservation: %#v %v", committed, err)
	}
	if c.inventory.ensures != 1 {
		t.Fatalf("want the worker to ask the inventory to ensure exactly once, got %d", c.inventory.ensures)
	}
	if len(c.inventory.adopts) != 0 {
		t.Fatalf("recovery called Adopt: %#v", c.inventory.adopts)
	}
	if f := findingsWithCode(t, c.ledger, "reservation_stuck"); len(f) != 0 {
		t.Fatalf("a recovered reservation raised findings: %#v", f)
	}

	// And the domain is open again, which is the availability half of the fix.
	a, _, status, err := c.service.Reserve(ctx, safetyPrincipal(), safetyRequest("neighbour"), "neighbour-key")
	if err != nil || status != 201 || a == nil {
		t.Fatalf("the overlap domain is still fenced after recovery: allocation=%#v status=%d err=%v", a, status, err)
	}
}

// Uncertainty is not a refusal. Without current, complete, trustworthy cloud
// evidence the recovery does nothing at all: it neither converts anything nor
// tells anybody something is wrong, because nothing is known to be. This
// mirrors adopt_recovery_test.go's TestAdoptionRecoveryDoesNothingWithout
// TrustworthyObservation exactly -- both recovery paths share recoveryEvidence.
func TestReservationRecoveryDoesNothingWithoutTrustworthyObservation(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for name, observation := range map[string]domain.Observation{
		"incomplete":         {DomainID: "d", Generation: "g", Complete: false, StartedAt: now.Add(-time.Minute), FinishedAt: now},
		"wrong_generation":   {DomainID: "d", Generation: "old-generation", Complete: true, StartedAt: now.Add(-time.Minute), FinishedAt: now},
		"missing_started_at": {DomainID: "d", Generation: "g", Complete: true, FinishedAt: now},
		"stale":              {DomainID: "d", Generation: "g", Complete: true, StartedAt: now.Add(-3 * time.Hour), FinishedAt: now.Add(-2 * time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			c := crashedReservation(t, now)
			c.observer.observation = observation

			if err := c.service.recoverReservations(context.Background()); err != nil {
				t.Fatalf("recover reservations: %v", err)
			}
			if c.inventory.ensures != 0 {
				t.Fatalf("recovery asked the inventory on untrustworthy evidence: %d calls", c.inventory.ensures)
			}
			assertReservationStillPending(t, c.ledger, c.allocation.ID)
			if f := findingsWithCode(t, c.ledger, "reservation_stuck"); len(f) != 0 {
				t.Fatalf("uncertainty raised a finding: %#v", f)
			}
		})
	}
}

// A timeout, a cancellation, or the adapter's own domain.ErrInventoryUncertain
// say nothing about the CIDR: the create may or may not have landed. The
// operation simply waits for the next pass, and waiting is not a finding.
func TestAnUncertainEnsureErrorIsRetriedWithoutAFinding(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for name, uncertain := range map[string]error{
		"deadline_exceeded": context.DeadlineExceeded,
		"cancelled":         context.Canceled,
		// The shape the netbox adapter really returns for a lost NetBox call
		// (internal/netbox/client.go's uncertainEnsure, added for this package).
		"unanswered_5xx":  fmt.Errorf("%w: uncertain NetBox reservation (operation remains pending): listing prefixes marked for alloc-1: netbox GET: HTTP 503", domain.ErrInventoryUncertain),
		"network_timeout": &net.DNSError{Err: "i/o timeout", Name: "netbox", IsTimeout: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := crashedReservation(t, now)
			c.aged(now) // uncertainty must stay silent however old the hold is
			c.inventory.ensureErr = uncertain
			ctx := context.Background()

			if err := c.service.recoverReservations(ctx); err != nil {
				t.Fatalf("first pass: %v", err)
			}
			assertReservationStillPending(t, c.ledger, c.allocation.ID)
			if f := findingsWithCode(t, c.ledger, "reservation_stuck"); len(f) != 0 {
				t.Fatalf("an uncertain adapter error raised a finding: %#v", f)
			}

			c.inventory.ensureErr = nil
			if err := c.service.recoverReservations(ctx); err != nil {
				t.Fatalf("second pass: %v", err)
			}
			got := safetyState(t, c.ledger, c.allocation.ID)
			if !got.Committed || got.InventoryID == "" {
				t.Fatalf("the next pass did not commit the retried reservation: %#v", got)
			}
		})
	}
}

// A decision Ensure reaches on evidence it read -- the CIDR is occupied, a
// duplicate marker, a marker conflict, anything that is not
// uncertainInventoryError -- will not answer differently next pass, so it is
// somebody's to look at. Each leaves the hold pending with exactly one
// finding, and a second pass adds none.
func TestEachDefiniteEnsureRefusalLeavesTheReservationPendingWithOneFinding(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for name, refusal := range map[string]error{
		"cidr_already_occupied":     errors.New("CIDR already occupied in managed VRF"),
		"duplicate_marker":          errors.New("duplicate allocation marker in NetBox"),
		"operation_marker_mismatch": fmt.Errorf("operation marker conflict: %s", "other-op"),
		"a_plain_4xx":               errors.New("netbox GET /api/ipam/prefixes/: HTTP 400"),
	} {
		t.Run(name, func(t *testing.T) {
			c := crashedReservation(t, now)
			c.aged(now)
			c.inventory.ensureErr = refusal
			ctx := context.Background()

			if err := c.service.recoverReservations(ctx); err != nil {
				t.Fatalf("recover reservations: %v", err)
			}
			if c.inventory.ensures != 1 {
				t.Fatalf("want the adapter asked exactly once, got %d calls", c.inventory.ensures)
			}
			assertReservationStillPending(t, c.ledger, c.allocation.ID)
			found := findingsWithCode(t, c.ledger, "reservation_stuck")
			if len(found) != 1 || found[0].Severity != "CRITICAL" || found[0].Status != "OPEN" {
				t.Fatalf("want one open CRITICAL reservation_stuck finding, got %#v", found)
			}

			// A second pass, same refusal: the finding is updated, not doubled.
			if err := c.service.recoverReservations(ctx); err != nil {
				t.Fatalf("second pass: %v", err)
			}
			if again := findingsWithCode(t, c.ledger, "reservation_stuck"); len(again) != 1 {
				t.Fatalf("a second pass added a second finding: %#v", again)
			}
		})
	}
}

// A trusted, current observation that shows something other than this
// allocation's own correctly tagged resource on the held CIDR is a decision
// reached on evidence read -- ground the operator would have to look at --
// not uncertainty, even though the adapter is never asked.
func TestAnObservationUnsafeReservationRaisesReservationStuck(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := crashedReservation(t, now)
	foreign := domain.Resource{
		Type: "vpc", ID: "vpc-foreign", AccountID: c.allocation.AccountID, Region: c.allocation.Region,
		CIDR: c.allocation.CIDR, CIDRs: []string{c.allocation.CIDR},
		// No platform-ipam tags: not this allocation's own resource, and not
		// the "already created, correctly tagged" exception either.
	}
	c.aged(now, foreign)
	ctx := context.Background()

	if err := c.service.recoverReservations(ctx); err != nil {
		t.Fatalf("recover reservations: %v", err)
	}
	if c.inventory.ensures != 0 {
		t.Fatalf("recovery asked the inventory over evidence it had already refused: %d calls", c.inventory.ensures)
	}
	assertReservationStillPending(t, c.ledger, c.allocation.ID)
	found := findingsWithCode(t, c.ledger, "reservation_stuck")
	if len(found) != 1 || found[0].Severity != "CRITICAL" || found[0].Status != "OPEN" {
		t.Fatalf("want one open CRITICAL reservation_stuck finding, got %#v", found)
	}
}

// Defensive: an adapter that answers with neither an id nor an error is not
// the silence uncertainInventoryError names, so it is treated as a decision
// rather than retried forever in silence.
func TestAnEmptyInventoryIDWithNoErrorRaisesReservationStuck(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := crashedReservation(t, now)
	c.aged(now)
	c.inventory.ensureEmpty = true

	if err := c.service.recoverReservations(context.Background()); err != nil {
		t.Fatalf("recover reservations: %v", err)
	}
	assertReservationStillPending(t, c.ledger, c.allocation.ID)
	found := findingsWithCode(t, c.ledger, "reservation_stuck")
	if len(found) != 1 || found[0].Severity != "CRITICAL" {
		t.Fatalf("want one CRITICAL reservation_stuck finding, got %#v", found)
	}
}

// A finding raised while the adapter was refusing has to close when the
// reservation finally lands, or an operator who repaired the inventory is
// left reading a CRITICAL about work that is done.
func TestCommittingARecoveredReservationResolvesItsFinding(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := crashedReservation(t, now)
	c.aged(now)
	c.inventory.ensureErr = errors.New("CIDR already occupied in managed VRF")
	ctx := context.Background()

	if err := c.service.recoverReservations(ctx); err != nil {
		t.Fatalf("refusing pass: %v", err)
	}
	if found := findingsWithCode(t, c.ledger, "reservation_stuck"); len(found) != 1 || found[0].Status != "OPEN" {
		t.Fatalf("want one open finding before repair, got %#v", found)
	}

	c.inventory.ensureErr = nil
	if err := c.service.recoverReservations(ctx); err != nil {
		t.Fatalf("recovering pass: %v", err)
	}
	found := findingsWithCode(t, c.ledger, "reservation_stuck")
	if len(found) != 1 || found[0].Status != "RESOLVED" {
		t.Fatalf("want the stuck finding resolved by the commit, got %#v", found)
	}
}

// The flapping rule (docs/WORK_PLAN.md's H4 block): a consumer's own
// synchronous POST calls Ensure itself, right after persisting the same
// pending operation a worker pass can already see (api and worker are
// independent processes sharing one ledger). Raising a CRITICAL on the very
// first definite refusal would flap on a hold that is merely young and still
// racing its own creator. The rule: wait until the operation's UpdatedAt is
// older than one observation age (s.cfg.Lifecycle.MaxObservationAge) before a
// decision becomes a finding; a decision reached earlier is simply not acted
// on yet, and the next pass re-evaluates it.
func TestAYoungPendingReservationIsNotFlaggedYet(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := crashedReservation(t, now)
	c.inventory.ensureErr = errors.New("CIDR already occupied in managed VRF")

	// No clock advance at all: the hold is exactly as old as the operation
	// that created it, which is the youngest a pending reservation can be.
	if err := c.service.recoverReservations(context.Background()); err != nil {
		t.Fatalf("recover reservations: %v", err)
	}
	if c.inventory.ensures != 1 {
		t.Fatalf("want the adapter still asked even while the flag is withheld, got %d calls", c.inventory.ensures)
	}
	assertReservationStillPending(t, c.ledger, c.allocation.ID)
	if f := findingsWithCode(t, c.ledger, "reservation_stuck"); len(f) != 0 {
		t.Fatalf("a young pending reservation was flagged: %#v", f)
	}
}

func TestAnAgedPendingReservationIsFlagged(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := crashedReservation(t, now)
	c.inventory.ensureErr = errors.New("CIDR already occupied in managed VRF")
	// safetyService's Lifecycle.MaxObservationAge is 3600 seconds; one hour
	// past it is unambiguously aged.
	c.service.SetClock(func() time.Time { return now.Add(time.Hour + time.Second) })
	c.observer.observation = safetyObservation(now.Add(time.Hour + time.Second))

	if err := c.service.recoverReservations(context.Background()); err != nil {
		t.Fatalf("recover reservations: %v", err)
	}
	found := findingsWithCode(t, c.ledger, "reservation_stuck")
	if len(found) != 1 || found[0].Status != "OPEN" {
		t.Fatalf("want the aged reservation flagged, got %#v", found)
	}
}

// A misconfigured or unset MaxObservationAge (threshold <= 0) must not
// silently suppress the finding forever: it is treated as no delay.
func TestAZeroObservationAgeFlagsImmediately(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := crashedReservation(t, now)
	c.service.cfg.Lifecycle.MaxObservationAge = 0
	c.inventory.ensureErr = errors.New("CIDR already occupied in managed VRF")

	if err := c.service.recoverReservations(context.Background()); err != nil {
		t.Fatalf("recover reservations: %v", err)
	}
	if found := findingsWithCode(t, c.ledger, "reservation_stuck"); len(found) != 1 {
		t.Fatalf("want an immediate finding under a zero threshold, got %#v", found)
	}
}

// The finding is tenant-scoped with the allocation id (workerFinding), so the
// consumer who holds the 202 can see why it never completes even though the
// uncommitted allocation itself answers 404; the operator sees it too
// (G3b3's allocation-scoped findings reach an operator unmerged); another
// tenant sees neither.
func TestReservationStuckReachesTenantAndOperatorButNotAnotherTenant(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := crashedReservation(t, now)
	c.aged(now)
	c.inventory.ensureErr = errors.New("CIDR already occupied in managed VRF")
	ctx := context.Background()

	if err := c.service.recoverReservations(ctx); err != nil {
		t.Fatalf("recover reservations: %v", err)
	}

	tenantFindings, err := c.service.Findings(ctx, safetyPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	if !hasFindingCode(tenantFindings, c.allocation.ID, "reservation_stuck") {
		t.Fatalf("the owning tenant did not see reservation_stuck: %#v", tenantFindings)
	}

	operatorFindingsGot, err := c.service.Findings(ctx, operatorPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	if !hasFindingCode(operatorFindingsGot, c.allocation.ID, "reservation_stuck") {
		t.Fatalf("the operator did not see reservation_stuck: %#v", operatorFindingsGot)
	}

	otherTenantFindings, err := c.service.Findings(ctx, tenantUPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	if hasFindingCode(otherTenantFindings, c.allocation.ID, "reservation_stuck") {
		t.Fatalf("another tenant saw reservation_stuck: %#v", otherTenantFindings)
	}

	// The hold itself stays invisible; only the finding about it is not.
	if _, err := c.service.Get(ctx, safetyPrincipal(), c.allocation.ID); err == nil {
		t.Fatal("the uncommitted hold behind reservation_stuck became readable")
	}
}

func hasFindingCode(findings []domain.Finding, allocationID, code string) bool {
	for _, f := range findings {
		if f.AllocationID == allocationID && f.Code == code {
			return true
		}
	}
	return false
}

// The two recovery paths must still never cross: a stuck RESERVE never gets
// adoption_stuck and a stuck ADOPT never gets reservation_stuck.
// TestRecoveryNeverCrossesTheTwoOperationTypes (adopt_recovery_test.go)
// already proves the adapter calls stay separate; this proves the finding
// codes do too.
func TestAStuckReservationAndAStuckAdoptionNeverSwapCodes(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{
		networks: []domain.Network{adoptedNetwork()}, adoptID: adoptedNetworkID,
		adoptErr: fmt.Errorf("%w: prefix 4242", errors.New("adopt refused")),
	}
	observer := &safetyObserver{observation: safetyObservation(now, adoptedResource())}
	s := adoptService(now, ledger, inventory, observer)

	reserved := safetyAllocation(now, "alloc-reserved-stuck", "vpc", "10.9.0.0/24", "", "")
	seedPendingHold(t, ledger, reserved, reserveOperation, nil)
	adopted := safetyAllocation(now, "alloc-adopted-stuck", "vpc", adoptedCIDR, "", "")
	record := reviewedRecord()
	seedPendingHold(t, ledger, adopted, adoptOperation, &record)

	// Age the reservation's own operation past the flapping threshold: it was
	// seeded directly, so seedPendingHold's CreatedAt/UpdatedAt is already
	// old relative to `now` in this fixture's own clock.
	s.SetClock(func() time.Time { return now.Add(2 * time.Hour) })
	observer.observation = safetyObservation(now.Add(2*time.Hour), adoptedResource())
	inventory.ensureErr = errors.New("CIDR already occupied in managed VRF")

	if err := s.recoverReservations(ctx); err != nil {
		t.Fatalf("recover reservations: %v", err)
	}
	if err := s.recoverAdoptions(ctx); err != nil {
		t.Fatalf("recover adoptions: %v", err)
	}

	if got := findingsWithCode(t, ledger, "reservation_stuck"); len(got) != 1 || got[0].AllocationID != reserved.ID {
		t.Fatalf("want reservation_stuck on the reservation alone, got %#v", got)
	}
	if got := findingsWithCode(t, ledger, "adoption_stuck"); len(got) != 1 || got[0].AllocationID != adopted.ID {
		t.Fatalf("want adoption_stuck on the adoption alone, got %#v", got)
	}
}
