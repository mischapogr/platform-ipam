package storage

// PostgreSQL integration tests are deliberately opt-in. Set
// IPAM_TEST_DATABASE_URL to a dedicated loopback PostgreSQL database. Every
// test creates and drops only a freshly generated schema; it never migrates,
// truncates, or drops objects in the URL's default schema.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

type isolatedPostgres struct {
	t       *testing.T
	ctx     context.Context
	admin   *pgxpool.Pool
	config  *pgxpool.Config
	schema  string
	ledgers []*PostgresLedger
}

func requireIsolatedPostgres(t *testing.T) *isolatedPostgres {
	t.Helper()
	dsn := os.Getenv("IPAM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set IPAM_TEST_DATABASE_URL to run opt-in PostgreSQL integration tests")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("IPAM_TEST_DATABASE_URL is not a valid PostgreSQL connection string")
	}
	if !loopbackHost(config.ConnConfig.Host) {
		t.Fatal("IPAM_TEST_DATABASE_URL must target loopback PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot connect to IPAM_TEST_DATABASE_URL")
	}
	schema := "ipam_test_" + randomSchemaSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoteIdentifier(schema)); err != nil {
		admin.Close()
		t.Fatal("cannot create isolated PostgreSQL test schema")
	}
	db := &isolatedPostgres{t: t, ctx: ctx, admin: admin, config: config, schema: schema}
	t.Cleanup(db.close)
	return db
}

func (db *isolatedPostgres) openLedger() *PostgresLedger {
	db.t.Helper()
	config := db.config.Copy()
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["search_path"] = quoteIdentifier(db.schema)
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(db.ctx, config)
	if err != nil {
		db.t.Fatal("cannot open isolated PostgreSQL ledger pool")
	}
	ledger := NewPostgresLedgerWithPool(pool)
	db.ledgers = append(db.ledgers, ledger)
	return ledger
}

func (db *isolatedPostgres) close() {
	for _, ledger := range db.ledgers {
		ledger.Close()
	}
	if db.admin != nil {
		_, _ = db.admin.Exec(context.Background(), "DROP SCHEMA "+quoteIdentifier(db.schema)+" CASCADE")
		db.admin.Close()
	}
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func randomSchemaSuffix(t *testing.T) string {
	t.Helper()
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal("cannot create random isolated schema name")
	}
	return hex.EncodeToString(bytes)
}

func postgresAllocation(id, key string) domain.Allocation {
	return domain.Allocation{
		Request: domain.Request{AllocationKey: key},
		ID:      id, TenantID: "tenant-a", DomainID: "domain-a", CIDR: "10.64.0.0/20",
		PoolID: "pool-a", State: domain.Reserved, Committed: true,
		CreatedAt: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
	}
}

func TestPostgresExplicitMigrationAndReady(t *testing.T) {
	db := requireIsolatedPostgres(t)
	ledger := db.openLedger()
	if err := ledger.Ready(db.ctx); err == nil {
		t.Fatal("an untouched isolated schema unexpectedly reported ready")
	}
	if err := ledger.Migrate(db.ctx); err != nil {
		t.Fatalf("explicit migration: %v", err)
	}
	if err := ledger.Ready(db.ctx); err != nil {
		t.Fatalf("ready after explicit migration: %v", err)
	}
}

func TestPostgresReloadPreservesExactIdempotencyKey(t *testing.T) {
	db := requireIsolatedPostgres(t)
	first := db.openLedger()
	if err := first.Migrate(db.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const requestID = "sha256-idempotency-key"
	if err := first.Update(db.ctx, func(state *domain.State) error {
		allocation := postgresAllocation("alloc_reload", "orders")
		state.Allocations[allocation.ID] = allocation
		state.Requests[requestID] = domain.Idempotency{
			TenantID: allocation.TenantID, Method: "POST", Path: "POST /v1/allocations",
			Key: "exact-client-key", Hash: "full-normalized-body", AllocationID: allocation.ID,
		}
		return nil
	}); err != nil {
		t.Fatalf("store idempotency record: %v", err)
	}
	first.Close()

	reloaded := db.openLedger()
	if err := reloaded.Ready(db.ctx); err != nil {
		t.Fatalf("reloaded ledger ready: %v", err)
	}
	if err := reloaded.View(db.ctx, func(state *domain.State) error {
		request, ok := state.Requests[requestID]
		if !ok {
			return fmt.Errorf("exact idempotency map key was not reloaded")
		}
		if request.Key != "exact-client-key" || request.Hash != "full-normalized-body" || request.AllocationID != "alloc_reload" {
			return fmt.Errorf("idempotency record changed across pool reload: %#v", request)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A pending adoption carries the record an operator reviewed, and the worker
// that finishes it after a restart is by definition not the process that wrote
// it: it compares the inventory's answer with NetworkID before it commits, and
// names Operator as the actor in the audit trail. The record rides in the
// operation's jsonb payload rather than in columns of its own, so this is the
// test that the payload really carries it across a reload.
func TestPostgresReloadPreservesTheReviewedAdoptionRecord(t *testing.T) {
	db := requireIsolatedPostgres(t)
	first := db.openLedger()
	if err := first.Migrate(db.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	reviewed := domain.AdoptionRecord{
		Operator: "ops:alice", NetworkID: "4242", ResourceID: "vpc-adopted", ImportBatch: "batch-2026-09-18",
	}
	if err := first.Update(db.ctx, func(state *domain.State) error {
		allocation := postgresAllocation("alloc_adopt", "orders")
		allocation.Committed = false
		state.Allocations[allocation.ID] = allocation
		state.Operations["op_adopt"] = domain.Operation{
			ID: "op_adopt", Type: "ADOPT", Status: "PENDING", AllocationID: allocation.ID,
			TenantID: allocation.TenantID, DomainID: allocation.DomainID, Adoption: &reviewed,
			CreatedAt: allocation.CreatedAt, UpdatedAt: allocation.CreatedAt,
		}
		return nil
	}); err != nil {
		t.Fatalf("store pending adoption: %v", err)
	}
	first.Close()

	reloaded := db.openLedger()
	if err := reloaded.View(db.ctx, func(state *domain.State) error {
		operation, ok := state.Operations["op_adopt"]
		if !ok {
			return fmt.Errorf("the pending adoption was not reloaded")
		}
		if operation.Adoption == nil {
			return fmt.Errorf("the reviewed record did not survive the reload: %#v", operation)
		}
		if *operation.Adoption != reviewed {
			return fmt.Errorf("reviewed record changed across pool reload: %#v", *operation.Adoption)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A finding's resource identity is what the operator's domain view groups on
// (ADR 0011, package G3b3). The findings table carries id, tenant_id and
// domain_id as columns and everything else in a jsonb payload, so the two
// fields needed no migration -- which is a claim about the payload, and this is
// the test of it. It is opt-in like the rest of this file; the runnable proof
// that the json tags carry them is the memory ledger's round trip in
// memory_test.go, which clones state through the same encoding.
func TestPostgresReloadPreservesTheFindingResourceIdentity(t *testing.T) {
	db := requireIsolatedPostgres(t)
	first := db.openLedger()
	if err := first.Migrate(db.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	occupancy := domain.Finding{
		ID: "finding_occupancy", TenantID: "tenant-a", DomainID: "domain-a",
		Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN",
		AccountID: "123456789012", Region: "eu-central-1",
		ResourceType: "vpc", ResourceID: "vpc-0unmanaged000000",
		FirstObservedAt: time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC),
		LastObservedAt:  time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC),
	}
	// An allocation-scoped finding carries no resource identity, and must come
	// back carrying none: the fields are omitempty, so this is also the check
	// that an absent value survives as absent rather than as something else.
	aged := domain.Finding{
		ID: "finding_aged", TenantID: "tenant-a", DomainID: "domain-a",
		AllocationID: "alloc_reload", Code: "reservation_aged", Severity: "WARNING",
		Status: "OPEN", FirstObservedAt: occupancy.FirstObservedAt, LastObservedAt: occupancy.LastObservedAt,
	}
	if err := first.Update(db.ctx, func(state *domain.State) error {
		state.Findings[occupancy.ID] = occupancy
		state.Findings[aged.ID] = aged
		return nil
	}); err != nil {
		t.Fatalf("store findings: %v", err)
	}
	first.Close()

	reloaded := db.openLedger()
	if err := reloaded.View(db.ctx, func(state *domain.State) error {
		got, ok := state.Findings[occupancy.ID]
		if !ok {
			return fmt.Errorf("the occupancy finding was not reloaded")
		}
		if got != occupancy {
			return fmt.Errorf("the occupancy finding changed across pool reload: %#v", got)
		}
		got, ok = state.Findings[aged.ID]
		if !ok {
			return fmt.Errorf("the allocation-scoped finding was not reloaded")
		}
		if got.ResourceType != "" || got.ResourceID != "" {
			return fmt.Errorf("an allocation-scoped finding gained a resource identity: %#v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresUpdateRollbackDoesNotPersistState(t *testing.T) {
	db := requireIsolatedPostgres(t)
	ledger := db.openLedger()
	if err := ledger.Migrate(db.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rollback := errors.New("intentional rollback")
	if err := ledger.Update(db.ctx, func(state *domain.State) error {
		state.Allocations["alloc_rollback"] = postgresAllocation("alloc_rollback", "rollback")
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("update error = %v, want rollback sentinel", err)
	}
	if err := ledger.View(db.ctx, func(state *domain.State) error {
		if len(state.Allocations) != 0 || len(state.Requests) != 0 {
			return fmt.Errorf("rolled-back state persisted: %#v", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresIndependentLedgersDoNotLoseWritesOrDuplicateKey(t *testing.T) {
	db := requireIsolatedPostgres(t)
	seed := db.openLedger()
	if err := seed.Migrate(db.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	left, right := db.openLedger(), db.openLedger()
	runConcurrent := func(leftFn, rightFn func(*domain.State) error) (error, error) {
		start := make(chan struct{})
		var group sync.WaitGroup
		group.Add(2)
		results := make(chan error, 2)
		go func() { defer group.Done(); <-start; results <- left.Update(db.ctx, leftFn) }()
		go func() { defer group.Done(); <-start; results <- right.Update(db.ctx, rightFn) }()
		close(start)
		group.Wait()
		first, second := <-results, <-results
		return first, second
	}
	if first, second := runConcurrent(
		func(state *domain.State) error {
			state.Allocations["alloc_left"] = postgresAllocation("alloc_left", "left")
			return nil
		},
		func(state *domain.State) error {
			state.Allocations["alloc_right"] = postgresAllocation("alloc_right", "right")
			return nil
		},
	); first != nil || second != nil {
		t.Fatalf("independent writes failed: left=%v right=%v", first, second)
	}
	if err := seed.View(db.ctx, func(state *domain.State) error {
		if len(state.Allocations) != 2 {
			return fmt.Errorf("lost concurrent update: %d allocations", len(state.Allocations))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first, second := runConcurrent(
		func(state *domain.State) error {
			state.Allocations["alloc_dup_a"] = postgresAllocation("alloc_dup_a", "same-key")
			return nil
		},
		func(state *domain.State) error {
			state.Allocations["alloc_dup_b"] = postgresAllocation("alloc_dup_b", "same-key")
			return nil
		},
	)
	if (first == nil) == (second == nil) {
		t.Fatalf("same tenant/allocation key must have exactly one winner: first=%v second=%v", first, second)
	}
	if err := seed.View(db.ctx, func(state *domain.State) error {
		count := 0
		for _, allocation := range state.Allocations {
			if allocation.TenantID == "tenant-a" && allocation.AllocationKey == "same-key" {
				count++
			}
		}
		if count != 1 || len(state.Allocations) != 3 {
			return fmt.Errorf("duplicate key or lost update after concurrent writes: key=%d allocations=%d", count, len(state.Allocations))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Abandoning an adoption is the one operation that removes a ledger row (ADR
// 0012, package H2b), and it does so by removing an allocation and its
// idempotency record from the state maps inside a Ledger.Update. Everything
// that makes that safe is a property of persistState: the nine tables it
// rewrites lose the derived rows with the allocation, audit_events is not among
// them and so outlives it, and the allocations table's uniqueness on
// (tenant_id, allocation_key) is released, which is what makes the key free
// again rather than retired.
func TestPostgresAbandoningAnAllocationDropsItsRowsAndFreesItsKey(t *testing.T) {
	db := requireIsolatedPostgres(t)
	ledger := db.openLedger()
	if err := ledger.Migrate(db.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const requestID = "sha256-adopt-record"
	hold := postgresAllocation("alloc_abandoned", "orders")
	hold.Committed = false
	reviewed := domain.AdoptionRecord{Operator: "ops:bob", NetworkID: "4242", ResourceID: "vpc-adopted"}
	if err := ledger.Update(db.ctx, func(state *domain.State) error {
		state.Allocations[hold.ID] = hold
		state.Requests[requestID] = domain.Idempotency{
			TenantID: hold.TenantID, Method: "ADOPT", Path: "ADOPT /v1/allocations",
			Key: hold.AllocationKey, Hash: "reviewed-record", AllocationID: hold.ID,
		}
		state.Operations["op_abandoned"] = domain.Operation{
			ID: "op_abandoned", Type: "ADOPT", Status: "PENDING", AllocationID: hold.ID,
			TenantID: hold.TenantID, DomainID: hold.DomainID, Adoption: &reviewed,
			CreatedAt: hold.CreatedAt, UpdatedAt: hold.CreatedAt,
		}
		state.Events = append(state.Events, domain.Event{
			ID: "evt_planned", AllocationID: hold.ID, TenantID: hold.TenantID,
			Action: "ADOPT_PLANNED", At: hold.CreatedAt,
		})
		return nil
	}); err != nil {
		t.Fatalf("store the pending adoption: %v", err)
	}
	if got := db.countRows(t, "allocation_keys"); got != 1 {
		t.Fatalf("want the hold's derived allocation_keys row, got %d", got)
	}

	// What the abandon's delete transaction does, and nothing else: the
	// operation stays, terminal, carrying what was reviewed.
	if err := ledger.Update(db.ctx, func(state *domain.State) error {
		delete(state.Allocations, hold.ID)
		delete(state.Requests, requestID)
		operation := state.Operations["op_abandoned"]
		operation.Status = "FAILED"
		operation.Error = domain.Err(409, "adoption_abandoned", "Adoption abandoned by an operator; the uncommitted hold was withdrawn.")
		state.Operations[operation.ID] = operation
		state.Events = append(state.Events, domain.Event{
			ID: "evt_abandoned", AllocationID: hold.ID, TenantID: hold.TenantID,
			Actor: "ops:bob", Action: "ADOPT_ABANDONED", At: hold.CreatedAt,
		})
		return nil
	}); err != nil {
		t.Fatalf("withdraw the hold: %v", err)
	}

	reloaded := db.openLedger()
	if err := reloaded.View(db.ctx, func(state *domain.State) error {
		if _, held := state.Allocations[hold.ID]; held {
			return fmt.Errorf("the allocation survived the reload")
		}
		if _, kept := state.Requests[requestID]; kept {
			return fmt.Errorf("the ADOPT idempotency record survived the reload")
		}
		operation, ok := state.Operations["op_abandoned"]
		if !ok || operation.Status != "FAILED" || operation.Error == nil || operation.Error.Code != "adoption_abandoned" {
			return fmt.Errorf("the terminal operation did not survive as written: %#v", operation)
		}
		if operation.Adoption == nil || *operation.Adoption != reviewed {
			return fmt.Errorf("the operation lost the reviewed record: %#v", operation.Adoption)
		}
		seen := map[string]bool{}
		for _, event := range state.Events {
			seen[event.ID] = true
		}
		if !seen["evt_planned"] || !seen["evt_abandoned"] {
			return fmt.Errorf("audit events did not outlive the allocation: %#v", state.Events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The derived rows go with the allocation, because persistState rewrites
	// them from the same map.
	for _, table := range []string{"allocation_keys", "holds"} {
		if got := db.countRows(t, table); got != 0 {
			t.Fatalf("%s still has %d row(s) for an allocation that is gone", table, got)
		}
	}

	// And the key is usable again: a new allocation under the same tenant and
	// allocation key no longer collides with the unique index.
	again := postgresAllocation("alloc_readopted", "orders")
	if err := reloaded.Update(db.ctx, func(state *domain.State) error {
		state.Allocations[again.ID] = again
		return nil
	}); err != nil {
		t.Fatalf("the allocation key was not released: %v", err)
	}
}

// countRows reads one of the tables persistState rewrites, so that a claim
// about a derived row is checked against the database rather than against the
// state the ledger reloaded.
func (db *isolatedPostgres) countRows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := db.admin.QueryRow(db.ctx, "SELECT count(*) FROM "+quoteIdentifier(db.schema)+"."+quoteIdentifier(table)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Cancelling a stuck reservation is the first tenant-facing operation that
// removes a ledger row (ADR 0013, package H8b), and it differs from the
// abandon's delete in three ways this test is about: the idempotency record is
// keyed on the consumer's OWN Idempotency-Key rather than on the allocation
// key, so the delete finds it by the allocation it names; the finding it
// resolves is reservation_stuck, which must survive the deleted row as a
// RESOLVED row rather than disappearing with it; and the fence empties
// operation_barriers, which persistState regenerates from the pending
// operations alone -- that is what unfences the overlap domain.
func TestPostgresCancellingAReservationDropsItsRowsAndFreesItsKey(t *testing.T) {
	db := requireIsolatedPostgres(t)
	ledger := db.openLedger()
	if err := ledger.Migrate(db.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const requestID = "sha256-consumer-idempotency-key"
	const stuckFinding = "finding_reservation_stuck"
	hold := postgresAllocation("alloc_cancelled", "orders")
	hold.Committed = false
	if err := ledger.Update(db.ctx, func(state *domain.State) error {
		state.Allocations[hold.ID] = hold
		state.Requests[requestID] = domain.Idempotency{
			TenantID: hold.TenantID, Method: "POST", Path: "POST /v1/allocations",
			Key: "orders-idempotency-key", Hash: "normalized-body", AllocationID: hold.ID,
			OperationID: "op_cancelled",
		}
		state.Operations["op_cancelled"] = domain.Operation{
			ID: "op_cancelled", Type: "RESERVE", Status: "PENDING", AllocationID: hold.ID,
			TenantID: hold.TenantID, DomainID: hold.DomainID,
			CreatedAt: hold.CreatedAt, UpdatedAt: hold.CreatedAt,
		}
		state.Findings[stuckFinding] = domain.Finding{
			ID: stuckFinding, TenantID: hold.TenantID, DomainID: hold.DomainID, AllocationID: hold.ID,
			Code: "reservation_stuck", Severity: "CRITICAL", Status: "OPEN",
			FirstObservedAt: hold.CreatedAt, LastObservedAt: hold.CreatedAt,
		}
		state.Events = append(state.Events, domain.Event{
			ID: "evt_reserve_planned", AllocationID: hold.ID, TenantID: hold.TenantID,
			Actor: "t:carla", Action: "RESERVE_PLANNED", At: hold.CreatedAt,
		})
		return nil
	}); err != nil {
		t.Fatalf("store the pending reservation: %v", err)
	}
	if got := db.countRows(t, "allocation_keys"); got != 1 {
		t.Fatalf("want the hold's derived allocation_keys row, got %d", got)
	}
	if got := db.countRows(t, "operation_barriers"); got != 1 {
		t.Fatalf("want the pending operation's barrier row, got %d", got)
	}

	// What the cancel's fence transaction does, and nothing else.
	if err := ledger.Update(db.ctx, func(state *domain.State) error {
		operation := state.Operations["op_cancelled"]
		operation.Status = "FAILED"
		operation.Error = domain.Err(409, "reservation_cancelled", "Reservation cancelled by the tenant that requested it; the uncommitted hold was withdrawn.")
		state.Operations[operation.ID] = operation
		state.Events = append(state.Events, domain.Event{
			ID: "evt_reserve_cancelled", AllocationID: hold.ID, TenantID: hold.TenantID,
			Actor: "t:carla", Action: "RESERVE_CANCELLED", At: hold.CreatedAt,
		})
		return nil
	}); err != nil {
		t.Fatalf("fence the operation: %v", err)
	}
	// The fence alone unfences the overlap domain, because the barrier table is
	// regenerated from the pending operations.
	if got := db.countRows(t, "operation_barriers"); got != 0 {
		t.Fatalf("the fenced operation still holds %d barrier row(s)", got)
	}

	// And what the delete transaction does.
	if err := ledger.Update(db.ctx, func(state *domain.State) error {
		delete(state.Allocations, hold.ID)
		for id, request := range state.Requests {
			if request.AllocationID == hold.ID {
				delete(state.Requests, id)
			}
		}
		finding := state.Findings[stuckFinding]
		finding.Status = "RESOLVED"
		state.Findings[stuckFinding] = finding
		return nil
	}); err != nil {
		t.Fatalf("withdraw the hold: %v", err)
	}

	reloaded := db.openLedger()
	if err := reloaded.View(db.ctx, func(state *domain.State) error {
		if _, held := state.Allocations[hold.ID]; held {
			return fmt.Errorf("the allocation survived the reload")
		}
		if _, kept := state.Requests[requestID]; kept {
			return fmt.Errorf("the consumer's POST idempotency record survived the reload")
		}
		operation, ok := state.Operations["op_cancelled"]
		if !ok || operation.Status != "FAILED" || operation.Error == nil || operation.Error.Code != "reservation_cancelled" {
			return fmt.Errorf("the terminal operation did not survive as written: %#v", operation)
		}
		finding, ok := state.Findings[stuckFinding]
		if !ok || finding.Status != "RESOLVED" || finding.AllocationID != hold.ID {
			return fmt.Errorf("the resolved finding did not outlive the allocation: %#v", finding)
		}
		seen := map[string]bool{}
		for _, event := range state.Events {
			seen[event.ID] = true
		}
		if !seen["evt_reserve_planned"] || !seen["evt_reserve_cancelled"] {
			return fmt.Errorf("audit events did not outlive the allocation: %#v", state.Events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"allocation_keys", "holds", "operation_barriers"} {
		if got := db.countRows(t, table); got != 0 {
			t.Fatalf("%s still has %d row(s) for an allocation that is gone", table, got)
		}
	}

	// And the key is usable again: a fresh reservation under the same tenant
	// and allocation key no longer collides with the unique index.
	again := postgresAllocation("alloc_reserved_again", "orders")
	if err := reloaded.Update(db.ctx, func(state *domain.State) error {
		state.Allocations[again.ID] = again
		return nil
	}); err != nil {
		t.Fatalf("the allocation key was not released: %v", err)
	}
}

func TestPostgresAuditEventsRemainAppendOnly(t *testing.T) {
	db := requireIsolatedPostgres(t)
	ledger := db.openLedger()
	if err := ledger.Migrate(db.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	first := domain.Event{ID: "evt_first", AllocationID: "alloc_audit", TenantID: "tenant-a", Action: "RESERVE", At: time.Now().UTC()}
	second := domain.Event{ID: "evt_second", AllocationID: "alloc_audit", TenantID: "tenant-a", Action: "RELEASE_REQUESTED", At: time.Now().UTC()}
	if err := ledger.Update(db.ctx, func(state *domain.State) error {
		state.Events = append(state.Events, first)
		return nil
	}); err != nil {
		t.Fatalf("append first audit event: %v", err)
	}
	if err := ledger.Update(db.ctx, func(state *domain.State) error {
		// A caller cannot erase persisted audit history by presenting a newer
		// state that only contains its own event.
		state.Events = []domain.Event{second}
		return nil
	}); err != nil {
		t.Fatalf("append second audit event: %v", err)
	}
	if err := ledger.View(db.ctx, func(state *domain.State) error {
		seen := map[string]bool{}
		for _, event := range state.Events {
			seen[event.ID] = true
		}
		if !seen[first.ID] || !seen[second.ID] || len(seen) != 2 {
			return fmt.Errorf("audit history is not append-only: %#v", state.Events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
