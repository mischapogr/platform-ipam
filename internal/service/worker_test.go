package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

type recoveringInventory struct {
	calls    int
	fail     bool
	cidrs    []string
	synced   []domain.Allocation
	adopts   []domain.Allocation
	adoptID  string
	abandons []domain.Allocation
	abandon  func(domain.Allocation, string, domain.PriorInventory) error
	cancels  []domain.Allocation
	cancel   func(domain.Allocation, string) error
	// syncHook, when set, decides Sync's outcome (and may itself mutate the
	// ledger, e.g. to simulate a concurrent write landing mid-pass, or cancel
	// a context to simulate the pass being cut short). Sync always records the
	// call in synced first, exactly as before.
	syncHook func(domain.Allocation) error
}

func (f *recoveringInventory) Snapshot(context.Context, domain.Domain) (domain.InventorySnapshot, error) {
	return domain.InventorySnapshot{Complete: true}, nil
}
func (f *recoveringInventory) Ensure(_ context.Context, a domain.Allocation, _ string) (string, error) {
	f.calls++
	f.cidrs = append(f.cidrs, a.CIDR)
	if f.fail {
		return "", context.DeadlineExceeded
	}
	return "123", nil
}

// Adopt records the call and refuses unless a test sets adoptID. The worker's
// recovery of a pending adoption is a later package; until it exists, a double
// that succeeded by default would make its absence look like success.
func (f *recoveringInventory) Adopt(_ context.Context, a domain.Allocation, _ string) (string, error) {
	f.adopts = append(f.adopts, a)
	if f.adoptID == "" {
		return "", errors.New("adopt not configured")
	}
	return f.adoptID, nil
}

// AbandonAdoption records the call and refuses unless a test sets abandon. No
// worker path may reach it: abandoning is an operator's decision and the worker
// only ever tries to finish an adoption (ADR 0012), so a refusal here is what
// fails a pass that started doing it on its own.
func (f *recoveringInventory) AbandonAdoption(_ context.Context, a domain.Allocation, operationID string, prior domain.PriorInventory) error {
	f.abandons = append(f.abandons, a)
	if f.abandon == nil {
		return errors.New("abandon not configured")
	}
	return f.abandon(a, operationID, prior)
}

// CancelReservation records the call and refuses unless a test sets cancel. No
// worker path may reach it either: cancelling is the consumer's decision and
// the worker only ever tries to finish a reservation (ADR 0013), so a refusal
// here is what fails a pass that started deleting prefixes on its own.
func (f *recoveringInventory) CancelReservation(_ context.Context, a domain.Allocation, operationID string) error {
	f.cancels = append(f.cancels, a)
	if f.cancel == nil {
		return errors.New("cancel not configured")
	}
	return f.cancel(a, operationID)
}
func (f *recoveringInventory) Sync(_ context.Context, a domain.Allocation) error {
	f.synced = append(f.synced, a)
	if f.syncHook != nil {
		return f.syncHook(a)
	}
	return nil
}
func (f *recoveringInventory) Delete(context.Context, domain.Allocation) error { return nil }

func TestWorkerRecoversReservedOperationAfterRestart(t *testing.T) {
	cfg := testConfig()
	ledger := storage.NewMemoryLedger()
	inv := &recoveringInventory{fail: true}
	now := time.Now().UTC()
	observer := testObserver{domain.Observation{DomainID: "d", Generation: "g", Complete: true, StartedAt: now, FinishedAt: now}}
	app := New(cfg, ledger, inv, observer)
	p := domain.Principal{TenantID: "t", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu"}}
	request := domain.Request{AllocationKey: "restart", Scope: "vpc", Environment: "prod", Region: "eu", PrefixLength: 24}
	a, o, status, err := app.Reserve(context.Background(), p, request, "lost-response")
	if err != nil || status != 202 {
		t.Fatalf("pending reservation: %d %v", status, err)
	}
	inv.fail = false
	worker := New(cfg, ledger, inv, observer)
	if err = worker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	committed, err := worker.Get(context.Background(), p, a.ID)
	if err != nil || !committed.Committed || committed.CIDR != a.CIDR {
		t.Fatalf("recovery: %+v %v", committed, err)
	}
	op, err := worker.Operation(context.Background(), p, o.ID)
	if err != nil || op.Status != operationSucceeded {
		t.Fatalf("operation: %+v %v", op, err)
	}
	if len(inv.cidrs) != 2 || inv.cidrs[0] != inv.cidrs[1] {
		t.Fatalf("retry changed candidate: %v", inv.cidrs)
	}
	if err = worker.recoverReservations(context.Background()); err != nil || inv.calls != 2 {
		t.Fatalf("committed operation repeated: %d %v", inv.calls, err)
	}
}

func TestWorkerDiscoversCloudBindingAndPreservesMissingActiveResource(t *testing.T) {
	cfg := testConfig()
	ledger := storage.NewMemoryLedger()
	now := time.Now().UTC()
	a := domain.Allocation{ID: "alloc_worker", TenantID: "t", DomainID: "d", PoolID: "p", CIDR: "10.0.0.0/24", State: domain.Reserved, Committed: true, Revision: 1, InventoryID: "123", CreatedAt: now, Request: domain.Request{AllocationKey: "worker", Scope: "vpc", AccountID: "123456789012", Region: "eu"}}
	obs := domain.Observation{DomainID: "d", Generation: "g", Complete: true, StartedAt: now, FinishedAt: now, Resources: []domain.Resource{{Type: "vpc", ID: "vpc-one", CIDR: a.CIDR, AccountID: a.AccountID, Region: a.Region, Tags: map[string]string{"platform-ipam:allocation-id": a.ID, "platform-ipam:allocation-key": a.AllocationKey}}}}
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		st.Allocations[a.ID] = a
		st.Observations["d"] = []domain.Observation{obs}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	inv := &recoveringInventory{}
	app := New(cfg, ledger, inv, nil)
	if err := app.reconcileAllocations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.syncProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	active, err := app.Get(context.Background(), domain.Principal{TenantID: "t"}, a.ID)
	if err != nil || active.State != domain.Active || active.Binding == nil || active.Binding.ResourceID != "vpc-one" {
		t.Fatalf("binding: %+v %v", active, err)
	}
	if len(inv.synced) != 1 || inv.synced[0].State != domain.Active {
		t.Fatal("active metadata not projected")
	}
	obs.Resources = nil
	if err := ledger.Update(context.Background(), func(st *domain.State) error { st.Observations["d"] = []domain.Observation{obs}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileAllocations(context.Background()); err != nil {
		t.Fatal(err)
	}
	retained, err := app.Get(context.Background(), domain.Principal{TenantID: "t"}, a.ID)
	if err != nil || retained.State != domain.Active {
		t.Fatal("cloud absence released active allocation")
	}
	findings, err := app.Findings(context.Background(), domain.Principal{TenantID: "t"})
	if err != nil || len(findings) != 1 || findings[0].Code != "resource_missing" {
		t.Fatalf("missing drift finding: %+v %v", findings, err)
	}
}

func TestCapacityIntervalUnionPreservesFragmentation(t *testing.T) {
	base := netip.MustParsePrefix("10.0.0.0/16")
	prefixes := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24"), netip.MustParsePrefix("10.0.0.128/25"), netip.MustParsePrefix("10.0.2.0/24")}
	if n := occupiedBlocks(base, 24, prefixes); n != 2 {
		t.Fatalf("nested CIDRs counted %d blocks", n)
	}
	if n := occupiedBlocks(base, 23, prefixes); n != 2 {
		t.Fatalf("fragmentation lost: %d", n)
	}
}

// countingLedger counts View and Update calls around a real ledger, so a test
// can assert syncProjections's batching directly: one Update for a whole
// pass, not one per allocation (package M4a).
type countingLedger struct {
	domain.Ledger
	updates int
	views   int
}

func (c *countingLedger) View(ctx context.Context, fn func(*domain.State) error) error {
	c.views++
	return c.Ledger.View(ctx, fn)
}
func (c *countingLedger) Update(ctx context.Context, fn func(*domain.State) error) error {
	c.updates++
	return c.Ledger.Update(ctx, fn)
}

func syncableAllocation(id string, n int) domain.Allocation {
	return domain.Allocation{ID: id, TenantID: "t", DomainID: "d", PoolID: "p", CIDR: fmt.Sprintf("10.0.%d.0/24", n), State: domain.Reserved, Committed: true, Revision: 1, InventoryID: "nb_" + id, CreatedAt: time.Now().UTC(), Request: domain.Request{AllocationKey: id, Scope: "vpc", AccountID: "123456789012", Region: "eu"}}
}

func seedAllocations(t *testing.T, ledger domain.Ledger, allocations ...domain.Allocation) {
	t.Helper()
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		for _, a := range allocations {
			st.Allocations[a.ID] = a
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSyncProjectionsBatchesTheLedgerWrite is the headline property package
// M4a exists for: internal/storage's persistState rewrites every row of every
// ledger table on every Update, so N per-allocation Update calls cost O(N^2);
// one Update for the whole pass costs O(N) (docs/DEPLOYMENT.md's dated
// measurement). Every job is still synced.
func TestSyncProjectionsBatchesTheLedgerWrite(t *testing.T) {
	cfg := testConfig()
	mem := storage.NewMemoryLedger()
	ctx := context.Background()
	ids := []string{"a1", "a2", "a3"}
	for i, id := range ids {
		seedAllocations(t, mem, syncableAllocation(id, i))
	}
	ledger := &countingLedger{Ledger: mem}
	inv := &recoveringInventory{}
	app := New(cfg, ledger, inv, nil)
	if err := app.syncProjections(ctx); err != nil {
		t.Fatal(err)
	}
	if ledger.updates != 1 {
		t.Fatalf("expected exactly one batched Update for the whole pass, got %d", ledger.updates)
	}
	if len(inv.synced) != len(ids) {
		t.Fatalf("expected every allocation synced, got %d", len(inv.synced))
	}
	snap, err := mem.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if snap.Allocations[id].InventorySync != "CURRENT" {
			t.Fatalf("%s not marked CURRENT: %+v", id, snap.Allocations[id])
		}
	}
}

// TestSyncProjectionsSkipsTheUpdateWhenNothingChanged is the other half of
// the fix: once a pass has recorded a projection as CURRENT with no open
// finding, a further pass whose Sync calls again succeed -- the common,
// steady-state case -- must not call ledger.Update (and pay persistState's
// full-ledger rewrite) at all. Sync itself still runs every pass; only the
// ledger write is what package M4a's Phase 3 narrows.
func TestSyncProjectionsSkipsTheUpdateWhenNothingChanged(t *testing.T) {
	cfg := testConfig()
	mem := storage.NewMemoryLedger()
	ctx := context.Background()
	seedAllocations(t, mem, syncableAllocation("a1", 0), syncableAllocation("a2", 1))
	ledger := &countingLedger{Ledger: mem}
	inv := &recoveringInventory{}
	app := New(cfg, ledger, inv, nil)
	if err := app.syncProjections(ctx); err != nil {
		t.Fatal(err)
	}
	if ledger.updates != 1 {
		t.Fatalf("first pass: expected one Update, got %d", ledger.updates)
	}
	ledger.updates, ledger.views = 0, 0
	if err := app.syncProjections(ctx); err != nil {
		t.Fatal(err)
	}
	if ledger.updates != 0 {
		t.Fatalf("second pass with nothing changed: expected zero Update calls, got %d", ledger.updates)
	}
	if len(inv.synced) != 4 {
		t.Fatalf("Sync itself must still run every pass: expected 4 calls total, got %d", len(inv.synced))
	}
}

// TestSyncProjectionsFailureIsolatesItsOwnAllocationAndRecovers proves a
// failing job never stops the others in the same batch (the finding it
// raises is its own, keyed on its own allocation), and that a later pass
// where the same allocation's Sync succeeds resolves that finding -- the
// success branch's resolveFinding call, not just its InventorySync write.
func TestSyncProjectionsFailureIsolatesItsOwnAllocationAndRecovers(t *testing.T) {
	cfg := testConfig()
	mem := storage.NewMemoryLedger()
	ctx := context.Background()
	ids := []string{"ok1", "bad", "ok2"}
	for i, id := range ids {
		seedAllocations(t, mem, syncableAllocation(id, i))
	}
	failing := true
	inv := &recoveringInventory{syncHook: func(a domain.Allocation) error {
		if a.ID == "bad" && failing {
			return errors.New("netbox refused")
		}
		return nil
	}}
	app := New(cfg, mem, inv, nil)
	if err := app.syncProjections(ctx); err != nil {
		t.Fatal(err)
	}
	if len(inv.synced) != len(ids) {
		t.Fatalf("a failing job must not stop the others: got %d synced", len(inv.synced))
	}
	snap, err := mem.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Allocations["ok1"].InventorySync != "CURRENT" || snap.Allocations["ok2"].InventorySync != "CURRENT" {
		t.Fatalf("succeeding allocations not marked CURRENT: %+v", snap.Allocations)
	}
	if snap.Allocations["bad"].InventorySync != "PENDING" {
		t.Fatalf("failing allocation not marked PENDING: %+v", snap.Allocations["bad"])
	}
	openFinding := func(st *domain.State) *domain.Finding {
		for id, f := range st.Findings {
			if f.AllocationID == "bad" && f.Code == "inventory_sync_pending" {
				found := st.Findings[id]
				return &found
			}
		}
		return nil
	}
	f := openFinding(snap)
	if f == nil || f.Status != "OPEN" {
		t.Fatalf("expected an open inventory_sync_pending finding for the failing allocation, got %+v", f)
	}
	failing = false
	if err := app.syncProjections(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err = mem.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Allocations["bad"].InventorySync != "CURRENT" {
		t.Fatalf("recovered allocation not marked CURRENT: %+v", snap.Allocations["bad"])
	}
	f = openFinding(snap)
	if f == nil || f.Status != "RESOLVED" {
		t.Fatalf("expected the finding resolved once the allocation recovered, got %+v", f)
	}
}

// TestSyncProjectionsLeavesAMovedAllocationAlone proves the batched write
// keeps the exact guard the old per-job write had: an allocation whose
// revision or state changed between being read for the job and the batched
// write is left alone, not overwritten with a stale InventorySync verdict.
func TestSyncProjectionsLeavesAMovedAllocationAlone(t *testing.T) {
	cfg := testConfig()
	mem := storage.NewMemoryLedger()
	ctx := context.Background()
	moved := syncableAllocation("moved", 0)
	moved.InventorySync = "PENDING"
	stable := syncableAllocation("stable", 1)
	stable.InventorySync = "PENDING"
	seedAllocations(t, mem, moved, stable)
	inv := &recoveringInventory{syncHook: func(a domain.Allocation) error {
		if a.ID != "moved" {
			return nil
		}
		// A concurrent write landing between this job's read and the batched
		// write -- exactly what reconcileAllocations or a request's own
		// commit could do mid-pass.
		return mem.Update(context.Background(), func(st *domain.State) error {
			m := st.Allocations["moved"]
			m.Revision++
			m.State = domain.Active
			st.Allocations["moved"] = m
			return nil
		})
	}}
	app := New(cfg, mem, inv, nil)
	if err := app.syncProjections(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := mem.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Allocations["moved"].InventorySync != "PENDING" {
		t.Fatalf("moved allocation's projection bookkeeping must be left alone, got %+v", snap.Allocations["moved"])
	}
	if snap.Allocations["moved"].Revision != 2 || snap.Allocations["moved"].State != domain.Active {
		t.Fatalf("the concurrent write itself must survive untouched: %+v", snap.Allocations["moved"])
	}
	if snap.Allocations["stable"].InventorySync != "CURRENT" {
		t.Fatalf("an unrelated allocation in the same batch must still be synced: %+v", snap.Allocations["stable"])
	}
}

// TestSyncProjectionsRecordsResultsGatheredBeforeCancellation proves the
// batched write keeps the property the old per-job write had for free: a
// pass cut short by ctx still records whatever it finished before that,
// rather than losing an entire batch's NetBox calls because the final write
// never got a chance to run under a still-live context.
func TestSyncProjectionsRecordsResultsGatheredBeforeCancellation(t *testing.T) {
	cfg := testConfig()
	mem := storage.NewMemoryLedger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := syncableAllocation("first", 0)
	first.InventorySync = "PENDING"
	second := syncableAllocation("second", 1)
	second.InventorySync = "PENDING"
	seedAllocations(t, mem, first, second)
	var processed string
	inv := &recoveringInventory{syncHook: func(a domain.Allocation) error {
		processed = a.ID
		cancel()
		return nil
	}}
	app := New(cfg, mem, inv, nil)
	err := app.syncProjections(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(inv.synced) != 1 {
		t.Fatalf("expected exactly one job attempted before the loop saw cancellation: %v", inv.synced)
	}
	snap, err := mem.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Allocations[processed].InventorySync != "CURRENT" {
		t.Fatalf("the result gathered before cancellation must still be recorded: %+v", snap.Allocations[processed])
	}
	other := "second"
	if processed == "second" {
		other = "first"
	}
	if snap.Allocations[other].InventorySync != "PENDING" {
		t.Fatalf("a job never reached before cancellation must be untouched: %+v", snap.Allocations[other])
	}
}

// A sync that fails because the pass was cancelled under it says nothing about
// the inventory, so it raises no finding and marks nothing PENDING.
func TestSyncProjectionsDoesNotBlameTheInventoryForACancelledCall(t *testing.T) {
	mem := storage.NewMemoryLedger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Two jobs, so that the pass notices the cancellation on its way to the
	// second and flushes what it gathered: with one job alone the flush would
	// run under the cancelled context and record nothing either way.
	for i, id := range []string{"one", "two"} {
		a := syncableAllocation(id, i)
		a.InventorySync = "CURRENT"
		seedAllocations(t, mem, a)
	}
	inv := &recoveringInventory{syncHook: func(domain.Allocation) error {
		cancel()
		return context.Canceled
	}}
	if err := New(testConfig(), mem, inv, nil).syncProjections(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	snap, err := mem.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for id, a := range snap.Allocations {
		if a.InventorySync != "CURRENT" {
			t.Fatalf("a cancelled call marked healthy allocation %s: %+v", id, a)
		}
	}
	if len(snap.Findings) != 0 {
		t.Fatalf("a cancelled call raised a finding: %+v", snap.Findings)
	}
}

// A failure that repeats is written every pass, so the finding's last-observed
// time keeps moving while the failure lasts.
func TestSyncProjectionsKeepsObservingAFailureThatLasts(t *testing.T) {
	mem := storage.NewMemoryLedger()
	seedAllocations(t, mem, syncableAllocation("failing", 0))
	inv := &recoveringInventory{syncHook: func(domain.Allocation) error { return errors.New("netbox refused") }}
	app := New(testConfig(), mem, inv, nil)
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	app.SetClock(func() time.Time { return now })
	if err := app.syncProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if err := app.syncProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := mem.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := snap.Findings[findingID("inventory_sync_pending", "failing")]
	if f.Status != "OPEN" || !f.LastObservedAt.Equal(now) {
		t.Fatalf("the lasting failure was last observed at %v, want %v: %+v", f.LastObservedAt, now, f)
	}
}

func TestPendingReservationAllowsOnlyItsExactTaggedAWSResource(t *testing.T) {
	a := domain.Allocation{ID: "alloc_pending", CIDR: "10.0.0.0/20", Request: domain.Request{AllocationKey: "pending", Scope: "vpc", AccountID: "123456789012", Region: "eu"}}
	owned := domain.Observation{Resources: []domain.Resource{{Type: "vpc", ID: "vpc-one", CIDR: a.CIDR, AccountID: a.AccountID, Region: a.Region, Tags: map[string]string{"platform-ipam:allocation-id": a.ID, "platform-ipam:allocation-key": a.AllocationKey}}}}
	if !pendingReservationObservationSafe(a, owned, "") {
		t.Fatal("exact tagged resource should permit durable recovery")
	}
	owned.Resources[0].Tags = nil
	if pendingReservationObservationSafe(a, owned, "") {
		t.Fatal("untagged overlapping resource should retain the pending hold")
	}
}
