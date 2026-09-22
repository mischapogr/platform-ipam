package storage

// TestMeasureStatementsPerUpdate is a measurement, not a correctness test
// (work-plan package M4d, ADR 0017's "what M4a could not contain": a direct
// count of round trips per Ledger.Update as a function of each state map's
// size). It needs no database connection.
//
// Instrument chosen, and why: persistState(ctx, tx pgx.Tx, s *domain.State)
// (postgres.go) is called with a concrete pgx.Tx *interface* value, and never
// does anything but call tx.Exec -- no Query, no QueryRow, no CopyFrom, no
// SendBatch, no Prepare, no nested Begin, no Conn(). A counting fake that
// implements the other eight pgx.Tx methods by panicking is therefore both
// feasible and the most honest choice available without a database: it
// exercises the REAL persistState function, not a reimplementation of its
// statement count, so a future change to persistState's write pattern changes
// this test's numbers automatically, and a future change that adds a Query or
// a CopyFrom call makes the fake panic loudly rather than silently
// undercounting. The alternative considered and rejected was a pgx
// QueryTracer, which needs a real *pgx.Conn/pgxpool.Pool to attach to and so
// needs a database -- exactly what this measurement must not require.
//
// The fake counts one entry per tx.Exec call, tagged with the table name
// parsed from the SQL text's own "INSERT INTO <table>" / "DELETE FROM <table>"
// prefix (persistState issues no other statement shape; the parser is checked
// against a table below, not trusted blindly).
//
// Updated for package M4e: persistState now takes a loadedEvents argument,
// the set of audit event ids the transaction's loadState call already found
// durably present. A real Update never populates it (M4e also stops Update
// from loading audit_events at all -- see postgres.go's loadState), so every
// event a real closure appends is, by construction, absent from
// loadedEvents and gets inserted. This measurement reproduces that shape
// rather than assuming it: for each swept "events" size it builds a
// synthetic ledger's worth of historical events, marks every one of them
// loaded (the way a real ledger's history would already be durable and
// therefore never re-offered), and then appends a small, fixed number of new
// events the way one real transaction actually does. The formula below drops
// the E term entirely and replaces it with that fixed constant, which is
// exactly M4e's claim: the write's cost no longer grows with the ledger's
// audit history, however large.
//
// Updated again for package M4f: persistState now also takes loaded, a
// *domain.State snapshot of what the transaction's loadState call read for
// the other eight tables (nil, as in every call in this file except
// TestMeasureStatementsPerUpdateProportionalToChange below, means "nothing
// was loaded" -- every row persistState sees is therefore classified
// "added", which is why TestMeasureStatementsPerUpdate's own formula below
// no longer has a constant term: pre-M4f, persistState opened every
// transaction with nine unconditional `DELETE FROM <table>` statements
// whether or not there was anything to delete; post-M4f, deletion is driven
// by the loaded key set, so a transaction with nothing loaded issues none.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// countingTx implements pgx.Tx (v5.10.0's eleven-method interface) by
// counting Exec calls and panicking on every method persistState never uses,
// so the panic path itself is evidence the fake matches the real call shape.
type countingTx struct {
	execTables []string
	// execKinds is the leading keyword of each recorded statement ("INSERT"
	// or "DELETE"), parallel to execTables. Added for package M4f: unlike
	// the pre-M4f store, which always issued a fixed nine deletes before
	// anything else, this store's DELETE count is itself part of what a
	// measurement needs to see -- a cold Update (nothing loaded) should
	// issue none at all.
	execKinds []string
}

func (c *countingTx) Begin(context.Context) (pgx.Tx, error) {
	panic("measurement fake: persistState never calls Tx.Begin")
}
func (c *countingTx) Commit(context.Context) error {
	panic("measurement fake: persistState never calls Tx.Commit (the caller, Update, does)")
}
func (c *countingTx) Rollback(context.Context) error {
	panic("measurement fake: persistState never calls Tx.Rollback (the caller, Update, does)")
}
func (c *countingTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("measurement fake: persistState never calls Tx.CopyFrom today -- if this fires, the round-trip formula below is stale")
}
func (c *countingTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("measurement fake: persistState never calls Tx.SendBatch today -- if this fires, the round-trip formula below is stale")
}
func (c *countingTx) LargeObjects() pgx.LargeObjects {
	panic("measurement fake: persistState never calls Tx.LargeObjects")
}
func (c *countingTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("measurement fake: persistState never calls Tx.Prepare")
}
func (c *countingTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("measurement fake: persistState never calls Tx.Query (only loadState does, and loadState is not under test here)")
}
func (c *countingTx) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("measurement fake: persistState never calls Tx.QueryRow")
}
func (c *countingTx) Conn() *pgx.Conn { return nil }

func (c *countingTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	c.execTables = append(c.execTables, tableFromSQL(sql))
	kind := "OTHER"
	switch {
	case strings.HasPrefix(sql, "INSERT"):
		kind = "INSERT"
	case strings.HasPrefix(sql, "DELETE"):
		kind = "DELETE"
	}
	c.execKinds = append(c.execKinds, kind)
	return pgconn.CommandTag{}, nil
}

// tableFromSQL extracts the table name from persistState's own two statement
// shapes ("DELETE FROM <table>" and "INSERT INTO <table>(..."). It is
// deliberately narrow -- persistState issues no third shape -- and returns a
// visibly-wrong sentinel for anything else so a shape change is loud, not
// silently mis-tallied.
func tableFromSQL(sql string) string {
	for _, kw := range []string{"INSERT INTO ", "DELETE FROM "} {
		if idx := strings.Index(sql, kw); idx >= 0 {
			rest := sql[idx+len(kw):]
			end := strings.IndexAny(rest, "( \n\t")
			if end < 0 {
				end = len(rest)
			}
			return rest[:end]
		}
	}
	return "UNRECOGNIZED_STATEMENT_SHAPE: " + sql
}

func TestTableFromSQLRecognizesEveryPersistStateStatement(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"DELETE FROM allocations", "allocations"},
		{`INSERT INTO allocations(id,tenant_id,allocation_key,committed,payload,created_at) VALUES($1,$2,$3,$4,$5,$6)`, "allocations"},
		{`INSERT INTO allocation_keys(tenant_id,allocation_key,allocation_id,retired,payload) VALUES($1,$2,$3,$4,$5)`, "allocation_keys"},
		{`INSERT INTO holds(hold_id,allocation_id,tenant_id,domain_id,cidr,committed,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, "holds"},
		{`INSERT INTO operations(id,tenant_id,domain_id,payload) VALUES($1,$2,$3,$4)`, "operations"},
		{`INSERT INTO operation_barriers(domain_id,operation_id,payload) VALUES($1,$2,$3) ON CONFLICT (domain_id) DO UPDATE SET operation_id=excluded.operation_id,payload=excluded.payload`, "operation_barriers"},
		{`INSERT INTO idempotency_requests(request_id,tenant_id,method,path,request_key,request_hash,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, "idempotency_requests"},
		{`INSERT INTO observations(domain_id,payload) VALUES($1,$2)`, "observations"},
		{`INSERT INTO findings(id,tenant_id,domain_id,payload) VALUES($1,$2,$3,$4)`, "findings"},
		{`INSERT INTO coverage(domain_id,generation) VALUES($1,$2)`, "coverage"},
		{`INSERT INTO audit_events(id,allocation_id,tenant_id,payload) VALUES($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING`, "audit_events"},
	}
	for _, c := range cases {
		if got := tableFromSQL(c.sql); got != c.want {
			t.Errorf("tableFromSQL(%q) = %q, want %q", c.sql, got, c.want)
		}
	}
}

// sizes names every map domain.State carries that persistState writes a
// row per entry of, plus the derived pending-operations count (P in the
// record's formula), which is not an independent axis: it is a fraction of
// Operations.
type sizes struct {
	allocations int
	operations  int
	idempotency int
	findings    int
	obsDomains  int
	coverage    int
	events      int
}

// pendingOperations reproduces the record's P term: persistState inserts one
// operation_barriers row per operation whose Status is PENDING
// (postgres.go:358-369). About half of a synthetic operation set is given
// PENDING status, which is neither 0 nor "all" -- either extreme would hide a
// P-specific bug (P==O or P==0 both collapse the P and O terms together).
func (s sizes) pendingOperations() int {
	if s.operations == 0 {
		return 0
	}
	p := s.operations / 2
	if p == 0 {
		p = 1
	}
	return p
}

func stateWithSizes(sz sizes) *domain.State {
	st := domain.NewState()
	now := time.Now().UTC()

	for i := 0; i < sz.allocations; i++ {
		id := fmt.Sprintf("alloc-%07d", i)
		st.Allocations[id] = domain.Allocation{
			ID:       id,
			TenantID: "measure",
			DomainID: fmt.Sprintf("dom-%07d", i%97),
			Request: domain.Request{
				AllocationKey: fmt.Sprintf("key-%07d", i),
				Scope:         "vpc",
				Environment:   "development",
				Region:        "eu-central-1",
				PrefixLength:  24,
				Labels:        map[string]string{},
			},
			CIDR:      fmt.Sprintf("10.%d.%d.0/24", (i/256)%256, i%256),
			PoolID:    "pool_dev_euc1",
			State:     domain.Reserved,
			Committed: true,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}

	pending := sz.pendingOperations()
	for i := 0; i < sz.operations; i++ {
		id := fmt.Sprintf("op-%07d", i)
		status := "DONE"
		if i < pending {
			status = "PENDING"
		}
		st.Operations[id] = domain.Operation{
			ID:        id,
			Type:      "RESERVE",
			Status:    status,
			TenantID:  "measure",
			DomainID:  fmt.Sprintf("opdom-%07d", i),
			Result:    map[string]string{},
			CreatedAt: now,
			UpdatedAt: now,
		}
	}

	for i := 0; i < sz.idempotency; i++ {
		id := fmt.Sprintf("req-%07d", i)
		st.Requests[id] = domain.Idempotency{
			TenantID: "measure",
			Method:   "POST",
			Path:     "/v1/allocations",
			Key:      fmt.Sprintf("key-%07d", i),
			Hash:     "h",
		}
	}

	for i := 0; i < sz.obsDomains; i++ {
		id := fmt.Sprintf("obsdom-%07d", i)
		st.Observations[id] = []domain.Observation{{
			DomainID:   id,
			Generation: "gen-1",
			Complete:   true,
			StartedAt:  now,
			FinishedAt: now,
			Resources: []domain.Resource{
				{AccountID: "000000000000", Region: "eu-central-1", Type: "vpc", ID: "vpc-1", CIDR: "10.0.0.0/16"},
			},
		}}
	}

	for i := 0; i < sz.findings; i++ {
		id := fmt.Sprintf("find-%07d", i)
		st.Findings[id] = domain.Finding{
			ID:              id,
			TenantID:        "measure",
			DomainID:        fmt.Sprintf("finddom-%07d", i%97),
			Code:            "reservation_stuck",
			Severity:        "warning",
			Status:          "OPEN",
			FirstObservedAt: now,
			LastObservedAt:  now,
		}
	}

	for i := 0; i < sz.coverage; i++ {
		st.Coverage[fmt.Sprintf("covdom-%07d", i)] = "gen-1"
	}

	for i := 0; i < sz.events; i++ {
		st.Events = append(st.Events, domain.Event{
			ID:           fmt.Sprintf("evt-%07d", i),
			AllocationID: fmt.Sprintf("alloc-%07d", i%97),
			TenantID:     "measure",
			Actor:        "measure",
			Action:       "reserved",
			Reason:       "synthetic measurement event",
			At:           now,
			Revision:     1,
		})
	}

	return st
}

// newEventsPerUpdate is how many events one real Update transaction's closure
// appends in this measurement -- a reservation's plan-then-commit pair is the
// model (service.go:366, service.go:435), which is two. It replaces the old E
// term in expectedRoundTrips below: after package M4e, a transaction's audit
// write cost is the number of events IT appends, not the ledger's total
// history, so sweeping "events" (the synthetic ledger's historical size) must
// leave the formula's audit term unchanged at this fixed constant.
const newEventsPerUpdate = 2

// expectedRoundTrips reproduces the COLD case of ADR 0017's formula -- every
// row of a freshly-built *domain.State persisted against an empty loaded
// snapshot (nil, meaning nothing durable yet), so every row is classified
// "added": three writes per allocation (allocations, allocation_keys,
// holds), one per operation, one per PENDING operation, one per idempotency
// record, one per observation-carrying routing domain, one per finding, one
// per coverage generation -- and, since package M4e, newEventsPerUpdate
// audit-event inserts rather than one per historical event, because Update no
// longer loads (and persistState no longer re-offers) the events already
// durably present when the transaction began.
//
// Before package M4f the formula had a leading "9 +": persistState opened
// every transaction, cold or not, with nine unconditional `DELETE FROM
// <table>` statements. Package M4f's diff computes deletion from the loaded
// key set, so a cold transaction (nothing loaded, as every call in this
// sweep still is) issues none -- the constant is gone, not renamed.
// TestMeasureStatementsPerUpdateProportionalToChange below is the
// complementary WARM measurement M4f's own package specifically asked for:
// a large, fully-loaded ledger with a small number of rows actually changed.
func expectedRoundTrips(sz sizes) int {
	return 3*sz.allocations + sz.operations + sz.pendingOperations() +
		sz.idempotency + sz.obsDomains + sz.findings + sz.coverage + newEventsPerUpdate
}

func TestMeasureStatementsPerUpdate(t *testing.T) {
	if os.Getenv("IPAM_MEASURE") != "1" {
		t.Skip("set IPAM_MEASURE=1 to run this measurement (package M4d); it needs no database and is skipped by default so it never slows the ordinary suite")
	}

	const small = 3 // held constant on every axis not being swept, and nonzero so the other eight terms are never trivially absent
	const mid = 100
	const big = 1000

	base := sizes{allocations: small, operations: small, idempotency: small, findings: small, obsDomains: small, coverage: small, events: small}

	type row struct {
		axis string
		size int
		sz   sizes
	}
	var rows []row
	rows = append(rows, row{"(baseline, all axes held at " + fmt.Sprint(small) + ")", small, base})
	for _, axis := range []string{"allocations", "operations", "idempotency", "findings", "obsDomains", "coverage", "events"} {
		for _, n := range []int{0, mid, big} {
			sz := base
			switch axis {
			case "allocations":
				sz.allocations = n
			case "operations":
				sz.operations = n
			case "idempotency":
				sz.idempotency = n
			case "findings":
				sz.findings = n
			case "obsDomains":
				sz.obsDomains = n
			case "coverage":
				sz.coverage = n
			case "events":
				sz.events = n
			}
			rows = append(rows, row{axis, n, sz})
		}
	}

	t.Log("statements (round trips) per Ledger.Update, by swept state-map size, others held at " + fmt.Sprint(small))
	t.Log("axis\tsize\tA\tO\tP\tR\tD\tF\tC\tE(historical)\tmeasured_exec_calls\tformula_9+3A+O+P+R+D+F+C+newEventsPerUpdate\tmatch")
	allMatch := true
	for _, r := range rows {
		ctx := context.Background()
		fake := &countingTx{}
		st := stateWithSizes(r.sz)
		// Reproduce what a real post-M4e Update transaction sees: every event
		// stateWithSizes generated for the swept "events" size stands in for
		// the ledger's pre-existing history, which Update no longer loads --
		// so mark all of it "loaded" (already durable) the way an empty
		// st.Events at closure-start would make true by construction, then
		// append newEventsPerUpdate fresh events the way one real closure
		// does. Only the fresh ones should ever reach persistState's INSERT.
		loaded := make(map[string]struct{}, len(st.Events))
		for _, e := range st.Events {
			loaded[e.ID] = struct{}{}
		}
		for i := 0; i < newEventsPerUpdate; i++ {
			st.Events = append(st.Events, domain.Event{
				ID:           fmt.Sprintf("evt-new-%07d", i),
				AllocationID: "alloc-0000000",
				TenantID:     "measure",
				Actor:        "measure",
				Action:       "reserved",
				Reason:       "synthetic new-this-transaction event",
				At:           time.Now().UTC(),
				Revision:     1,
			})
		}
		if err := persistState(ctx, fake, st, nil, loaded); err != nil {
			t.Fatalf("persistState(%s=%d): %v", r.axis, r.size, err)
		}
		measured := len(fake.execTables)
		formula := expectedRoundTrips(r.sz)
		match := measured == formula
		allMatch = allMatch && match
		t.Logf("%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%v",
			r.axis, r.size, r.sz.allocations, r.sz.operations, r.sz.pendingOperations(), r.sz.idempotency,
			r.sz.obsDomains, r.sz.findings, r.sz.coverage, r.sz.events, measured, formula, match)

		// Cross-check the per-table breakdown against the formula's own named
		// terms, not just the total, so a compensating error (one table over,
		// another under, same total) cannot hide.
		counts := map[string]int{}
		for _, tbl := range fake.execTables {
			counts[tbl]++
			if strings.HasPrefix(tbl, "UNRECOGNIZED_STATEMENT_SHAPE") {
				t.Errorf("%s=%d: %s", r.axis, r.size, tbl)
			}
		}
		wantByTable := map[string]int{
			"allocations":          r.sz.allocations,
			"allocation_keys":      r.sz.allocations,
			"holds":                r.sz.allocations,
			"operations":           r.sz.operations,
			"operation_barriers":   r.sz.pendingOperations(),
			"idempotency_requests": r.sz.idempotency,
			"observations":         r.sz.obsDomains,
			"findings":             r.sz.findings,
			"coverage":             r.sz.coverage,
			"audit_events":         newEventsPerUpdate,
		}
		for tbl, want := range wantByTable {
			// Package M4f: every Exec call tagged with this table IS an
			// INSERT (this sweep never loads anything, so nothing is ever
			// classified "removed" and no DELETE is ever issued -- unlike
			// the pre-M4f store, which cleared every one of the nine tables
			// unconditionally before writing a single row).
			if got := counts[tbl]; got != want {
				t.Errorf("%s=%d: table %s got %d inserts, want %d", r.axis, r.size, tbl, got, want)
			}
		}
		for _, kind := range fake.execKinds {
			if kind == "DELETE" {
				t.Errorf("%s=%d: a cold Update (nothing loaded) issued a DELETE statement; deletion must be driven by the loaded key set, which was empty here", r.axis, r.size)
				break
			}
		}
	}
	if !allMatch {
		t.Error("at least one row's measured Exec-call count disagreed with the formula 9 + 3A + O + P + R + D + F + C + newEventsPerUpdate -- see the per-table breakdown above for which term is wrong")
	}
}

// TestPersistStateInsertsEveryNewEvent is an ordinary, always-on correctness
// test (unlike TestMeasureStatementsPerUpdate above, it needs no
// IPAM_MEASURE and no database): every event present in s.Events that is not
// in loadedEvents must reach an INSERT. It exists to catch the mutation
// "an appended event dropped" -- an off-by-one or an early continue in
// persistState's events loop that silently skips a newly appended event.
func TestPersistStateInsertsEveryNewEvent(t *testing.T) {
	st := domain.NewState()
	st.Events = []domain.Event{
		{ID: "evt_a", AllocationID: "alloc_1", TenantID: "t", Action: "RESERVE_PLANNED"},
		{ID: "evt_b", AllocationID: "alloc_1", TenantID: "t", Action: "RESERVE_COMMITTED"},
		{ID: "evt_c", AllocationID: "alloc_2", TenantID: "t", Action: "RELEASE_REQUESTED"},
	}
	fake := &countingTx{}
	if err := persistState(context.Background(), fake, st, nil, nil); err != nil {
		t.Fatalf("persistState: %v", err)
	}
	got := 0
	for _, tbl := range fake.execTables {
		if tbl == "audit_events" {
			got++
		}
	}
	if got != len(st.Events) {
		t.Fatalf("persistState issued %d audit_events inserts for %d newly appended events, want %d (nothing was loaded, so nothing should be skipped)", got, len(st.Events), len(st.Events))
	}
}

// TestPersistStateSkipsAlreadyLoadedEvents is the write-side proof of package
// M4e's decision one, isolated from whether Update currently loads events at
// all: given a loadedEvents set, persistState must insert only the events of
// s.Events whose ids are absent from it. It exists to catch the mutation
// "a loaded event re-offered" -- removing or weakening the
// `if _, already := loadedEvents[v.ID]; already { continue }` check, which a
// real Update transaction's own always-empty loadedEvents (Update no longer
// loads audit_events, see postgres.go) would never exercise on its own, so
// this direct call is the only thing that can catch that specific mutation.
func TestPersistStateSkipsAlreadyLoadedEvents(t *testing.T) {
	st := domain.NewState()
	st.Events = []domain.Event{
		{ID: "evt_old_1", AllocationID: "alloc_1", TenantID: "t", Action: "RESERVE_PLANNED"},
		{ID: "evt_old_2", AllocationID: "alloc_1", TenantID: "t", Action: "RESERVE_COMMITTED"},
		{ID: "evt_new", AllocationID: "alloc_1", TenantID: "t", Action: "RELEASE_REQUESTED"},
	}
	loaded := map[string]struct{}{"evt_old_1": {}, "evt_old_2": {}}
	fake := &countingTx{}
	if err := persistState(context.Background(), fake, st, nil, loaded); err != nil {
		t.Fatalf("persistState: %v", err)
	}
	got := 0
	for _, tbl := range fake.execTables {
		if tbl == "audit_events" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("persistState issued %d audit_events inserts, want 1 (only evt_new, since evt_old_1 and evt_old_2 were already loaded)", got)
	}
}

// TestMeasureStatementsPerUpdateProportionalToChange is package M4f's own
// measurement, explicitly asked for beside M4d's: unlike
// TestMeasureStatementsPerUpdate above (which always measures the COLD case,
// nothing loaded, everything added), this builds a full thousand-row ledger
// on every one of the six diffed axes as the LOADED baseline, changes
// exactly one row per axis, and counts. It needs no IPAM_MEASURE gate and no
// database, and it is not a "measurement" in M4d's sense of an approximate,
// machine-dependent number -- it is an exact, deterministic statement count,
// so it runs as an always-on correctness test: a regression that makes
// persistState start rewriting unchanged rows again would fail it directly.
func TestMeasureStatementsPerUpdateProportionalToChange(t *testing.T) {
	const big = 1000
	base := sizes{allocations: big, operations: big, idempotency: big, findings: big, obsDomains: big, coverage: big}
	loaded := stateWithSizes(base)
	// cloneState (memory.go) is a full JSON round trip: current shares
	// nothing mutable with loaded, so mutating current below cannot move
	// loaded's own comparison baseline out from under it -- the same
	// independence loadState itself buys by decoding each row's bytes twice
	// (see loadState's doc comment in postgres.go).
	current := cloneState(loaded)

	// One changed row per axis, chosen to be isolated to that axis alone:
	// alloc-0000000 is not itself a barrier-holder concern (changing an
	// allocation only ever touches allocations/allocation_keys/holds, by
	// package M4f's design), and op-0000999 is deliberately a DONE operation
	// (i=999 >= pendingOperations()=500, see stateWithSizes), so changing it
	// never touches operation_barriers -- this measurement is about the six
	// primary tables' own diff, not the barrier derivation, which
	// TestPersistStateBarrierFollowsItsWinningOperation below covers
	// directly.
	a := current.Allocations["alloc-0000000"]
	a.Description = "changed for this measurement"
	current.Allocations["alloc-0000000"] = a

	o := current.Operations["op-0000999"]
	if o.Status != "DONE" {
		t.Fatalf("test setup: op-0000999 must be DONE (not a barrier-holder) so this measurement isolates the operations table alone; got %q", o.Status)
	}
	o.Result = map[string]string{"changed": "for this measurement"}
	current.Operations["op-0000999"] = o

	r := current.Requests["req-0000000"]
	r.Hash = "changed-for-this-measurement"
	current.Requests["req-0000000"] = r

	obs := append([]domain.Observation(nil), current.Observations["obsdom-0000000"]...)
	obs[0].Generation = "gen-2"
	current.Observations["obsdom-0000000"] = obs

	f := current.Findings["find-0000000"]
	f.Status = "RESOLVED"
	current.Findings["find-0000000"] = f

	current.Coverage["covdom-0000000"] = "gen-2"

	fake := &countingTx{}
	if err := persistState(context.Background(), fake, current, loaded, nil); err != nil {
		t.Fatalf("persistState: %v", err)
	}

	counts := map[string]int{}
	for _, tbl := range fake.execTables {
		counts[tbl]++
	}
	t.Logf("a %d-row-per-table ledger (%d rows total across six tables) with exactly one row changed on each of six axes: %d total Exec calls; per table: %v",
		big, 6*big, len(fake.execTables), counts)

	want := map[string]int{
		"allocations":          1,
		"allocation_keys":      1,
		"holds":                1,
		"operations":           1,
		"idempotency_requests": 1,
		"observations":         1,
		"findings":             1,
		"coverage":             1,
	}
	for tbl, w := range want {
		if got := counts[tbl]; got != w {
			t.Errorf("table %s: got %d statements, want %d (one row changed on this axis, %d unchanged rows on it)", tbl, got, w, big-1)
		}
	}
	if got := counts["operation_barriers"]; got != 0 {
		t.Errorf("operation_barriers: got %d statements, want 0 -- op-0000999 was DONE, never a barrier-holder", got)
	}
	if wantTotal := len(want); len(fake.execTables) != wantTotal {
		t.Errorf("total Exec calls = %d, want %d (one statement per changed table, %d unchanged tables touched not at all)", len(fake.execTables), wantTotal, 0)
	}
	for _, kind := range fake.execKinds {
		if kind != "INSERT" {
			t.Errorf("a pure content change (no row added or removed on any axis) issued a %s statement; want only INSERT ... ON CONFLICT DO UPDATE", kind)
		}
	}
}

// TestPersistStateBarrierFollowsItsWinningOperation is the operation_barriers
// counterpart to the measurement above: changing the CONTENT of the
// operation that is currently a domain's pending barrier-holder must also
// rewrite that domain's operation_barriers row (its payload column mirrors
// the winning operation's own payload), even though the winning operation's
// identity and the domain's PENDING/committed status do not change.
func TestPersistStateBarrierFollowsItsWinningOperation(t *testing.T) {
	loaded := domain.NewState()
	loaded.Operations["op_pending"] = domain.Operation{ID: "op_pending", Type: "RESERVE", Status: "PENDING", DomainID: "dom-a", TenantID: "t"}
	current := cloneState(loaded)
	o := current.Operations["op_pending"]
	o.Result = map[string]string{"touched": "yes"}
	current.Operations["op_pending"] = o

	fake := &countingTx{}
	if err := persistState(context.Background(), fake, current, loaded, nil); err != nil {
		t.Fatalf("persistState: %v", err)
	}
	counts := map[string]int{}
	for _, tbl := range fake.execTables {
		counts[tbl]++
	}
	if counts["operations"] != 1 {
		t.Errorf("operations: got %d statements, want 1", counts["operations"])
	}
	if counts["operation_barriers"] != 1 {
		t.Errorf("operation_barriers: got %d statements, want 1 -- the winning operation's content changed, so its barrier row's payload (a copy of that content) must be rewritten too", counts["operation_barriers"])
	}
}

// TestPersistStateDeletesEveryRemovedRow exists because M4c's differential
// harness, whose generator deliberately mirrors only the shapes
// internal/service really produces, cannot exercise every removed-row path
// this diff has to get right: no production closure ever deletes an
// operation from State.Operations (an abandon or cancel marks it terminal
// and keeps it -- checked, grepping every "delete(st.Operations" and
// "delete(state.Operations" site in internal/service: none exist), so a
// mutant that breaks operations' removed-loop specifically survives the
// harness's 150-step run untouched (verified: it does). This test covers
// every one of the nine tables' removed path directly and unconditionally,
// independent of whether today's service ever happens to produce that
// shape, because the diff itself promises the general property "a key
// present in loaded and absent from the closure's result is deleted" for
// every table, not just the ones service.go currently exercises.
func TestPersistStateDeletesEveryRemovedRow(t *testing.T) {
	loaded := domain.NewState()
	loaded.Allocations["alloc_1"] = domain.Allocation{ID: "alloc_1", TenantID: "t", DomainID: "d", CIDR: "10.0.0.0/24", State: domain.Reserved, Request: domain.Request{AllocationKey: "k"}}
	loaded.Operations["op_1"] = domain.Operation{ID: "op_1", TenantID: "t", DomainID: "d", Status: "PENDING"} // also this domain's operation_barriers row.
	loaded.Requests["req_1"] = domain.Idempotency{TenantID: "t", Method: "POST", Path: "p", Key: "k1"}
	loaded.Observations["obsdom_1"] = []domain.Observation{{DomainID: "obsdom_1"}}
	loaded.Findings["find_1"] = domain.Finding{ID: "find_1", TenantID: "t", DomainID: "d"}
	loaded.Coverage["covdom_1"] = "gen-1"

	current := domain.NewState() // every row above is gone.

	fake := &countingTx{}
	if err := persistState(context.Background(), fake, current, loaded, nil); err != nil {
		t.Fatalf("persistState: %v", err)
	}

	counts, deletes := map[string]int{}, map[string]int{}
	for i, tbl := range fake.execTables {
		counts[tbl]++
		if fake.execKinds[i] == "DELETE" {
			deletes[tbl]++
		}
	}
	wantOneDelete := []string{"allocations", "allocation_keys", "holds", "operations", "operation_barriers", "idempotency_requests", "observations", "findings", "coverage"}
	for _, tbl := range wantOneDelete {
		if counts[tbl] != 1 {
			t.Errorf("table %s: got %d Exec calls for a row that no longer exists, want exactly 1", tbl, counts[tbl])
		}
		if deletes[tbl] != 1 {
			t.Errorf("table %s: the one statement issued was not a DELETE", tbl)
		}
	}
	if len(fake.execTables) != len(wantOneDelete) {
		t.Errorf("total Exec calls = %d, want %d (exactly one DELETE per table, nothing else)", len(fake.execTables), len(wantOneDelete))
	}
}

// allocation_keys is keyed by tenant and allocation key, while allocations
// is keyed by id. Swapping two keys in one Update used to leave the old
// derived rows behind (or hit allocations' unique key before a removed row
// was deleted). The loaded-state pass must clear both old identities before
// either new one is written, preserving the old full-rewrite result.
func TestPersistStateRekeysAllocationsBeforeWritingDerivedRows(t *testing.T) {
	loaded := domain.NewState()
	for id, key := range map[string]string{"alloc_1": "key_1", "alloc_2": "key_2"} {
		loaded.Allocations[id] = domain.Allocation{
			ID: id, TenantID: "tenant", DomainID: "domain", CIDR: "10.0.0.0/24",
			State: domain.Reserved, Request: domain.Request{AllocationKey: key},
		}
	}
	current := cloneState(loaded)
	for id, key := range map[string]string{"alloc_1": "key_2", "alloc_2": "key_1"} {
		a := current.Allocations[id]
		a.AllocationKey = key
		current.Allocations[id] = a
	}

	fake := &countingTx{}
	if err := persistState(context.Background(), fake, current, loaded, nil); err != nil {
		t.Fatalf("persistState: %v", err)
	}
	if len(fake.execKinds) != 12 {
		t.Fatalf("Exec calls = %d, want six deletes then six inserts: %v %v", len(fake.execKinds), fake.execKinds, fake.execTables)
	}
	counts := map[string]map[string]int{}
	for i, table := range fake.execTables {
		kind := fake.execKinds[i]
		if i < 6 && kind != "DELETE" || i >= 6 && kind != "INSERT" {
			t.Errorf("Exec %d: %s %s, want all old identities deleted before any insert", i, kind, table)
		}
		if counts[table] == nil {
			counts[table] = map[string]int{}
		}
		counts[table][kind]++
	}
	for _, table := range []string{"allocations", "allocation_keys", "holds"} {
		if counts[table]["DELETE"] != 2 || counts[table]["INSERT"] != 2 {
			t.Errorf("%s: got %v, want two deletes and two inserts", table, counts[table])
		}
	}
}
