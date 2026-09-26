package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// PostgresLedger is the durable implementation of domain.Ledger. The first
// implementation deliberately takes one transaction advisory lock for every
// operation. This is a correctness-first coordination boundary; it can later
// be replaced by per-domain fencing after contention is measured. Pending
// operations and Allocation.InventorySync are the durable projection queue;
// an external outbox is intentionally not claimed until a delivery worker and
// retry/ack semantics are implemented.
type PostgresLedger struct{ pool *pgxpool.Pool }

func NewPostgresLedger(ctx context.Context, dsn string) (*PostgresLedger, error) {
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open platform ledger: %w", err)
	}
	l := &PostgresLedger{pool: p}
	return l, nil
}

// NewPostgres is kept as a concise constructor for process wiring.
func NewPostgres(ctx context.Context, dsn string) (*PostgresLedger, error) {
	return NewPostgresLedger(ctx, dsn)
}

// Open is the process-wiring name. Schema changes are explicit and belong to
// the deployment migration job; callers should invoke l.Migrate separately.
func Open(ctx context.Context, dsn string) (*PostgresLedger, error) {
	return NewPostgresLedger(ctx, dsn)
}

// Migrate opens a short-lived pool for an explicit migration job.
func Migrate(ctx context.Context, dsn string) error {
	l, err := NewPostgresLedger(ctx, dsn)
	if err != nil {
		return err
	}
	defer l.Close()
	return l.Migrate(ctx)
}

func NewPostgresLedgerWithPool(pool *pgxpool.Pool) *PostgresLedger {
	return &PostgresLedger{pool: pool}
}
func (l *PostgresLedger) Close() {
	if l != nil && l.pool != nil {
		l.pool.Close()
	}
}

func (l *PostgresLedger) Ready(ctx context.Context) error {
	if l == nil || l.pool == nil {
		return fmt.Errorf("platform ledger is not configured")
	}
	var n int
	return l.pool.QueryRow(ctx, `SELECT 1 FROM platform_ledger_schema WHERE version=1`).Scan(&n)
}

func (l *PostgresLedger) Migrate(ctx context.Context) error {
	if l == nil || l.pool == nil {
		return fmt.Errorf("platform ledger is not configured")
	}
	_, err := l.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS platform_ledger_schema (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS allocations (
  id text PRIMARY KEY, tenant_id text NOT NULL, allocation_key text NOT NULL,
  committed boolean NOT NULL, payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, allocation_key)
);
CREATE TABLE IF NOT EXISTS allocation_keys (
  tenant_id text NOT NULL, allocation_key text NOT NULL, allocation_id text NOT NULL,
  retired boolean NOT NULL DEFAULT false, payload jsonb NOT NULL, PRIMARY KEY (tenant_id, allocation_key)
);
CREATE TABLE IF NOT EXISTS operations (id text PRIMARY KEY, tenant_id text NOT NULL, domain_id text NOT NULL, payload jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS idempotency_requests (request_id text PRIMARY KEY, tenant_id text NOT NULL, method text NOT NULL, path text NOT NULL, request_key text NOT NULL, request_hash text NOT NULL, payload jsonb NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS idempotency_identity ON idempotency_requests (tenant_id, method, path, request_key);
CREATE TABLE IF NOT EXISTS observations (domain_id text PRIMARY KEY, payload jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS findings (id text PRIMARY KEY, tenant_id text NOT NULL, domain_id text NOT NULL, payload jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS coverage (domain_id text PRIMARY KEY, generation text NOT NULL);
CREATE TABLE IF NOT EXISTS audit_events (id text PRIMARY KEY, allocation_id text NOT NULL, tenant_id text NOT NULL, payload jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS holds (
  hold_id text PRIMARY KEY, allocation_id text NOT NULL, tenant_id text NOT NULL,
  domain_id text NOT NULL, cidr cidr NOT NULL, committed boolean NOT NULL, payload jsonb NOT NULL
);
CREATE TABLE IF NOT EXISTS operation_barriers (
  domain_id text PRIMARY KEY, operation_id text NOT NULL, payload jsonb NOT NULL
);
INSERT INTO platform_ledger_schema(version) VALUES (1) ON CONFLICT DO NOTHING;
`)
	if err != nil {
		return fmt.Errorf("migrate platform ledger: %w", err)
	}
	return nil
}

const coordinationLock int64 = 0x706c6174666f726d // "platform"

func (l *PostgresLedger) View(ctx context.Context, fn func(*domain.State) error) error {
	if l == nil || l.pool == nil {
		return fmt.Errorf("platform ledger is not configured")
	}
	// The advisory lock is the initial global coordination fence. READ COMMITTED
	// takes its snapshot after a waiting transaction acquires that lock, so a
	// contender always loads the committed state from its predecessor.
	tx, err := l.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin ledger read: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, coordinationLock); err != nil {
		return fmt.Errorf("lock ledger read: %w", err)
	}
	// package M4f: View stops decoding audit_events too, the same way Update
	// already stopped (package M4e). Every production View closure in
	// internal/service was re-checked and none reads State.Events -- the
	// callers that need the whole state (reserve's pre-check, List, Findings,
	// capacity, the worker's per-domain and per-operation scans) all read
	// Allocations, Operations, Requests, Observations or Findings, never
	// Events. The opt-in PostgreSQL tests that used to read audit history
	// back through View (TestPostgresAuditEventsRemainAppendOnly and the two
	// drops-its-rows-and-frees-its-key tests) now call the additive
	// domain.Ledger.Events method below instead, which is the ledger's only
	// remaining reader of audit_events. View also never needs the
	// loaded-snapshot bookkeeping Update carries for its diff (below): View
	// never calls persistState, so there is no write-amplification term for
	// it to remove and nothing to diff against.
	s, _, err := loadState(ctx, tx, false)
	if err != nil {
		return err
	}
	if err = fn(s); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (l *PostgresLedger) Update(ctx context.Context, fn func(*domain.State) error) error {
	if l == nil || l.pool == nil {
		return fmt.Errorf("platform ledger is not configured")
	}
	tx, err := l.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin ledger update: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, coordinationLock); err != nil {
		return fmt.Errorf("lock ledger update: %w", err)
	}
	// Update does not load the audit table (package M4e); package M4f gives
	// it the same treatment for the other eight -- it retains a snapshot of
	// what loadState read (loaded, below) so persistState can persist only
	// what the closure actually changed instead of rewriting the ledger.
	// Every reader of State.Events in internal/service and internal/transport
	// was checked (grepping every ".Events" and "st.Events" site): the only
	// production uses are appends -- reserve's plan and commit closures
	// (service.go:366, service.go:435), Patch (service.go:602), finishBinding
	// (service.go:752), Release's release-requested event (service.go:802),
	// the worker's adoption-commit and reconciliation events (worker.go:260,
	// worker.go:670), abandon's fence event (abandon.go:187) and cancel's
	// fence event (cancel.go:165) -- and every one of them only appends to
	// st.Events; none reads its prior content to decide anything. An Update
	// closure that appends therefore starts from an empty st.Events and ends
	// with exactly the events it appended this transaction -- nothing
	// "loaded" ever needs filtering out for the audit table, which is why the
	// last argument to persistState below is always nil from here.
	s, loaded, err := loadState(ctx, tx, true)
	if err != nil {
		return err
	}
	if err = fn(s); err != nil {
		return err
	}
	if err = persistState(ctx, tx, s, loaded, nil); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ledger update: %w", err)
	}
	return nil
}

// Events returns allocationID's full audit history, ordered by when each
// event happened (At ascending), with the event id as a deterministic
// tiebreaker for two events recorded at the same instant -- the order a
// caller reading "the history" actually wants, unlike the pre-M4f
// loadState's arbitrary `ORDER BY id`, which carried no temporal meaning and
// which nothing ever read as an ordered sequence (M4c's differential harness
// had to sort around it for exactly that reason; see its doc comment).
//
// It is the one place this store still decodes audit_events on demand, and
// deliberately its own standalone query outside any View or Update
// transaction and outside the advisory lock: it never has to agree with an
// in-flight Update, because it only ever answers "what has this allocation's
// audit trail recorded so far", a point-in-time question a lock cannot make
// more correct, and every production writer of State.Events only appends
// (see Update's doc comment above), so there is nothing a concurrent
// transaction could do to this allocation's history that a lock would need
// to hide mid-read.
func (l *PostgresLedger) Events(ctx context.Context, allocationID string) ([]domain.Event, error) {
	if l == nil || l.pool == nil {
		return nil, fmt.Errorf("platform ledger is not configured")
	}
	rows, err := l.pool.Query(ctx, `SELECT id,payload FROM audit_events WHERE allocation_id=$1`, allocationID)
	if err != nil {
		return nil, fmt.Errorf("query allocation events: %w", err)
	}
	defer rows.Close()
	var events []domain.Event
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		var v domain.Event
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("decode audit event %s: %w", id, err)
		}
		events = append(events, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortEvents(events)
	return events, nil
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// loadState decodes the ledger's six non-derived tables (everything but the
// write-only allocation_keys/holds/operation_barriers, and, since package
// M4f, audit_events -- see Events above) into a *domain.State.
//
// forUpdate controls whether loadState also builds and returns a SECOND,
// fully independent *domain.State -- the "loaded" snapshot persistState's
// diff (below) compares the closure's result against. View passes false: a
// View closure never mutates anything persistState would need to diff, so
// building a snapshot nothing will read would only cost memory and an extra
// decode. Update passes true.
//
// The loaded snapshot is built by decoding each row's bytes a SECOND time
// into its own value, rather than by copying the value already decoded into
// s. This is deliberate, not an oversight: domain.Allocation,
// domain.Operation and domain.Idempotency all carry pointer or map/slice
// fields (Allocation.Binding, Operation.Result, Operation.Candidate,
// Operation.Adoption, the []domain.Observation slice), and Go copies a
// struct's reference-typed fields by reference, not by value. If the loaded
// snapshot shared those inner maps/slices/pointers with s, a closure that
// mutates one in place (the only legal way to change a map field of a
// map-of-structs entry: read the struct out, mutate its map, write the
// struct back -- e.g. an operation's Result map) would silently corrupt the
// "before" picture through the very reference persistState is about to
// compare it against. Two independent Unmarshal calls over the same bytes
// produce two object graphs that share nothing mutable, at the cost of one
// extra decode per row -- paid only on Update, and dwarfed by the round trips
// this package removes.
func loadState(ctx context.Context, q queryer, forUpdate bool) (*domain.State, *domain.State, error) {
	s := domain.NewState()
	var loaded *domain.State
	if forUpdate {
		loaded = domain.NewState()
	}

	rows, err := q.Query(ctx, `SELECT id,payload FROM allocations`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var b []byte
		if err = rows.Scan(&id, &b); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var v domain.Allocation
		if err = json.Unmarshal(b, &v); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("decode allocation %s: %w", id, err)
		}
		s.Allocations[id] = v
		if forUpdate {
			var lv domain.Allocation
			if err = json.Unmarshal(b, &lv); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("decode allocation %s (loaded snapshot): %w", id, err)
			}
			loaded.Allocations[id] = lv
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = q.Query(ctx, `SELECT id,payload FROM operations`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var b []byte
		if err = rows.Scan(&id, &b); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var v domain.Operation
		if err = json.Unmarshal(b, &v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		s.Operations[id] = v
		if forUpdate {
			var lv domain.Operation
			if err = json.Unmarshal(b, &lv); err != nil {
				rows.Close()
				return nil, nil, err
			}
			loaded.Operations[id] = lv
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = q.Query(ctx, `SELECT request_id,payload FROM idempotency_requests`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var b []byte
		if err = rows.Scan(&id, &b); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var v domain.Idempotency
		if err = json.Unmarshal(b, &v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		s.Requests[id] = v
		if forUpdate {
			var lv domain.Idempotency
			if err = json.Unmarshal(b, &lv); err != nil {
				rows.Close()
				return nil, nil, err
			}
			loaded.Requests[id] = lv
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = q.Query(ctx, `SELECT domain_id,payload FROM observations`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var b []byte
		if err = rows.Scan(&id, &b); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var v []domain.Observation
		if err = json.Unmarshal(b, &v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		s.Observations[id] = v
		if forUpdate {
			var lv []domain.Observation
			if err = json.Unmarshal(b, &lv); err != nil {
				rows.Close()
				return nil, nil, err
			}
			loaded.Observations[id] = lv
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = q.Query(ctx, `SELECT id,payload FROM findings`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var b []byte
		if err = rows.Scan(&id, &b); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var v domain.Finding
		if err = json.Unmarshal(b, &v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		s.Findings[id] = v
		if forUpdate {
			var lv domain.Finding
			if err = json.Unmarshal(b, &lv); err != nil {
				rows.Close()
				return nil, nil, err
			}
			loaded.Findings[id] = lv
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = q.Query(ctx, `SELECT domain_id,generation FROM coverage`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id, gen string
		if err = rows.Scan(&id, &gen); err != nil {
			rows.Close()
			return nil, nil, err
		}
		s.Coverage[id] = gen
		if forUpdate {
			loaded.Coverage[id] = gen // a plain text column: no JSON round trip involved, so no second decode is needed for independence.
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}

	return s, loaded, nil
}

// pendingBarrierDomains derives, from a set of operations, exactly the row
// set persistState's operation_barriers write has always derived: one entry
// per domain_id that has at least one PENDING operation, keyed by that
// operation's id. It is applied twice -- once to loadState's retained
// "loaded" operations snapshot and once to the closure's own result -- so
// that operation_barriers, a table nothing in this repository reads back
// (ADR 0017), can still be diffed without loadState ever issuing a tenth
// SELECT against it: its previous content is fully determined by the
// operations this transaction already loaded, by the same formula that
// derives its next content from the operations the closure leaves behind.
// If more than one PENDING operation ever named the same domain (fencing is
// supposed to prevent this; nothing here re-verifies it), map iteration
// picks whichever one wins arbitrarily -- exactly persistState's pre-M4f
// behaviour via `ON CONFLICT (domain_id) DO UPDATE`, not a new risk.
func pendingBarrierDomains(ops map[string]domain.Operation) map[string]string {
	out := map[string]string{}
	for id, v := range ops {
		if v.Status != "PENDING" {
			continue
		}
		out[v.DomainID] = id
	}
	return out
}

// persistState writes only what changed between loaded (loadState's
// retained "before" snapshot, nil for a transaction that never diffs -- see
// the two audit-only unit tests in measure_update_test.go) and s (the
// closure's result), one INSERT/UPDATE/DELETE per row that actually differs
// instead of a full rewrite. loadedEvents is unrelated and unchanged from
// package M4e: the set of audit event ids the transaction's loadState call
// already found durably present, always nil from a real Update since
// package M4f (Update no longer loads audit_events at all -- see above), and
// otherwise as package M4e's two dedicated tests exercise it directly.
//
// "Did this row change" is a byte comparison of json.Marshal(loaded value)
// against json.Marshal(current value) -- the ADR's own mechanism ("it
// already marshals every entity to JSON in order to write it"), NOT a
// comparison against the raw bytes loadState originally read off the wire.
// That distinction matters and is not cosmetic: every payload column here is
// `jsonb`, and PostgreSQL's jsonb storage re-serializes on every read --
// object keys sorted alphabetically at every nesting level, whitespace
// normalised -- which does NOT match encoding/json.Marshal's struct-field-
// order output for anything with more than one field. Comparing the raw
// bytes loadState scanned off the wire against a fresh Marshal would make
// nearly every unchanged multi-field row look "changed" (verified against a
// throw-away postgres:17.5-alpine: `{"z":1,"a":2}` round-trips through a
// jsonb column as `{"a": 2, "z": 1}`), silently defeating the entire
// optimisation while still being safe (a spurious UPDATE, never a lost
// write) -- exactly the kind of failure this record asks to be stated
// rather than discovered in a benchmark. Comparing two Marshal outputs
// produced by this process, one from the "loaded" snapshot's independently
// decoded value and one from the closure's result, sidesteps jsonb's
// canonicalisation entirely because neither side of the comparison ever
// passes through it.
//
// Deletion is computed from key-set membership alone -- a key present in
// loaded and absent from s -- never from a byte comparison, exactly as the
// record requires: byte-comparison failure modes are all "write something
// unnecessary", key-set failure modes are the only ones that can lose a row.
func persistState(ctx context.Context, tx pgx.Tx, s *domain.State, loaded *domain.State, loadedEvents map[string]struct{}) error {
	if loaded == nil {
		loaded = domain.NewState() // every map below reads as empty; see the two package-M4e-only tests that pass nil.
	}

	// --- allocations, and its two derived, write-only tables. -------------
	//
	// allocation_keys and holds are not independently diffed against a
	// loaded snapshot of their own: package M4f chose NOT to add the two
	// SELECTs that would take (loadState never reads them, and nothing in
	// this repository reads them back either -- ADR 0017). Instead, both are
	// recomputed from an allocation exactly as persistState always has, and
	// this record proves that recomputation is 1:1 with the allocation's OWN
	// added/changed/removed classification: allocation_keys' and holds'
	// entire content (payload, retired, committed) is a pure function of one
	// allocation's current value and nothing else, so "did this allocation's
	// row change" already answers "did its derived rows change", for both
	// tables, without any further comparison. allocation_keys' real primary
	// key is (tenant_id, allocation_key), not allocation_id. Production
	// keeps those fields stable, but a whole-state closure can change them.
	// Remove the old allocation and both derived rows before writing a
	// changed identity: otherwise an upsert under the new composite key
	// would leave the old allocation_keys row behind. The same first pass
	// removes deleted allocations, so a new key can replace one the closure
	// removed without depending on Go map iteration order.
	// holds' primary key, hold_id, is always exactly "hold_"+allocationID, so
	// it needs no lookup at all, loaded or otherwise.
	for id, old := range loaded.Allocations {
		current, exists := s.Allocations[id]
		if exists && old.TenantID == current.TenantID && old.AllocationKey == current.AllocationKey {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM allocations WHERE id=$1`, id); err != nil {
			return fmt.Errorf("delete allocation: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM allocation_keys WHERE allocation_id=$1`, id); err != nil {
			return fmt.Errorf("delete allocation key: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM holds WHERE hold_id=$1`, "hold_"+id); err != nil {
			return fmt.Errorf("delete allocation hold: %w", err)
		}
	}
	for id, v := range s.Allocations {
		lv, existed := loaded.Allocations[id]
		if existed {
			lb, err := json.Marshal(lv)
			if err != nil {
				return err
			}
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if bytes.Equal(lb, b) {
				continue // unchanged: not allocations, not allocation_keys, not holds.
			}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO allocations(id,tenant_id,allocation_key,committed,payload,created_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO UPDATE SET tenant_id=excluded.tenant_id,allocation_key=excluded.allocation_key,committed=excluded.committed,payload=excluded.payload,created_at=excluded.created_at`,
			id, v.TenantID, v.AllocationKey, v.Committed, b, v.CreatedAt); err != nil {
			return fmt.Errorf("persist allocation: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO allocation_keys(tenant_id,allocation_key,allocation_id,retired,payload) VALUES($1,$2,$3,$4,$5) ON CONFLICT (tenant_id,allocation_key) DO UPDATE SET allocation_id=excluded.allocation_id,retired=excluded.retired,payload=excluded.payload`,
			v.TenantID, v.AllocationKey, id, v.State == domain.Released, b); err != nil {
			return fmt.Errorf("persist allocation key: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO holds(hold_id,allocation_id,tenant_id,domain_id,cidr,committed,payload) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (hold_id) DO UPDATE SET allocation_id=excluded.allocation_id,tenant_id=excluded.tenant_id,domain_id=excluded.domain_id,cidr=excluded.cidr,committed=excluded.committed,payload=excluded.payload`,
			"hold_"+id, id, v.TenantID, v.DomainID, v.CIDR, v.Committed && v.State != domain.Released, b); err != nil {
			return fmt.Errorf("persist allocation hold: %w", err)
		}
	}
	// --- operations, and the operation_barriers table derived from them. --
	//
	// operation_barriers IS diffed against a loaded picture of its own
	// (pendingBarrierDomains applied to loaded.Operations, above s.Operations
	// after the closure ran), because unlike allocation_keys/holds its key
	// space (domain_id) is not 1:1 with the parent table's key (operation
	// id): a domain's barrier can survive one pending operation being
	// succeeded and another beginning. That "loaded" picture costs no extra
	// SELECT either, because loadState already retained the operations it
	// read.
	for id, v := range s.Operations {
		lv, existed := loaded.Operations[id]
		if existed {
			lb, err := json.Marshal(lv)
			if err != nil {
				return err
			}
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if bytes.Equal(lb, b) {
				continue
			}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operations(id,tenant_id,domain_id,payload) VALUES($1,$2,$3,$4) ON CONFLICT (id) DO UPDATE SET tenant_id=excluded.tenant_id,domain_id=excluded.domain_id,payload=excluded.payload`,
			id, v.TenantID, v.DomainID, b); err != nil {
			return fmt.Errorf("persist operation: %w", err)
		}
	}
	for id := range loaded.Operations {
		if _, ok := s.Operations[id]; ok {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM operations WHERE id=$1`, id); err != nil {
			return fmt.Errorf("delete operation: %w", err)
		}
	}

	loadedBarriers := pendingBarrierDomains(loaded.Operations)
	currentBarriers := pendingBarrierDomains(s.Operations)
	for dom, opID := range currentBarriers {
		loadedOpID, existed := loadedBarriers[dom]
		if existed && loadedOpID == opID {
			// Same winning operation as before: the barrier row changed only
			// if that operation's own row changed, which the operations loop
			// above already decided -- ask loaded.Operations/s.Operations
			// directly rather than re-deriving it.
			lv, lok := loaded.Operations[opID]
			cv := s.Operations[opID]
			if lok {
				lb, err := json.Marshal(lv)
				if err != nil {
					return err
				}
				b, err := json.Marshal(cv)
				if err != nil {
					return err
				}
				if bytes.Equal(lb, b) {
					continue // unchanged winning operation: no barrier write.
				}
			}
		}
		v := s.Operations[opID]
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operation_barriers(domain_id,operation_id,payload) VALUES($1,$2,$3) ON CONFLICT (domain_id) DO UPDATE SET operation_id=excluded.operation_id,payload=excluded.payload`,
			dom, opID, b); err != nil {
			return fmt.Errorf("persist operation barrier: %w", err)
		}
	}
	for dom := range loadedBarriers {
		if _, ok := currentBarriers[dom]; ok {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM operation_barriers WHERE domain_id=$1`, dom); err != nil {
			return fmt.Errorf("delete operation barrier: %w", err)
		}
	}

	// --- idempotency_requests. ---------------------------------------------
	for id, v := range s.Requests {
		lv, existed := loaded.Requests[id]
		if existed {
			lb, err := json.Marshal(lv)
			if err != nil {
				return err
			}
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if bytes.Equal(lb, b) {
				continue
			}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO idempotency_requests(request_id,tenant_id,method,path,request_key,request_hash,payload) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (request_id) DO UPDATE SET tenant_id=excluded.tenant_id,method=excluded.method,path=excluded.path,request_key=excluded.request_key,request_hash=excluded.request_hash,payload=excluded.payload`,
			id, v.TenantID, v.Method, v.Path, v.Key, v.Hash, b); err != nil {
			return fmt.Errorf("persist idempotency request: %w", err)
		}
	}
	for id := range loaded.Requests {
		if _, ok := s.Requests[id]; ok {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM idempotency_requests WHERE request_id=$1`, id); err != nil {
			return fmt.Errorf("delete idempotency request: %w", err)
		}
	}

	// --- observations. ------------------------------------------------------
	for id, v := range s.Observations {
		lv, existed := loaded.Observations[id]
		if existed {
			lb, err := json.Marshal(lv)
			if err != nil {
				return err
			}
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if bytes.Equal(lb, b) {
				continue
			}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO observations(domain_id,payload) VALUES($1,$2) ON CONFLICT (domain_id) DO UPDATE SET payload=excluded.payload`, id, b); err != nil {
			return fmt.Errorf("persist observations: %w", err)
		}
	}
	for id := range loaded.Observations {
		if _, ok := s.Observations[id]; ok {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM observations WHERE domain_id=$1`, id); err != nil {
			return fmt.Errorf("delete observations: %w", err)
		}
	}

	// --- findings. ------------------------------------------------------
	for id, v := range s.Findings {
		lv, existed := loaded.Findings[id]
		if existed {
			lb, err := json.Marshal(lv)
			if err != nil {
				return err
			}
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if bytes.Equal(lb, b) {
				continue
			}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO findings(id,tenant_id,domain_id,payload) VALUES($1,$2,$3,$4) ON CONFLICT (id) DO UPDATE SET tenant_id=excluded.tenant_id,domain_id=excluded.domain_id,payload=excluded.payload`,
			id, v.TenantID, v.DomainID, b); err != nil {
			return fmt.Errorf("persist finding: %w", err)
		}
	}
	for id := range loaded.Findings {
		if _, ok := s.Findings[id]; ok {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM findings WHERE id=$1`, id); err != nil {
			return fmt.Errorf("delete finding: %w", err)
		}
	}

	// --- coverage: a plain text column, no JSON involved at either end. --
	for id, v := range s.Coverage {
		if lv, existed := loaded.Coverage[id]; existed && lv == v {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO coverage(domain_id,generation) VALUES($1,$2) ON CONFLICT (domain_id) DO UPDATE SET generation=excluded.generation`, id, v); err != nil {
			return fmt.Errorf("persist coverage: %w", err)
		}
	}
	for id := range loaded.Coverage {
		if _, ok := s.Coverage[id]; ok {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM coverage WHERE domain_id=$1`, id); err != nil {
			return fmt.Errorf("delete coverage: %w", err)
		}
	}

	// audit_events is never cleared and never diffed like the tables above --
	// that is package M4e's decision, unchanged here. It is append-only by
	// omission from every DELETE loop plus ON CONFLICT DO NOTHING below: a
	// closure cannot erase persisted history by presenting a state that no
	// longer contains an old event (TestPostgresAuditEventsRemainAppendOnly).
	// Before package M4e this loop re-offered every event loadState had just
	// read back; since package M4e (and unchanged by M4f) it inserts only the
	// events this transaction's closure actually appended -- the ones whose
	// ids are not in loadedEvents, i.e. were not already known durable, which
	// for a real Update is every event in s.Events, because Update's loadState
	// call never populates loadedEvents at all (see above). ON CONFLICT DO
	// NOTHING stays as the brace: if loadedEvents is ever wrong or stale, a
	// re-offered row that already exists is a no-op, never a silent
	// overwrite -- this loop fails towards writing one row too many, never
	// towards losing one.
	for i := range s.Events {
		if s.Events[i].ID == "" {
			s.Events[i].ID = domain.NewID("evt")
		}
		v := s.Events[i]
		if _, already := loadedEvents[v.ID]; already {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(id,allocation_id,tenant_id,payload) VALUES($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING`, v.ID, v.AllocationID, v.TenantID, b); err != nil {
			return err
		}
	}
	return nil
}
