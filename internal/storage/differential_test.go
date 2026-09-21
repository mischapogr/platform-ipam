package storage

// differential_test.go is ADR 0017's first evidence item (M4c): a
// deterministic, seeded differential harness comparing PostgresLedger against
// MemoryLedger over generated sequences of domain.Ledger.Update/View
// transactions, shaped exactly like the closures internal/service actually
// builds -- never a shape the service does not produce. It changes no
// production code and is gated on IPAM_TEST_DATABASE_URL exactly like the
// rest of this package's PostgreSQL integration tests (requireIsolatedPostgres
// in postgres_test.go), so it skips cleanly without a database.
//
// What it generates. A seeded generator (genModel) plays a fixed-length
// sequence of operations drawn from the shapes ADR 0017 names: a reservation
// or an adoption planned (reserve's planning Update, service.go:271-376) and
// committed (its commit closure, service.go:405-459); a binding requested
// (RequestBinding's Update, service.go:689) and finished (finishBinding,
// service.go:719-755); a release (Release, service.go:759-836); an adoption
// fenced and deleted (abandon.go's fence and delete transactions,
// abandon.go:170, abandon.go:232); a reservation fenced and deleted (cancel.go's
// fence and delete transactions, cancel.go:148, cancel.go:214); a finding
// raised and resolved the way the worker does it (workerFinding/resolveFinding,
// worker.go:672-694) and the way a projection sync result does it
// (applyProjectionResults, worker.go:614-630); an observation recorded past the
// sixty-four-row retention (recordObservation, service.go:1811-1828,
// reimplemented here as applyRecordObservation because it is unexported); and
// an intentional-rollback transaction, to give the "error each Update
// returned" comparison axis real content and to exercise property 4 of ADR
// 0017's ten ("a closure that returns an error changes nothing").
//
// What it compares, after every step that both stores accepted or both stores
// refused identically:
//   - the error each Update returned (errors.Is against the same sentinel for
//     the intentional-rollback step; nil-ness agreement for every other step,
//     since every other generated closure is built from a model that only ever
//     picks a precondition the real service closures would also accept, so it
//     never manufactures a business refusal to compare);
//   - the reloaded *domain.State from View, normalised (see below) and
//     compared with reflect.DeepEqual;
//   - all nine tables persistState writes -- allocations, allocation_keys,
//     operations, idempotency_requests, observations, findings, coverage,
//     holds, operation_barriers -- row by row, decoded from the schema
//     directly with admin SQL rather than through the store, because a reload
//     is exactly the thing that cannot see allocation_keys, holds or
//     operation_barriers (loadState reads seven tables, persistState writes
//     nine): those three are write-only and a defect in their derivation is
//     visible ONLY here.
//
// Normalisation and exclusion, each named because ADR 0017 requires it stated
// rather than papered over:
//
//  1. Id minting. Every real closure mints ids with domain.NewID
//     (domain/types.go:341-347), which reads crypto/rand and would make two
//     independent invocations of the "same" closure -- one against the memory
//     store, one against Postgres -- mint two different ids and diverge
//     trivially. The generator mints every id once, outside and before both
//     Update calls (idMint below), and bakes the fixed id into the one
//     closure value that is then handed to both ledgers unchanged. This is
//     the ADR's own prescription: "make the generator mint ids deterministically
//     outside the closure."
//
//  2. persistState's empty-event id assignment (postgres.go:403-405 assigns
//     domain.NewID("evt") to any audit event whose ID is empty; the memory
//     store does not). This is excluded by construction rather than
//     special-cased: every event-append site in internal/service always sets
//     ID via domain.NewID before appending (service.go:366, service.go:435,
//     service.go:752, abandon.go, cancel.go, worker.go's workerEvent), so no
//     shape the service produces ever appends an empty-id event, and the
//     generator does not either. The divergence exists in the checkout and is
//     recorded, not exercised.
//
//  3. View's lock. The memory store's View takes a read lock and admits
//     concurrent readers (memory.go:25); Postgres's View takes the same
//     exclusive advisory lock as Update (postgres.go:108-133). This harness
//     drives both stores from a single goroutine, one transaction at a time,
//     and never opens two overlapping Views -- so the lock-strength
//     difference cannot manifest here. It is a property of the two stores to
//     record, not a bug this harness is built to catch, exactly as ADR 0017
//     says.
//
//  4. Audit event order. loadState's query is `ORDER BY id`
//     (postgres.go:297), an arbitrary lexical order over ids that carry no
//     temporal meaning; the memory store's State.Events is the insertion-order
//     slice a JSON clone preserves (memory.go:59-68). Nothing in
//     internal/service reads audit-event order -- Findings, List, and every
//     lookup this repository has index by allocation id or finding id, never
//     by position in Events -- so comparing Events as an order-sensitive
//     sequence would fail on this ordering difference alone, on every sequence
//     with more than one event, independent of any defect in persistState.
//     This harness's state-normalisation step (normalizeState) therefore
//     sorts both sides' Events by ID before comparing content: this still
//     catches a dropped, duplicated or content-altered event (mutant 5 below),
//     it just stops mistaking a legitimate ordering difference for one. This
//     is a normalisation the harness itself discovered running against the
//     unmodified store, not one of ADR 0017's three; it is documented here for
//     the same reason those are.
//
// The mutants that prove this harness can fail are not shipped in this file
// -- carrying broken production code permanently defeats their purpose. They
// were applied one at a time to a scratch copy of postgres.go's persistState,
// each run against this exact test, and each one's failure output is pasted
// in the M4c package report. The six were: (1) drop operation_barriers from
// persistState's DELETE list, so a fenced operation's barrier row outlives it
// -- reachable only through the operation_barriers table, since loadState
// never reads it back; (2) skip the allocation_keys insert entirely --
// likewise reachable only through the table; (3) write holds.committed as an
// unconditional true instead of `v.Committed && v.State != domain.Released`
// -- likewise table-only; (4) drop the `if v.Status != "PENDING" { continue }`
// guard before the operation_barriers insert, so a non-pending operation gets
// a barrier row -- likewise table-only; (5) skip inserting the last audit
// event of the batch (an off-by-one in the events loop), visible both in the
// reloaded state's Events and in the audit_events table; (6) swap the values
// written to the idempotency_requests table's request_key and request_hash
// columns, leaving the jsonb payload correct -- reachable only through the
// table, since loadState decodes idempotency_requests from its payload column
// alone and never reads request_key back.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// ---------------------------------------------------------------------------
// Deterministic id minting and clock. See exclusion 1 above.
// ---------------------------------------------------------------------------

type idMint struct{ n int }

func (m *idMint) next(prefix string) string {
	m.n++
	return fmt.Sprintf("%s_%08d", prefix, m.n)
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) tick() time.Time {
	c.t = c.t.Add(time.Second)
	return c.t
}

// findingKey mirrors internal/service/service.go's findingID exactly (sha256
// of code+NUL+allocationID, prefixed "finding_"). Reimplemented here because
// it is unexported: this is an id-derivation SHAPE, not a divergence between
// the two stores -- both ledgers are handed the same precomputed key by the
// generator, exactly as every other id in this harness is.
func findingKey(code, allocationID string) string {
	h := sha256.Sum256([]byte(code + "\x00" + allocationID))
	return "finding_" + hex.EncodeToString(h[:])
}

const (
	genReserveOperation = "RESERVE"
	genAdoptOperation   = "ADOPT"
	genBindOperation    = "VERIFY_BINDING"

	genReservationStuckCode = "reservation_stuck"
	genAdoptionStuckCode    = "adoption_stuck"
	genProjectionCode       = "inventory_sync_pending"

	genAdoptionAbandonedCode = "adoption_abandoned"
	genReservationCancelled  = "reservation_cancelled"
)

// ---------------------------------------------------------------------------
// Generator model: enough of the service's own bookkeeping to pick, for each
// operation kind, a target that satisfies the same preconditions the real
// service closures enforce (pendingDomain's one-pending-operation-per-domain
// fence, pendingForAllocation's one-pending-operation-per-allocation fence,
// commit/fence/delete ordering), so every generated closure is a shape the
// service could really have produced.
// ---------------------------------------------------------------------------

type genAllocation struct {
	id, tenantID, domainID, allocationKey, scope, poolID, cidr string
	accountID, region                                          string
	committed                                                  bool
	state                                                      string
	revision                                                   int64
	hasBinding                                                 bool
	pendingOpID, pendingOpType                                 string
	fencedAbandon, fencedCancel                                bool
	deleted                                                    bool
	findingOpen                                                map[string]bool
}

type genModel struct {
	rng           *rand.Rand
	ids           idMint
	clock         fakeClock
	allocations   map[string]*genAllocation
	domainPending map[string]bool
	order         []string
	cidrN         int
}

func newGenModel(seed int64) *genModel {
	return &genModel{
		rng:           rand.New(rand.NewSource(seed)),
		clock:         fakeClock{t: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)},
		allocations:   map[string]*genAllocation{},
		domainPending: map[string]bool{},
	}
}

func (m *genModel) nextCIDR() string {
	m.cidrN++
	return fmt.Sprintf("10.%d.%d.0/24", (m.cidrN/256)%256, m.cidrN%256)
}

func (m *genModel) pickTenant() string {
	if m.rng.Intn(2) == 0 {
		return "tenant-a"
	}
	return "tenant-b"
}

func (m *genModel) pickDomain() string {
	if m.rng.Intn(2) == 0 {
		return "domain-a"
	}
	return "domain-b"
}

func (m *genModel) candidates(pred func(*genAllocation) bool) []*genAllocation {
	var out []*genAllocation
	for _, id := range m.order {
		a := m.allocations[id]
		if a != nil && !a.deleted && pred(a) {
			out = append(out, a)
		}
	}
	return out
}

func (m *genModel) pick(pred func(*genAllocation) bool) *genAllocation {
	cs := m.candidates(pred)
	if len(cs) == 0 {
		return nil
	}
	return cs[m.rng.Intn(len(cs))]
}

// diffOp is one generated ledger transaction. apply is pure and mints no ids
// of its own -- every id a real closure would mint with domain.NewID is
// pre-baked by the generator (exclusion 1 in the package doc comment above).
type diffOp struct {
	name  string
	apply func(*domain.State) error
}

var errIntentionalRollback = errors.New("differential harness: intentional rollback")

// ---------------------------------------------------------------------------
// Operation generators. Each mirrors one real internal/service closure body.
// ---------------------------------------------------------------------------

// genReservePlan mirrors reserve's planning Update (service.go:342-372): a
// new allocation and its RESERVE or ADOPT operation, a *_PLANNED event, and
// the idempotency record the plan writes.
func (m *genModel) genReservePlan(kind string) *diffOp {
	dom := m.pickDomain()
	if m.domainPending[dom] {
		return nil
	}
	planned := "RESERVE_PLANNED"
	if kind == genAdoptOperation {
		planned = "ADOPT_PLANNED"
	}
	tenant := m.pickTenant()
	id := m.ids.next("alloc")
	opID := m.ids.next("op")
	evtID := m.ids.next("evt")
	reqID := m.ids.next("req")
	now := m.clock.tick()
	cidr := m.nextCIDR()
	key := m.ids.next("key")
	accountID, region := "123456789012", "eu-central-1"

	a := domain.Allocation{
		Request: domain.Request{
			AllocationKey: key, Scope: "vpc", Environment: "prod", Region: region,
			AccountID: accountID, PrefixLength: 24, Description: "generated", Labels: map[string]string{"gen": "m4c"},
		},
		ID: id, TenantID: tenant, DomainID: dom, RequestHash: "hash-" + id, CIDR: cidr, PoolID: "pool-a",
		State: domain.Reserved, Revision: 1, PolicyVersion: "v1", InventorySync: "PENDING",
		CreatedAt: now, UpdatedAt: now, Committed: false, ReleaseBlockers: []string{},
	}
	op := domain.Operation{ID: opID, Type: kind, Status: "PENDING", AllocationID: id, TenantID: tenant, DomainID: dom, CreatedAt: now, UpdatedAt: now}
	apply := func(st *domain.State) error {
		st.Allocations[a.ID] = a
		st.Operations[op.ID] = op
		st.Events = append(st.Events, domain.Event{ID: evtID, AllocationID: a.ID, TenantID: tenant, Actor: "t:" + tenant, Action: planned, Reason: "generated", At: now, Revision: a.Revision})
		st.Requests[reqID] = domain.Idempotency{TenantID: tenant, Method: "POST", Path: "POST /v1/allocations", Key: key, Hash: "h-" + reqID, AllocationID: a.ID, OperationID: op.ID}
		return nil
	}

	m.allocations[id] = &genAllocation{
		id: id, tenantID: tenant, domainID: dom, allocationKey: key, scope: "vpc", poolID: "pool-a", cidr: cidr,
		accountID: accountID, region: region, committed: false, state: domain.Reserved, revision: 1,
		pendingOpID: opID, pendingOpType: kind, findingOpen: map[string]bool{},
	}
	m.order = append(m.order, id)
	m.domainPending[dom] = true
	return &diffOp{name: "reservePlan/" + kind, apply: apply}
}

// genReserveCommit mirrors reserve's commit closure (service.go:421-455).
func (m *genModel) genReserveCommit() *diffOp {
	a := m.pick(func(a *genAllocation) bool {
		return !a.committed && (a.pendingOpType == genReserveOperation || a.pendingOpType == genAdoptOperation)
	})
	if a == nil {
		return nil
	}
	kind := a.pendingOpType
	committedAction := "RESERVE_COMMITTED"
	stuck := genReservationStuckCode
	if kind == genAdoptOperation {
		committedAction, stuck = "ADOPT_COMMITTED", genAdoptionStuckCode
	}
	now := m.clock.tick()
	evtID := m.ids.next("evt")
	invID := "inv_" + a.id
	opID := a.pendingOpID
	id, tenant, revision := a.id, a.tenantID, a.revision
	fKey := findingKey(stuck, id)

	apply := func(st *domain.State) error {
		al := st.Allocations[id]
		al.Committed = true
		al.InventoryID = invID
		al.InventorySync = "CURRENT"
		al.UpdatedAt = now
		st.Allocations[id] = al
		st.Events = append(st.Events, domain.Event{ID: evtID, AllocationID: id, TenantID: tenant, Actor: "commit", Action: committedAction, Reason: "generated commit", At: now, Revision: revision})
		o := st.Operations[opID]
		o.Status = "SUCCEEDED"
		o.Result = map[string]string{"allocation_id": id}
		o.UpdatedAt = now
		st.Operations[opID] = o
		if f, ok := st.Findings[fKey]; ok {
			f.Status = "RESOLVED"
			st.Findings[fKey] = f
		}
		return nil
	}

	a.committed = true
	a.pendingOpID, a.pendingOpType = "", ""
	a.findingOpen[stuck] = false
	m.domainPending[a.domainID] = false
	return &diffOp{name: "reserveCommit/" + kind, apply: apply}
}

// genBindRequest mirrors RequestBinding's Update (service.go:689).
func (m *genModel) genBindRequest() *diffOp {
	a := m.pick(func(a *genAllocation) bool {
		return a.committed && a.state == domain.Reserved && !a.hasBinding && a.pendingOpType == "" && !m.domainPending[a.domainID]
	})
	if a == nil {
		return nil
	}
	now := m.clock.tick()
	opID := m.ids.next("op")
	reqID := m.ids.next("req")
	id, tenant, dom := a.id, a.tenantID, a.domainID
	candidate := domain.Binding{Provider: "aws", ResourceType: a.scope, ResourceID: "res_" + id, AccountID: a.accountID, Region: a.region}

	apply := func(st *domain.State) error {
		o := domain.Operation{ID: opID, Type: genBindOperation, Status: "PENDING", AllocationID: id, TenantID: tenant, DomainID: dom, Candidate: &candidate, CreatedAt: now, UpdatedAt: now}
		st.Operations[o.ID] = o
		st.Requests[reqID] = domain.Idempotency{TenantID: tenant, Method: "PUT", Path: "/v1/allocations/" + id + "/binding", Key: reqID, Hash: "h-" + reqID, AllocationID: id, OperationID: o.ID}
		return nil
	}

	a.pendingOpID, a.pendingOpType = opID, genBindOperation
	m.domainPending[dom] = true
	return &diffOp{name: "bindRequest", apply: apply}
}

// genFinishBinding mirrors finishBinding (service.go:719-755).
func (m *genModel) genFinishBinding() *diffOp {
	a := m.pick(func(a *genAllocation) bool { return a.pendingOpType == genBindOperation })
	if a == nil {
		return nil
	}
	now := m.clock.tick()
	evtID := m.ids.next("evt")
	id, tenant, opID, revision := a.id, a.tenantID, a.pendingOpID, a.revision+1
	binding := domain.Binding{Provider: "aws", ResourceType: a.scope, ResourceID: "res_" + id, AccountID: a.accountID, Region: a.region, VerifiedAt: &now}

	apply := func(st *domain.State) error {
		x := st.Allocations[id]
		x.Binding = &binding
		x.State = domain.Active
		x.InventorySync = "PENDING"
		x.Revision = revision
		x.UpdatedAt = now
		st.Allocations[id] = x
		o := st.Operations[opID]
		o.Status = "SUCCEEDED"
		o.Result = map[string]string{"allocation_id": id}
		o.UpdatedAt = now
		st.Operations[opID] = o
		st.Events = append(st.Events, domain.Event{ID: evtID, AllocationID: id, TenantID: tenant, Action: "BIND", Reason: "generated bind", At: now, Revision: revision})
		return nil
	}

	a.hasBinding = true
	a.state = domain.Active
	a.revision = revision
	a.pendingOpID, a.pendingOpType = "", ""
	m.domainPending[a.domainID] = false
	return &diffOp{name: "finishBinding", apply: apply}
}

// genReleaseRequest mirrors Release (service.go:759-805): it also fails a
// pending VERIFY_BINDING operation, exactly as Release's own loop does.
func (m *genModel) genReleaseRequest() *diffOp {
	a := m.pick(func(a *genAllocation) bool {
		return a.committed && (a.state == domain.Reserved || a.state == domain.Active)
	})
	if a == nil {
		return nil
	}
	now := m.clock.tick()
	evtID := m.ids.next("evt")
	id, tenant, revision := a.id, a.tenantID, a.revision+1
	failBindOp := a.pendingOpType == genBindOperation
	bindOpID := a.pendingOpID
	until := now.Add(7 * 24 * time.Hour)

	apply := func(st *domain.State) error {
		if failBindOp {
			o := st.Operations[bindOpID]
			o.Status = "FAILED"
			o.Error = domain.Err(409, "allocation_key_retired", "Allocation was released during binding verification.")
			o.UpdatedAt = now
			st.Operations[bindOpID] = o
		}
		x := st.Allocations[id]
		x.State = domain.Quarantined
		x.InventorySync = "PENDING"
		x.ReleaseRequestedAt = &now
		x.QuarantineUntil = &until
		x.UpdatedAt = now
		x.Revision = revision
		x.ReleaseBlockers = []string{"quarantine_not_elapsed"}
		st.Allocations[id] = x
		st.Events = append(st.Events, domain.Event{ID: evtID, AllocationID: id, TenantID: tenant, Action: "RELEASE_REQUESTED", Reason: "generated release", At: now, Revision: revision})
		return nil
	}

	a.state = domain.Quarantined
	a.revision = revision
	if failBindOp {
		a.pendingOpID, a.pendingOpType = "", ""
		m.domainPending[a.domainID] = false
	}
	return &diffOp{name: "releaseRequest", apply: apply}
}

// genAbandonFence mirrors abandon.go's fence transaction (abandon.go:170).
func (m *genModel) genAbandonFence() *diffOp {
	a := m.pick(func(a *genAllocation) bool {
		return !a.committed && a.pendingOpType == genAdoptOperation && !a.fencedAbandon
	})
	if a == nil {
		return nil
	}
	now := m.clock.tick()
	evtID := m.ids.next("evt")
	id, tenant, opID, revision := a.id, a.tenantID, a.pendingOpID, a.revision

	apply := func(st *domain.State) error {
		o := st.Operations[opID]
		o.Status = "FAILED"
		o.Error = domain.Err(409, genAdoptionAbandonedCode, "Adoption abandoned by an operator; the uncommitted hold was withdrawn.")
		o.UpdatedAt = now
		st.Operations[opID] = o
		st.Events = append(st.Events, domain.Event{ID: evtID, AllocationID: id, TenantID: tenant, Actor: "ops:generated", Action: "ADOPT_ABANDONED", Reason: "generated abandon", At: now, Revision: revision})
		return nil
	}

	a.fencedAbandon = true
	m.domainPending[a.domainID] = false
	return &diffOp{name: "abandonFence", apply: apply}
}

// genAbandonDelete mirrors abandon.go's delete transaction (abandon.go:232).
func (m *genModel) genAbandonDelete() *diffOp {
	a := m.pick(func(a *genAllocation) bool { return a.fencedAbandon })
	if a == nil {
		return nil
	}
	id := a.id
	fKey := findingKey(genAdoptionStuckCode, id)

	apply := func(st *domain.State) error {
		delete(st.Allocations, id)
		for reqID, request := range st.Requests {
			if request.AllocationID == id {
				delete(st.Requests, reqID)
			}
		}
		if f, ok := st.Findings[fKey]; ok {
			f.Status = "RESOLVED"
			st.Findings[fKey] = f
		}
		return nil
	}

	a.deleted = true
	return &diffOp{name: "abandonDelete", apply: apply}
}

// genCancelFence mirrors cancel.go's fence transaction (cancel.go:148).
func (m *genModel) genCancelFence() *diffOp {
	a := m.pick(func(a *genAllocation) bool {
		return !a.committed && a.pendingOpType == genReserveOperation && !a.fencedCancel
	})
	if a == nil {
		return nil
	}
	now := m.clock.tick()
	evtID := m.ids.next("evt")
	id, tenant, opID, revision := a.id, a.tenantID, a.pendingOpID, a.revision

	apply := func(st *domain.State) error {
		o := st.Operations[opID]
		o.Status = "FAILED"
		o.Error = domain.Err(409, genReservationCancelled, "Reservation cancelled by the tenant that requested it; the uncommitted hold was withdrawn.")
		o.UpdatedAt = now
		st.Operations[opID] = o
		st.Events = append(st.Events, domain.Event{ID: evtID, AllocationID: id, TenantID: tenant, Actor: "t:generated", Action: "RESERVE_CANCELLED", Reason: "generated cancel", At: now, Revision: revision})
		return nil
	}

	a.fencedCancel = true
	m.domainPending[a.domainID] = false
	return &diffOp{name: "cancelFence", apply: apply}
}

// genCancelDelete mirrors cancel.go's delete transaction (cancel.go:214).
func (m *genModel) genCancelDelete() *diffOp {
	a := m.pick(func(a *genAllocation) bool { return a.fencedCancel })
	if a == nil {
		return nil
	}
	id := a.id
	fKey := findingKey(genReservationStuckCode, id)

	apply := func(st *domain.State) error {
		delete(st.Allocations, id)
		for reqID, request := range st.Requests {
			if request.AllocationID == id {
				delete(st.Requests, reqID)
			}
		}
		if f, ok := st.Findings[fKey]; ok {
			f.Status = "RESOLVED"
			st.Findings[fKey] = f
		}
		return nil
	}

	a.deleted = true
	return &diffOp{name: "cancelDelete", apply: apply}
}

// genWorkerRaiseFinding mirrors workerFinding (worker.go:672-681), used both
// by flagStuckHold (CRITICAL, the two stuck codes) and reconcileAllocations
// (WARNING, inventory_sync_pending).
func (m *genModel) genWorkerRaiseFinding() *diffOp {
	a := m.pick(func(a *genAllocation) bool { return true })
	if a == nil {
		return nil
	}
	code, severity := genProjectionCode, "WARNING"
	if m.rng.Intn(2) == 0 {
		if a.pendingOpType == genAdoptOperation {
			code, severity = genAdoptionStuckCode, "CRITICAL"
		} else {
			code, severity = genReservationStuckCode, "CRITICAL"
		}
	}
	now := m.clock.tick()
	id, tenant, dom, accountID, region := a.id, a.tenantID, a.domainID, a.accountID, a.region
	fKey := findingKey(code, id)

	apply := func(st *domain.State) error {
		f := st.Findings[fKey]
		if f.ID == "" {
			f = domain.Finding{ID: fKey, TenantID: tenant, DomainID: dom, AllocationID: id, Code: code, Severity: severity, AccountID: accountID, Region: region, FirstObservedAt: now}
		}
		f.Status = "OPEN"
		f.LastObservedAt = now
		st.Findings[fKey] = f
		return nil
	}

	a.findingOpen[code] = true
	return &diffOp{name: "workerRaiseFinding/" + code, apply: apply}
}

// genWorkerResolveFinding mirrors resolveFinding (worker.go:686-693).
func (m *genModel) genWorkerResolveFinding() *diffOp {
	var target *genAllocation
	var code string
	for _, a := range m.candidates(func(a *genAllocation) bool { return true }) {
		for c, open := range a.findingOpen {
			if open {
				target, code = a, c
				break
			}
		}
		if target != nil {
			break
		}
	}
	if target == nil {
		return nil
	}
	fKey := findingKey(code, target.id)
	apply := func(st *domain.State) error {
		if f, ok := st.Findings[fKey]; ok {
			f.Status = "RESOLVED"
			st.Findings[fKey] = f
		}
		return nil
	}
	target.findingOpen[code] = false
	return &diffOp{name: "workerResolveFinding/" + code, apply: apply}
}

// genApplyProjectionSuccess and genApplyProjectionFailure mirror the two
// branches of applyProjectionResults' per-result loop (worker.go:614-630).
func (m *genModel) genApplyProjectionSuccess() *diffOp {
	a := m.pick(func(a *genAllocation) bool { return a.committed })
	if a == nil {
		return nil
	}
	id, revision, state := a.id, a.revision, a.state
	fKey := findingKey(genProjectionCode, id)
	now := m.clock.tick()
	apply := func(st *domain.State) error {
		x, ok := st.Allocations[id]
		if !ok || x.Revision != revision || x.State != state {
			return nil
		}
		x.InventorySync = "CURRENT"
		x.UpdatedAt = now
		st.Allocations[id] = x
		if f, ok := st.Findings[fKey]; ok {
			f.Status = "RESOLVED"
			st.Findings[fKey] = f
		}
		return nil
	}
	a.findingOpen[genProjectionCode] = false
	return &diffOp{name: "applyProjectionSuccess", apply: apply}
}

func (m *genModel) genApplyProjectionFailure() *diffOp {
	a := m.pick(func(a *genAllocation) bool { return a.committed })
	if a == nil {
		return nil
	}
	id, tenant, dom, accountID, region, revision, state := a.id, a.tenantID, a.domainID, a.accountID, a.region, a.revision, a.state
	fKey := findingKey(genProjectionCode, id)
	now := m.clock.tick()
	apply := func(st *domain.State) error {
		x, ok := st.Allocations[id]
		if !ok || x.Revision != revision || x.State != state {
			return nil
		}
		x.InventorySync = "PENDING"
		x.UpdatedAt = now
		st.Allocations[id] = x
		f := st.Findings[fKey]
		if f.ID == "" {
			f = domain.Finding{ID: fKey, TenantID: tenant, DomainID: dom, AllocationID: id, Code: genProjectionCode, Severity: "WARNING", AccountID: accountID, Region: region, FirstObservedAt: now}
		}
		f.Status = "OPEN"
		f.LastObservedAt = now
		st.Findings[fKey] = f
		return nil
	}
	a.findingOpen[genProjectionCode] = true
	return &diffOp{name: "applyProjectionFailure", apply: apply}
}

// applyRecordObservation mirrors internal/service/service.go's
// recordObservation (service.go:1811-1828) exactly, reimplemented here
// because it is unexported: replace-if-same-FinishedAt, drop-and-restart on
// an incomplete or stale-generation observation, and the sixty-four-row
// retention. This is the function the generator's genRecordObservation calls,
// so that a sequence really exercises the retention cap the way a real
// reconciliation pass would.
func applyRecordObservation(st *domain.State, o domain.Observation) {
	xs := st.Observations[o.DomainID]
	if len(xs) > 0 && o.FinishedAt.Before(xs[len(xs)-1].FinishedAt) {
		return
	}
	if !o.Complete || st.Coverage[o.DomainID] != o.Generation {
		xs = nil
	}
	if len(xs) > 0 && o.FinishedAt.Equal(xs[len(xs)-1].FinishedAt) {
		xs[len(xs)-1] = o
	} else {
		xs = append(xs, o)
	}
	if len(xs) > 64 {
		xs = xs[len(xs)-64:]
	}
	st.Observations[o.DomainID] = xs
	st.Coverage[o.DomainID] = o.Generation
}

func (m *genModel) genRecordObservation() *diffOp {
	return m.genRecordObservationFor(m.pickDomain())
}

func (m *genModel) genRecordObservationFor(dom string) *diffOp {
	now := m.clock.tick()
	o := domain.Observation{
		DomainID: dom, Generation: "gen-1", Complete: true, StartedAt: now.Add(-time.Second), FinishedAt: now,
		Resources: []domain.Resource{{AccountID: "123456789012", Region: "eu-central-1", Type: "vpc", ID: fmt.Sprintf("vpc-%08d", m.cidrN), CIDR: m.nextCIDR(), State: "in-use"}},
	}
	apply := func(st *domain.State) error {
		applyRecordObservation(st, o)
		return nil
	}
	return &diffOp{name: "recordObservation/" + dom, apply: apply}
}

// genIntentionalRollback gives the "error each Update returned" comparison
// axis real content, and exercises ADR 0017's property 4 ("a closure that
// returns an error changes nothing", postgres.go:151-153,
// TestPostgresUpdateRollbackDoesNotPersistState) differentially: both stores
// must refuse identically and neither may retain the mutation the closure
// attempted before returning the sentinel error.
func (m *genModel) genIntentionalRollback() *diffOp {
	apply := func(st *domain.State) error {
		st.Allocations["should-not-persist"] = domain.Allocation{ID: "should-not-persist"}
		return errIntentionalRollback
	}
	return &diffOp{name: "intentionalRollback", apply: apply}
}

// generateSequence builds the fixed, seeded sequence this test replays. The
// seed is constant so a failure is always reproducible.
func generateSequence(t *testing.T) []*diffOp {
	t.Helper()
	m := newGenModel(20260922)
	kinds := []func() *diffOp{
		func() *diffOp { return m.genReservePlan(genReserveOperation) },
		func() *diffOp { return m.genReservePlan(genAdoptOperation) },
		m.genReserveCommit,
		m.genBindRequest,
		m.genFinishBinding,
		m.genReleaseRequest,
		m.genAbandonFence,
		m.genAbandonDelete,
		m.genCancelFence,
		m.genCancelDelete,
		m.genWorkerRaiseFinding,
		m.genWorkerResolveFinding,
		m.genApplyProjectionSuccess,
		m.genApplyProjectionFailure,
		m.genRecordObservation,
		m.genIntentionalRollback,
	}
	var seq []*diffOp
	attempts, produced := 0, 0
	for produced < 80 && attempts < 4000 {
		attempts++
		op := kinds[m.rng.Intn(len(kinds))]()
		if op == nil {
			continue
		}
		seq = append(seq, op)
		produced++
	}
	if produced < 60 {
		t.Fatalf("generator produced only %d steps in %d attempts; the model's preconditions are too tight to be a meaningful sequence", produced, attempts)
	}
	// A deterministic tail, outside the random walk: seventy consecutive
	// observations of the same domain, to exercise the sixty-four-row
	// retention explicitly and repeatably rather than leaving it to chance.
	for i := 0; i < 70; i++ {
		seq = append(seq, m.genRecordObservationFor("domain-a"))
	}
	return seq
}

// ---------------------------------------------------------------------------
// Normalisation for the reloaded-state comparison (exclusions 3 and 4 above).
// ---------------------------------------------------------------------------

func normTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func normAllocation(a domain.Allocation) domain.Allocation {
	a.CreatedAt = a.CreatedAt.UTC()
	a.UpdatedAt = a.UpdatedAt.UTC()
	a.ReleaseRequestedAt = normTimePtr(a.ReleaseRequestedAt)
	a.QuarantineUntil = normTimePtr(a.QuarantineUntil)
	a.LastObservedAt = normTimePtr(a.LastObservedAt)
	if a.Binding != nil {
		b := *a.Binding
		b.VerifiedAt = normTimePtr(b.VerifiedAt)
		a.Binding = &b
	}
	return a
}

func normOperation(o domain.Operation) domain.Operation {
	o.CreatedAt = o.CreatedAt.UTC()
	o.UpdatedAt = o.UpdatedAt.UTC()
	if o.Candidate != nil {
		c := *o.Candidate
		c.VerifiedAt = normTimePtr(c.VerifiedAt)
		o.Candidate = &c
	}
	return o
}

func normFinding(f domain.Finding) domain.Finding {
	f.FirstObservedAt = f.FirstObservedAt.UTC()
	f.LastObservedAt = f.LastObservedAt.UTC()
	return f
}

func normObservations(xs []domain.Observation) []domain.Observation {
	out := make([]domain.Observation, len(xs))
	for i, o := range xs {
		o.StartedAt = o.StartedAt.UTC()
		o.FinishedAt = o.FinishedAt.UTC()
		out[i] = o
	}
	return out
}

func normEvent(e domain.Event) domain.Event {
	e.At = e.At.UTC()
	return e
}

// normalizeState returns a canonicalised deep copy so that reflect.DeepEqual
// is comparing the two stores' actual content rather than incidental
// representation: every time.Time forced to UTC (both stores already produce
// UTC-located values by construction here, but this makes the comparison
// robust rather than lucky), and Events sorted by id (exclusion 4 above) since
// nothing in this repository depends on their order and the two stores
// legitimately disagree on it.
func normalizeState(s *domain.State) *domain.State {
	out := domain.NewState()
	for id, a := range s.Allocations {
		out.Allocations[id] = normAllocation(a)
	}
	for id, o := range s.Operations {
		out.Operations[id] = normOperation(o)
	}
	for id, r := range s.Requests {
		out.Requests[id] = r
	}
	for id, xs := range s.Observations {
		out.Observations[id] = normObservations(xs)
	}
	for id, f := range s.Findings {
		out.Findings[id] = normFinding(f)
	}
	for id, gen := range s.Coverage {
		out.Coverage[id] = gen
	}
	events := make([]domain.Event, len(s.Events))
	for i, e := range s.Events {
		events[i] = normEvent(e)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	out.Events = events
	return out
}

// ---------------------------------------------------------------------------
// Table-level comparison: the nine tables persistState writes, read directly
// with admin SQL so that allocation_keys, holds and operation_barriers --
// write-only, never read by loadState -- are checked too.
// ---------------------------------------------------------------------------

var nineTables = []string{
	"allocations", "allocation_keys", "operations", "idempotency_requests",
	"observations", "findings", "coverage", "holds", "operation_barriers",
}

type allocRow struct {
	ID, TenantID, AllocationKey string
	Committed                   bool
	CreatedAt                   time.Time
	Payload                     domain.Allocation
}
type allocKeyRow struct {
	TenantID, AllocationKey, AllocationID string
	Retired                               bool
	Payload                               domain.Allocation
}
type opRow struct {
	ID, TenantID, DomainID string
	Payload                domain.Operation
}
type idemRow struct {
	RequestID, TenantID, Method, Path, RequestKey, RequestHash string
	Payload                                                    domain.Idempotency
}
type obsRow struct {
	DomainID string
	Payload  []domain.Observation
}
type findingRow struct {
	ID, TenantID, DomainID string
	Payload                domain.Finding
}
type coverageRow struct{ DomainID, Generation string }
type holdRow struct {
	HoldID, AllocationID, TenantID, DomainID, CIDR string
	Committed                                      bool
	Payload                                        domain.Allocation
}
type barrierRow struct {
	DomainID, OperationID string
	Payload               domain.Operation
}

// expectedTables derives, from a *domain.State exactly as persistState
// derives them, the row set every one of the nine tables should hold. It is
// deliberately the same arithmetic as postgres.go's persistState (comment
// per table names the persistState lines it mirrors), because that
// arithmetic -- not a re-reading of loadState -- is what a table-level
// comparison is checking.
func expectedTables(s *domain.State) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, t := range nineTables {
		out[t] = map[string]any{}
	}
	for id, v := range s.Allocations { // postgres.go:328-339
		out["allocations"][id] = allocRow{ID: id, TenantID: v.TenantID, AllocationKey: v.AllocationKey, Committed: v.Committed, CreatedAt: v.CreatedAt.UTC(), Payload: normAllocation(v)}
		out["allocation_keys"][v.TenantID+"\x1f"+v.AllocationKey] = allocKeyRow{TenantID: v.TenantID, AllocationKey: v.AllocationKey, AllocationID: id, Retired: v.State == domain.Released, Payload: normAllocation(v)}
	}
	for id, v := range s.Operations { // postgres.go:340-348
		out["operations"][id] = opRow{ID: id, TenantID: v.TenantID, DomainID: v.DomainID, Payload: normOperation(v)}
	}
	for id, v := range s.Allocations { // postgres.go:349-357
		out["holds"]["hold_"+id] = holdRow{HoldID: "hold_" + id, AllocationID: id, TenantID: v.TenantID, DomainID: v.DomainID, CIDR: v.CIDR, Committed: v.Committed && v.State != domain.Released, Payload: normAllocation(v)}
	}
	for id, v := range s.Operations { // postgres.go:358-369
		if v.Status != "PENDING" {
			continue
		}
		out["operation_barriers"][v.DomainID] = barrierRow{DomainID: v.DomainID, OperationID: id, Payload: normOperation(v)}
	}
	for id, v := range s.Requests { // postgres.go:370-378
		out["idempotency_requests"][id] = idemRow{RequestID: id, TenantID: v.TenantID, Method: v.Method, Path: v.Path, RequestKey: v.Key, RequestHash: v.Hash, Payload: v}
	}
	for id, v := range s.Observations { // postgres.go:379-387
		out["observations"][id] = obsRow{DomainID: id, Payload: normObservations(v)}
	}
	for id, v := range s.Findings { // postgres.go:388-396
		out["findings"][id] = findingRow{ID: id, TenantID: v.TenantID, DomainID: v.DomainID, Payload: normFinding(v)}
	}
	for id, v := range s.Coverage { // postgres.go:397-401
		out["coverage"][id] = coverageRow{DomainID: id, Generation: v}
	}
	return out
}

// actualTables reads the nine tables directly from the isolated schema.
func actualTables(ctx context.Context, t *testing.T, db *isolatedPostgres) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, tbl := range nineTables {
		out[tbl] = map[string]any{}
	}
	schema := quoteIdentifier(db.schema)

	rows, err := db.admin.Query(ctx, `SELECT id,tenant_id,allocation_key,committed,payload,created_at FROM `+schema+`.allocations`)
	if err != nil {
		t.Fatalf("query allocations: %v", err)
	}
	for rows.Next() {
		var id, tenant, key string
		var committed bool
		var payload []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &tenant, &key, &committed, &payload, &createdAt); err != nil {
			t.Fatalf("scan allocations: %v", err)
		}
		var a domain.Allocation
		mustUnmarshal(t, payload, &a)
		out["allocations"][id] = allocRow{ID: id, TenantID: tenant, AllocationKey: key, Committed: committed, CreatedAt: createdAt.UTC(), Payload: normAllocation(a)}
	}
	rows.Close()

	rows, err = db.admin.Query(ctx, `SELECT tenant_id,allocation_key,allocation_id,retired,payload FROM `+schema+`.allocation_keys`)
	if err != nil {
		t.Fatalf("query allocation_keys: %v", err)
	}
	for rows.Next() {
		var tenant, key, allocID string
		var retired bool
		var payload []byte
		if err := rows.Scan(&tenant, &key, &allocID, &retired, &payload); err != nil {
			t.Fatalf("scan allocation_keys: %v", err)
		}
		var a domain.Allocation
		mustUnmarshal(t, payload, &a)
		out["allocation_keys"][tenant+"\x1f"+key] = allocKeyRow{TenantID: tenant, AllocationKey: key, AllocationID: allocID, Retired: retired, Payload: normAllocation(a)}
	}
	rows.Close()

	rows, err = db.admin.Query(ctx, `SELECT id,tenant_id,domain_id,payload FROM `+schema+`.operations`)
	if err != nil {
		t.Fatalf("query operations: %v", err)
	}
	for rows.Next() {
		var id, tenant, dom string
		var payload []byte
		if err := rows.Scan(&id, &tenant, &dom, &payload); err != nil {
			t.Fatalf("scan operations: %v", err)
		}
		var o domain.Operation
		mustUnmarshal(t, payload, &o)
		out["operations"][id] = opRow{ID: id, TenantID: tenant, DomainID: dom, Payload: normOperation(o)}
	}
	rows.Close()

	rows, err = db.admin.Query(ctx, `SELECT request_id,tenant_id,method,path,request_key,request_hash,payload FROM `+schema+`.idempotency_requests`)
	if err != nil {
		t.Fatalf("query idempotency_requests: %v", err)
	}
	for rows.Next() {
		var id, tenant, method, path, key, hash string
		var payload []byte
		if err := rows.Scan(&id, &tenant, &method, &path, &key, &hash, &payload); err != nil {
			t.Fatalf("scan idempotency_requests: %v", err)
		}
		var v domain.Idempotency
		mustUnmarshal(t, payload, &v)
		out["idempotency_requests"][id] = idemRow{RequestID: id, TenantID: tenant, Method: method, Path: path, RequestKey: key, RequestHash: hash, Payload: v}
	}
	rows.Close()

	rows, err = db.admin.Query(ctx, `SELECT domain_id,payload FROM `+schema+`.observations`)
	if err != nil {
		t.Fatalf("query observations: %v", err)
	}
	for rows.Next() {
		var dom string
		var payload []byte
		if err := rows.Scan(&dom, &payload); err != nil {
			t.Fatalf("scan observations: %v", err)
		}
		var v []domain.Observation
		mustUnmarshal(t, payload, &v)
		out["observations"][dom] = obsRow{DomainID: dom, Payload: normObservations(v)}
	}
	rows.Close()

	rows, err = db.admin.Query(ctx, `SELECT id,tenant_id,domain_id,payload FROM `+schema+`.findings`)
	if err != nil {
		t.Fatalf("query findings: %v", err)
	}
	for rows.Next() {
		var id, tenant, dom string
		var payload []byte
		if err := rows.Scan(&id, &tenant, &dom, &payload); err != nil {
			t.Fatalf("scan findings: %v", err)
		}
		var v domain.Finding
		mustUnmarshal(t, payload, &v)
		out["findings"][id] = findingRow{ID: id, TenantID: tenant, DomainID: dom, Payload: normFinding(v)}
	}
	rows.Close()

	rows, err = db.admin.Query(ctx, `SELECT domain_id,generation FROM `+schema+`.coverage`)
	if err != nil {
		t.Fatalf("query coverage: %v", err)
	}
	for rows.Next() {
		var dom, gen string
		if err := rows.Scan(&dom, &gen); err != nil {
			t.Fatalf("scan coverage: %v", err)
		}
		out["coverage"][dom] = coverageRow{DomainID: dom, Generation: gen}
	}
	rows.Close()

	rows, err = db.admin.Query(ctx, `SELECT hold_id,allocation_id,tenant_id,domain_id,cidr::text,committed,payload FROM `+schema+`.holds`)
	if err != nil {
		t.Fatalf("query holds: %v", err)
	}
	for rows.Next() {
		var holdID, allocID, tenant, dom, cidr string
		var committed bool
		var payload []byte
		if err := rows.Scan(&holdID, &allocID, &tenant, &dom, &cidr, &committed, &payload); err != nil {
			t.Fatalf("scan holds: %v", err)
		}
		var a domain.Allocation
		mustUnmarshal(t, payload, &a)
		out["holds"][holdID] = holdRow{HoldID: holdID, AllocationID: allocID, TenantID: tenant, DomainID: dom, CIDR: cidr, Committed: committed, Payload: normAllocation(a)}
	}
	rows.Close()

	rows, err = db.admin.Query(ctx, `SELECT domain_id,operation_id,payload FROM `+schema+`.operation_barriers`)
	if err != nil {
		t.Fatalf("query operation_barriers: %v", err)
	}
	for rows.Next() {
		var dom, opID string
		var payload []byte
		if err := rows.Scan(&dom, &opID, &payload); err != nil {
			t.Fatalf("scan operation_barriers: %v", err)
		}
		var o domain.Operation
		mustUnmarshal(t, payload, &o)
		out["operation_barriers"][dom] = barrierRow{DomainID: dom, OperationID: opID, Payload: normOperation(o)}
	}
	rows.Close()

	return out
}

func mustUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The test itself.
// ---------------------------------------------------------------------------

func TestDifferentialMemoryAndPostgresAgreeOverGeneratedSequences(t *testing.T) {
	db := requireIsolatedPostgres(t)
	pg := db.openLedger()
	if err := pg.Migrate(db.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mem := NewMemoryLedger()

	ops := generateSequence(t)
	t.Logf("generated %d steps", len(ops))

	for i, op := range ops {
		beforeMem, err := mem.Snapshot(db.ctx)
		if err != nil {
			t.Fatalf("step %d (%s): memory snapshot before: %v", i, op.name, err)
		}

		errMem := mem.Update(db.ctx, op.apply)
		errPg := pg.Update(db.ctx, op.apply)

		if errors.Is(errMem, errIntentionalRollback) || errors.Is(errPg, errIntentionalRollback) {
			if !errors.Is(errMem, errIntentionalRollback) || !errors.Is(errPg, errIntentionalRollback) {
				t.Fatalf("step %d (%s): rollback error mismatch: memory=%v postgres=%v", i, op.name, errMem, errPg)
			}
			afterMem, err := mem.Snapshot(db.ctx)
			if err != nil {
				t.Fatalf("step %d (%s): memory snapshot after: %v", i, op.name, err)
			}
			if !reflect.DeepEqual(normalizeState(beforeMem), normalizeState(afterMem)) {
				t.Fatalf("step %d (%s): memory store retained a rolled-back mutation", i, op.name)
			}
			var pgAfter *domain.State
			if err := pg.View(db.ctx, func(s *domain.State) error { pgAfter = s; return nil }); err != nil {
				t.Fatalf("step %d (%s): postgres view after rollback: %v", i, op.name, err)
			}
			if _, ok := pgAfter.Allocations["should-not-persist"]; ok {
				t.Fatalf("step %d (%s): postgres store retained a rolled-back mutation", i, op.name)
			}
			continue
		}

		if (errMem == nil) != (errPg == nil) {
			t.Fatalf("step %d (%s): error mismatch: memory=%v postgres=%v", i, op.name, errMem, errPg)
		}
		if errMem != nil {
			// Neither store accepted the step; both refused identically.
			// Nothing was persisted on either side, so there is nothing
			// further to compare for this step.
			continue
		}

		memState, err := mem.Snapshot(db.ctx)
		if err != nil {
			t.Fatalf("step %d (%s): memory snapshot: %v", i, op.name, err)
		}
		var pgState *domain.State
		if err := pg.View(db.ctx, func(s *domain.State) error { pgState = s; return nil }); err != nil {
			t.Fatalf("step %d (%s): postgres view: %v", i, op.name, err)
		}
		if !reflect.DeepEqual(normalizeState(memState), normalizeState(pgState)) {
			t.Fatalf("step %d (%s): reloaded state diverged\nmemory:   %+v\npostgres: %+v", i, op.name, normalizeState(memState), normalizeState(pgState))
		}

		want := expectedTables(memState)
		got := actualTables(db.ctx, t, db)
		for _, table := range nineTables {
			if !reflect.DeepEqual(want[table], got[table]) {
				t.Fatalf("step %d (%s): table %q diverged\nwant: %#v\ngot:  %#v", i, op.name, table, want[table], got[table])
			}
		}
	}
}
