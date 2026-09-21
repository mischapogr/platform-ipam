package storage

import (
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
	// View still loads the audit table, unlike Update below (package M4e).
	// Nothing in internal/service ever reads a View closure's st.Events -- the
	// callers that need the whole state (reserve's pre-check, List, Findings,
	// capacity, the worker's per-domain and per-operation scans) all read
	// Allocations, Operations, Requests, Observations or Findings, never
	// Events -- so this eager load costs a View closure nothing it uses today.
	// It stays because View never writes (persistState is never called from a
	// View transaction, so there is no write-amplification term for it to
	// remove) and because the opt-in Postgres integration tests read audit
	// history back through exactly this method
	// (TestPostgresAuditEventsRemainAppendOnly and the two
	// drops-its-rows-and-frees-its-key tests): keeping View's eager load
	// unchanged is what lets those tests pass without being touched.
	s, _, err := loadState(ctx, tx, true)
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
	// Update does not load the audit table (package M4e). Every reader of
	// State.Events in internal/service and internal/transport was checked
	// (grepping every ".Events" and "st.Events" site): the only production
	// uses are appends -- reserve's plan and commit closures
	// (service.go:366, service.go:435), Patch (service.go:602), finishBinding
	// (service.go:752), Release's release-requested event (service.go:802),
	// the worker's adoption-commit and reconciliation events (worker.go:260,
	// worker.go:670), abandon's fence event (abandon.go:187) and cancel's
	// fence event (cancel.go:165) -- and every one of them only appends to
	// st.Events; none reads its prior content to decide anything. There is no
	// transport endpoint or CLI verb that lists an allocation's events today
	// (grepped api/openapi.yaml, internal/transport and cmd/: no match), so
	// there is no production reader that would see an empty slice and
	// conclude wrongly. An Update closure that appends therefore starts from
	// an empty st.Events and ends with exactly the events it appended this
	// transaction -- nothing "loaded" ever needs filtering out, which is what
	// the empty loadedEvents set below both records and lets persistState
	// prove rather than assume.
	s, loadedEvents, err := loadState(ctx, tx, false)
	if err != nil {
		return err
	}
	if err = fn(s); err != nil {
		return err
	}
	if err = persistState(ctx, tx, s, loadedEvents); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ledger update: %w", err)
	}
	return nil
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// loadState decodes the ledger's tables into a *domain.State. includeEvents
// controls whether it also decodes audit_events (package M4e): View passes
// true, and Update passes false because no Update closure in this repository
// reads State.Events for anything but appending. The second return value is
// the set of audit event ids this call actually loaded -- empty when
// includeEvents is false -- carried beside the state for exactly one
// transaction and never stored globally, so persistState can tell an event a
// closure appended this transaction from one it merely re-offered because it
// was already sitting in State.Events when the closure started.
func loadState(ctx context.Context, q queryer, includeEvents bool) (*domain.State, map[string]struct{}, error) {
	s := domain.NewState()
	loadedEvents := map[string]struct{}{}
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
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	// package M4e: only View asks for the audit history (includeEvents=true).
	// An Update transaction skips this SELECT and its decode of every row in
	// the ledger's lifetime entirely -- that is the read-side half of the fix
	// (persistState below is the write-side half).
	if includeEvents {
		rows, err = q.Query(ctx, `SELECT id,payload FROM audit_events ORDER BY id`)
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
			var v domain.Event
			if err = json.Unmarshal(b, &v); err != nil {
				rows.Close()
				return nil, nil, err
			}
			s.Events = append(s.Events, v)
			loadedEvents[id] = struct{}{}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return s, loadedEvents, nil
}

// persistState writes s. loadedEvents is the set of audit event ids the
// transaction's loadState call already found durably present (package M4e);
// it may be nil or empty, which simply means every event in s.Events is
// treated as newly appended. Every other table keeps being rewritten in
// full -- that is M4f's cut, not this one.
func persistState(ctx context.Context, tx pgx.Tx, s *domain.State, loadedEvents map[string]struct{}) error {
	for _, table := range []string{"allocations", "allocation_keys", "operations", "idempotency_requests", "observations", "findings", "coverage", "holds", "operation_barriers"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	for id, v := range s.Allocations {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO allocations(id,tenant_id,allocation_key,committed,payload,created_at) VALUES($1,$2,$3,$4,$5,$6)`, id, v.TenantID, v.AllocationKey, v.Committed, b, v.CreatedAt); err != nil {
			return fmt.Errorf("persist allocation: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO allocation_keys(tenant_id,allocation_key,allocation_id,retired,payload) VALUES($1,$2,$3,$4,$5)`, v.TenantID, v.AllocationKey, id, v.State == domain.Released, b); err != nil {
			return fmt.Errorf("persist allocation key: %w", err)
		}
	}
	for id, v := range s.Operations {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operations(id,tenant_id,domain_id,payload) VALUES($1,$2,$3,$4)`, id, v.TenantID, v.DomainID, b); err != nil {
			return err
		}
	}
	for id, v := range s.Allocations {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO holds(hold_id,allocation_id,tenant_id,domain_id,cidr,committed,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, "hold_"+id, id, v.TenantID, v.DomainID, v.CIDR, v.Committed && v.State != domain.Released, b); err != nil {
			return fmt.Errorf("persist allocation hold: %w", err)
		}
	}
	for id, v := range s.Operations {
		if v.Status != "PENDING" {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operation_barriers(domain_id,operation_id,payload) VALUES($1,$2,$3) ON CONFLICT (domain_id) DO UPDATE SET operation_id=excluded.operation_id,payload=excluded.payload`, v.DomainID, id, b); err != nil {
			return fmt.Errorf("persist operation barrier: %w", err)
		}
	}
	for id, v := range s.Requests {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO idempotency_requests(request_id,tenant_id,method,path,request_key,request_hash,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, v.TenantID, v.Method, v.Path, v.Key, v.Hash, b); err != nil {
			return err
		}
	}
	for id, v := range s.Observations {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO observations(domain_id,payload) VALUES($1,$2)`, id, b); err != nil {
			return err
		}
	}
	for id, v := range s.Findings {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO findings(id,tenant_id,domain_id,payload) VALUES($1,$2,$3,$4)`, id, v.TenantID, v.DomainID, b); err != nil {
			return err
		}
	}
	for id, v := range s.Coverage {
		if _, err := tx.Exec(ctx, `INSERT INTO coverage(domain_id,generation) VALUES($1,$2)`, id, v); err != nil {
			return err
		}
	}
	// audit_events is never in the DELETE list above -- that omission plus
	// the ON CONFLICT DO NOTHING below are what make the table append-only
	// (TestPostgresAuditEventsRemainAppendOnly). Before package M4e this loop
	// re-offered every event loadState had just read back, one INSERT per
	// historical row on every single Update; now it inserts only the events
	// this transaction's closure actually appended -- the ones whose ids are
	// not in loadedEvents, i.e. were not already known durable.
	// ON CONFLICT DO NOTHING stays as the brace: if loadedEvents is wrong or
	// stale for any reason, re-offering a row that already exists is a no-op,
	// never a silent overwrite, so this loop fails towards writing one row
	// too many rather than losing one.
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
