package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/config"
	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

type pendingInventory struct{}

func (pendingInventory) Snapshot(context.Context, domain.Domain) (domain.InventorySnapshot, error) {
	return domain.InventorySnapshot{Complete: true}, nil
}
func (pendingInventory) Ensure(context.Context, domain.Allocation, string) (string, error) {
	return "", context.DeadlineExceeded
}

// Adoption is an operator process mode and never an endpoint (ADR 0010), so no
// request this boundary serves may reach Adopt. Refusing rather than returning
// an ID is what makes a future route that did so fail a test here.
func (pendingInventory) Adopt(context.Context, domain.Allocation, string) (string, error) {
	return "", errors.New("adopt not configured")
}

// Abandoning an adoption is a third subcommand of an operator process mode and
// never an endpoint (ADR 0012), so no request this boundary serves may reach
// it. Refusing is what makes a future route that did so fail a test here.
func (pendingInventory) AbandonAdoption(context.Context, domain.Allocation, string, domain.PriorInventory) error {
	return errors.New("abandon not configured")
}

// Cancelling a reservation is a tenant's own write, and package H8c wires it
// to the one route ADR 0013 permits: DELETE /v1/operations/{operation_id}.
// Unlike Adopt and AbandonAdoption above -- which no request this transport
// boundary serves may ever reach, because they are an operator process
// mode's alone -- CancelReservation is legitimately transport-reachable, so
// this fake behaves like its no-op siblings Sync and Delete: nothing in this
// boundary's fixtures ever marks a prefix, so there is nothing to remove.
func (pendingInventory) CancelReservation(context.Context, domain.Allocation, string) error {
	return nil
}
func (pendingInventory) Sync(context.Context, domain.Allocation) error   { return nil }
func (pendingInventory) Delete(context.Context, domain.Allocation) error { return nil }

type completeObserver struct{}

func (completeObserver) Observe(_ context.Context, d domain.Domain) (domain.Observation, error) {
	now := time.Now().UTC()
	return domain.Observation{DomainID: d.ID, Generation: d.CoverageGeneration, Complete: true, StartedAt: now, FinishedAt: now}, nil
}

const testToken = "this-is-a-test-token-with-enough-entropy-for-local-tests"

func boundary(t *testing.T) (http.Handler, *storage.MemoryLedger) {
	t.Helper()
	cfg, err := config.Load("../../examples/config/pools.yaml", "", "development")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identities = []domain.Principal{{Subject: "developer", TenantID: "orders-team", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}}}
	auth, err := NewAuth(context.Background(), config.Settings{Environment: "development", AuthMode: "local", LocalToken: testToken, LocalSubject: "developer"}, cfg.Identities)
	if err != nil {
		t.Fatal(err)
	}
	ledger := storage.NewMemoryLedger()
	app := service.New(cfg, ledger, pendingInventory{}, completeObserver{})
	return New(app, auth, ledger, cfg).Handler(), ledger
}
func call(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "http-test-reservation")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const reservation = `{"allocation_key":"transport-test","scope":"vpc","environment":"prod","region":"eu-central-1","account_id":"123456789012","prefix_length":20}`

func TestPendingResponseNeverExposesCIDR(t *testing.T) {
	h, _ := boundary(t)
	w := call(h, "POST", "/v1/allocations", reservation, testToken)
	if w.Code != 202 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "PENDING" || body["cidr"] != nil || body["tenant_id"] != nil || !strings.HasPrefix(w.Header().Get("Location"), "/v1/operations/op_") {
		t.Fatalf("uncommitted data exposed: %v", body)
	}
}
func TestRequestSecurityBoundary(t *testing.T) {
	h, ledger := boundary(t)
	cases := []struct {
		body, token string
		status      int
	}{{reservation, "wrong", 401}, {strings.TrimSuffix(reservation, "}") + `,"tenant_id":"attacker"}`, testToken, 422}, {strings.TrimSuffix(reservation, "}") + `,"description":null}`, testToken, 422}, {strings.TrimSuffix(reservation, "}") + `,"labels":{"x":null}}`, testToken, 422}, {strings.TrimSuffix(reservation, "}") + `,"scope":"subnet"}`, testToken, 422}}
	for _, tc := range cases {
		w := call(h, "POST", "/v1/allocations", tc.body, tc.token)
		if w.Code != tc.status {
			t.Errorf("status=%d expected%d body%s", w.Code, tc.status, w.Body)
		}
		if w.Header().Get("X-Request-ID") == "" {
			t.Error("missing requestID")
		}
	}
	if err := ledger.View(context.Background(), func(s *domain.State) error {
		if len(s.Allocations) > 0 {
			t.Error("rejected input mutated ledger")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestTenantAndInternalFields(t *testing.T) {
	h, ledger := boundary(t)
	now := time.Now().UTC()
	a := domain.Allocation{ID: "alloc_other", TenantID: "other", Committed: true, CreatedAt: now}
	if err := ledger.Update(context.Background(), func(s *domain.State) error { s.Allocations[a.ID] = a; return nil }); err != nil {
		t.Fatal(err)
	}
	w := call(h, "GET", "/v1/allocations/alloc_other", "", testToken)
	if w.Code != 404 {
		t.Fatalf("cross tenant status%d", w.Code)
	}
	w = call(h, "GET", "/v1/allocations", "", testToken)
	if strings.Contains(w.Body.String(), "alloc_other") {
		t.Fatal("cross tenant list leak")
	}
}

// TestGetAnUncommittedPendingAllocationRendersAllocationPending is package
// E3's transport-level proof that internal/service.pendingAllocationError
// reaches the wire exactly as internal/transport/http.go's failure() renders
// any other *domain.APIError -- no transport code changed for this package,
// so this test is the demonstration that none needed to: retryable, details,
// and code all flow through the same envelope every other error uses.
func TestGetAnUncommittedPendingAllocationRendersAllocationPending(t *testing.T) {
	h, ledger := boundary(t)
	now := time.Now().UTC()
	allocationID, operationID := "alloc_pending_read", "op_pending_read"
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		st.Allocations[allocationID] = domain.Allocation{
			ID: allocationID, TenantID: "orders-team", Committed: false, State: domain.Reserved,
			Request:   domain.Request{AllocationKey: "pending-read", Scope: "vpc", Environment: "prod", Region: "eu-central-1", AccountID: "123456789012", PrefixLength: 22},
			CreatedAt: now, UpdatedAt: now,
		}
		st.Operations[operationID] = domain.Operation{ID: operationID, Type: "RESERVE", Status: "PENDING", AllocationID: allocationID, TenantID: "orders-team", CreatedAt: now, UpdatedAt: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := call(h, "GET", "/v1/allocations/"+allocationID, "", testToken)
	if w.Code != 409 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var envelope struct {
		Error struct {
			Code      string         `json:"code"`
			Message   string         `json:"message"`
			Retryable bool           `json:"retryable"`
			Details   map[string]any `json:"details"`
			RequestID string         `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "allocation_pending" {
		t.Fatalf("code=%q, want allocation_pending", envelope.Error.Code)
	}
	if !envelope.Error.Retryable {
		t.Fatal("allocation_pending must render retryable:true")
	}
	if envelope.Error.Details["operation_id"] != operationID {
		t.Fatalf("details=%#v, want operation_id=%q", envelope.Error.Details, operationID)
	}
	if envelope.Error.RequestID == "" {
		t.Fatal("missing request_id, required on every error the same as any other")
	}
	// This is a durable-state conflict, not a transient one: unlike 503/429,
	// no Retry-After header is set (internal/transport/http.go's failure()
	// only sets it for those two statuses).
	if w.Header().Get("Retry-After") != "" {
		t.Fatalf("Retry-After=%q, want none for 409", w.Header().Get("Retry-After"))
	}
}
func TestPublicAllocationHidesAdapterState(t *testing.T) {
	s := New(nil, nil, nil, domain.Config{})
	a := domain.Allocation{ID: "alloc_one", TenantID: "secret", DomainID: "internal", InventoryID: "123", RequestHash: "hash", State: domain.Reserved}
	v := s.publicAllocation(a, domain.Principal{})
	for _, key := range []string{"tenant_id", "domain_id", "inventory_id", "request_hash", "committed"} {
		if _, ok := v[key]; ok {
			t.Fatal("internal field escaped", key)
		}
	}
	// ADR 0011 stage two / package G3b4: an operator's projection gains
	// exactly one new key, tenant_id, carrying the allocation's real tenant --
	// every other internal field (inventory id, request hash, the committed
	// flag, domain id) stays hidden from an operator too.
	ov := s.publicAllocation(a, domain.Principal{Role: domain.OperatorRole})
	if ov["tenant_id"] != "secret" {
		t.Fatalf("operator tenant_id=%v, want the allocation's real tenant", ov["tenant_id"])
	}
	for _, key := range []string{"domain_id", "inventory_id", "request_hash", "committed"} {
		if _, ok := ov[key]; ok {
			t.Fatal("internal field escaped to an operator", key)
		}
	}
}

// noPoolBoundary mirrors boundary(t) but authenticates a principal whose
// tenant ("ops-team") does not appear in any pool's eligible_tenants in
// ../../examples/config/pools.yaml (whose only pool is eligible for
// "orders-team"), for ADR 0011 stage one's honest refusal at GET /v1/pools.
func noPoolBoundary(t *testing.T) http.Handler {
	t.Helper()
	cfg, err := config.Load("../../examples/config/pools.yaml", "", "development")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identities = []domain.Principal{{Subject: "ops", TenantID: "ops-team", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}}}
	auth, err := NewAuth(context.Background(), config.Settings{Environment: "development", AuthMode: "local", LocalToken: testToken, LocalSubject: "ops"}, cfg.Identities)
	if err != nil {
		t.Fatal(err)
	}
	ledger := storage.NewMemoryLedger()
	app := service.New(cfg, ledger, pendingInventory{}, completeObserver{})
	return New(app, auth, ledger, cfg).Handler()
}

// TestNoEligiblePoolRefusesPoolsButAllocationsAndFindingsStayHonestlyEmpty is
// ADR 0011 stage one's central invariant: /v1/pools is the one endpoint that
// turns an empty entitlement into a 403, everything else keeps being able to
// say "there is nothing" with a 200.
func TestNoEligiblePoolRefusesPoolsButAllocationsAndFindingsStayHonestlyEmpty(t *testing.T) {
	h := noPoolBoundary(t)

	w := call(h, "GET", "/v1/pools", "", testToken)
	if w.Code != 403 {
		t.Fatalf("pools status=%d body=%s", w.Code, w.Body)
	}
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "no_eligible_pool" {
		t.Fatalf("code=%q, want no_eligible_pool", envelope.Error.Code)
	}
	if envelope.Error.Retryable {
		t.Fatal("no_eligible_pool must not be retryable")
	}
	// The refusal must not leak which pool(s) exist or how many: no pool id,
	// name or count from the config.
	if strings.Contains(w.Body.String(), "pool_prod_euc1") {
		t.Fatalf("refusal body leaked a pool id: %s", w.Body)
	}

	for _, path := range []string{"/v1/allocations", "/v1/findings"} {
		w := call(h, "GET", path, "", testToken)
		if w.Code != 200 {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body)
		}
		var page struct {
			Items []any `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("%s: expected an empty items list, got %v", path, page.Items)
		}
	}
}

// TestEligiblePoolsCursorPastEndStaysEmpty200 proves the refusal is decided
// on the principal's full pool list before paging, exactly once, and never
// re-decided per page: an eligible principal whose cursor lands past the end
// of that (non-empty) list still gets an ordinary empty 200 page, not the
// 403 refusal.

// -- package G3b1: the operator role is refused everywhere ------------------
//
// operatorRoleToken authenticates as the "operator" subject configured by
// operatorBoundary below (package G3c's IPAM_LOCAL_EXTRA_CREDENTIALS
// mechanism, reused here to put a second identity beside the primary one in
// a single test process, exactly as the real Compose stack will).
const operatorRoleToken = "this-is-a-test-operator-token-with-enough-entropy"

// anotherTenantToken authenticates as a second, unrelated tenant (package
// H8c's TestCancelOperationRefusesAnotherTenantAndAnOperatorIdenticallyToUnknown),
// the same IPAM_LOCAL_EXTRA_CREDENTIALS mechanism as operatorRoleToken above.
const anotherTenantToken = "this-is-another-tenants-test-token-with-plenty-of-entropy"

// operatorForeignAllocationID belongs to a THIRD tenant, so the operator's
// cross-tenant reads (package G3b2) have two tenants to cross and the list has
// more than one row to page. operatorHoldAllocationID is an UNCOMMITTED hold,
// which no caller may see -- an operator included.
const (
	operatorForeignAllocationID = "alloc_payments_boundary"
	operatorHoldAllocationID    = "alloc_orders_uncommitted"
)

// operatorBoundary mirrors boundary(t): the same "developer" principal
// (tenant orders-team) authenticated by testToken, with an operator principal
// -- no tenant, no accounts, no environments, no regions -- configured
// BESIDE it and authenticated by operatorRoleToken. It seeds one committed,
// RESERVED allocation and one pending operation belonging to the ordinary
// "orders-team" tenant, one committed allocation of a second tenant, and one
// uncommitted hold, for the two proofs below: everything package G3b2 does not
// grant is still refused by a check that already existed, and everything it
// does grant is a read across tenants of committed state only.
//
// Package G3b3 adds the findings a domain-wide view needs to be worth having:
// one unmanaged resource reported to BOTH tenants, which an operator must see
// once; a second unmanaged resource reported to one of them, which must stay
// its own row; and one allocation-scoped finding, which is never grouped. So
// the store holds four rows, and both the operator's list and the developer's
// are three -- of which exactly one row, the shared resource, is the same fact
// under a different id.
func operatorBoundary(t *testing.T) (h http.Handler, ledger *storage.MemoryLedger, allocationID, operationID string) {
	t.Helper()
	cfg, err := config.Load("../../examples/config/pools.yaml", "", "development")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identities = []domain.Principal{
		{Subject: "developer", TenantID: "orders-team", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}},
		{Subject: "operator", Role: domain.OperatorRole},
	}
	auth, err := NewAuth(context.Background(), config.Settings{
		Environment: "development", AuthMode: "local", LocalToken: testToken, LocalSubject: "developer",
		LocalExtraCredentials: "operator:" + operatorRoleToken,
	}, cfg.Identities)
	if err != nil {
		t.Fatal(err)
	}
	ledger = storage.NewMemoryLedger()
	app := service.New(cfg, ledger, pendingInventory{}, completeObserver{})
	allocationID, operationID = "alloc_operator_boundary", "op_operator_boundary"
	now := time.Now().UTC()
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		st.Allocations[allocationID] = domain.Allocation{
			ID: allocationID, TenantID: "orders-team", Committed: true, State: domain.Reserved, Revision: 1,
			Request:   domain.Request{AllocationKey: "operator-boundary", Scope: "vpc", Environment: "prod", Region: "eu-central-1", AccountID: "123456789012", PrefixLength: 22},
			CIDR:      "10.64.0.0/22",
			PoolID:    "pool_prod_euc1",
			CreatedAt: now, UpdatedAt: now,
		}
		st.Allocations[operatorForeignAllocationID] = domain.Allocation{
			ID: operatorForeignAllocationID, TenantID: "payments-team", Committed: true, State: domain.Active, Revision: 1,
			Request:   domain.Request{AllocationKey: "payments-boundary", Scope: "vpc", Environment: "prod", Region: "eu-central-1", AccountID: "210987654321", PrefixLength: 22},
			CIDR:      "10.64.8.0/22",
			PoolID:    "pool_prod_euc1",
			CreatedAt: now, UpdatedAt: now,
		}
		st.Allocations[operatorHoldAllocationID] = domain.Allocation{
			ID: operatorHoldAllocationID, TenantID: "orders-team", Committed: false, State: domain.Reserved, Revision: 1,
			Request:   domain.Request{AllocationKey: "orders-hold", Scope: "vpc", Environment: "prod", Region: "eu-central-1", AccountID: "123456789012", PrefixLength: 22},
			CIDR:      "10.64.16.0/22",
			PoolID:    "pool_prod_euc1",
			CreatedAt: now, UpdatedAt: now,
		}
		st.Operations[operationID] = domain.Operation{ID: operationID, Type: "RESERVE", Status: "PENDING", AllocationID: allocationID, TenantID: "orders-team", CreatedAt: now, UpdatedAt: now}
		for _, f := range operatorFindingRows(now) {
			st.Findings[f.ID] = f
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return New(app, auth, ledger, cfg).Handler(), ledger, allocationID, operationID
}

// The finding ids are literals rather than the service's hash of the fan-out
// key, because what is under test here is the transport: which rows reach which
// caller, and how they page. They are chosen so the SECOND tenant's copy of the
// shared resource sorts first, which makes the representative visibly a choice
// (the lexicographically smallest member id) rather than whichever row happened
// to be written first.
const (
	operatorSharedFindingID = "finding_a_shared_payments"
	operatorOwnFindingID    = "finding_c_orders_only"
	operatorAllocFindingID  = "finding_d_orders_allocation"
)

func operatorFindingRows(now time.Time) []domain.Finding {
	shared := func(id, tenant string) domain.Finding {
		return domain.Finding{
			ID: id, TenantID: tenant, DomainID: "corporate-connected",
			Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN",
			AccountID: "123456789012", Region: "eu-central-1",
			ResourceType: "vpc", ResourceID: "vpc-0shared000000000",
			FirstObservedAt: now.Add(-time.Hour), LastObservedAt: now,
		}
	}
	orders := shared("finding_b_shared_orders", "orders-team")
	return []domain.Finding{
		shared(operatorSharedFindingID, "payments-team"),
		orders,
		{
			ID: operatorOwnFindingID, TenantID: "orders-team", DomainID: "corporate-connected",
			Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN",
			AccountID: "123456789012", Region: "eu-central-1",
			ResourceType: "vpc", ResourceID: "vpc-0second000000000",
			FirstObservedAt: now, LastObservedAt: now,
		},
		{
			ID: operatorAllocFindingID, TenantID: "orders-team", DomainID: "corporate-connected",
			AllocationID: operatorHoldAllocationID, Code: "adoption_stuck", Severity: "CRITICAL",
			Status: "OPEN", FirstObservedAt: now, LastObservedAt: now,
		},
	}
}

func operatorErrorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, w.Body)
	}
	return envelope.Error.Code
}

// TestOperatorDeniedByDefault is package G3b1's central proof, narrowed by
// package G3b2 to what is still refused. Every refusal below is the
// pre-existing tenant/scope check named in its comment, now reached by a
// principal that has no tenant to match, and none of them may ever be granted:
// the four writes are refused by ADR 0011's whole safety argument, and
// GET /v1/operations/{id} is withheld on purpose in v1.
//
// G3b2 deliberately moved four cases out of this test, because they are now
// grants rather than refusals: GET by id, GET /v1/allocations, GET /v1/pools
// and GET .../capacity are asserted positively in TestOperatorGrantedReads
// below. Package G3b3 moved the fifth for the same reason -- GET /v1/findings
// now answers the de-duplicated domain view, asserted in
// TestOperatorFindingsAreTheDomainView.
func TestOperatorDeniedByDefault(t *testing.T) {
	h, _, allocationID, operationID := operatorBoundary(t)

	t.Run("POST allocations: validateRequest's first gate", func(t *testing.T) {
		// internal/service/service.go, validateRequest: `if p.TenantID == ""`
		// -> 403 forbidden, "authenticated principal has no tenant".
		w := call(h, "POST", "/v1/allocations", reservation, operatorRoleToken)
		if w.Code != 403 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		if code := operatorErrorCode(t, w); code != "forbidden" {
			t.Fatalf("code=%q, want forbidden", code)
		}
	})

	// Each case below sends an otherwise-valid body and Idempotency-Key (and
	// If-Match, for PATCH) so the 404 can only be the tenant-scoping check
	// firing before the request's content is ever considered.
	writeCases := []struct {
		name, method, path, body string
		extraHeaders             map[string]string
		refusedBy                string
	}{
		{
			"PATCH: Patch compares a.TenantID != p.TenantID",
			"PATCH", "/v1/allocations/" + allocationID, `{"description":"operator probe"}`,
			map[string]string{"If-Match": `"1"`},
			"internal/service/service.go Patch",
		},
		{
			"PUT binding: Bind compares a.TenantID != p.TenantID",
			"PUT", "/v1/allocations/" + allocationID + "/binding",
			`{"provider":"aws","resource_type":"vpc","resource_id":"vpc-0operator00000000","account_id":"123456789012","region":"eu-central-1"}`,
			nil,
			"internal/service/service.go Bind",
		},
		{
			"DELETE: Release compares a.TenantID != p.TenantID",
			"DELETE", "/v1/allocations/" + allocationID, "", nil,
			"internal/service/service.go Release",
		},
		{
			"GET operation: Operation compares o.TenantID != p.TenantID",
			"GET", "/v1/operations/" + operationID, "", nil,
			"internal/service/service.go Operation",
		},
	}
	for _, tc := range writeCases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "operator-boundary-"+tc.method+"-"+tc.path)
			r.Header.Set("Authorization", "Bearer "+operatorRoleToken)
			for k, v := range tc.extraHeaders {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 404 {
				t.Fatalf("refused by %s: status=%d body=%s", tc.refusedBy, w.Code, w.Body)
			}
			if code := operatorErrorCode(t, w); code != "not_found" {
				t.Fatalf("refused by %s: code=%q, want not_found", tc.refusedBy, code)
			}
		})
	}

}

// TestOperatorGrantedReads is package G3b2's half of the boundary: the four
// reads ADR 0011 stage two grants, through the real handlers. Nothing in
// internal/transport changed -- the pools handler stops refusing an operator
// only because Service.Pools now answers with every configured pool, and the
// list, get and capacity handlers were never the place the tenant comparison
// lived.
func TestOperatorGrantedReads(t *testing.T) {
	h, _, allocationID, _ := operatorBoundary(t)

	t.Run("GET pools: every configured pool, so the stage-one refusal no longer applies", func(t *testing.T) {
		w := call(h, "GET", "/v1/pools", "", operatorRoleToken)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 0 || page.Items[0].ID != "pool_prod_euc1" {
			t.Fatalf("expected the configured pool, got %+v", page.Items)
		}
	})

	// allocationTenant names the tenant each operatorBoundary allocation
	// belongs to, so the presence assertions below check the real value, not
	// merely the key's presence.
	allocationTenant := map[string]string{allocationID: "orders-team", operatorForeignAllocationID: "payments-team"}

	t.Run("GET allocations: the other tenants' committed allocations, never a hold", func(t *testing.T) {
		w := call(h, "GET", "/v1/allocations", "", operatorRoleToken)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		ids := listedIDs(t, w)
		if len(ids) != 2 || ids[operatorForeignAllocationID] == false || ids[allocationID] == false {
			t.Fatalf("operator list = %v, want both tenants' committed allocations", ids)
		}
		if ids[operatorHoldAllocationID] {
			t.Fatal("an uncommitted hold reached the operator's list")
		}
		// Package G3b4: an operator's list carries the recipient tenant of
		// EVERY allocation, not just its own -- the field that lets it tell
		// the two tenants' rows apart.
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, item := range page.Items {
			id, _ := item["id"].(string)
			want := allocationTenant[id]
			if got, _ := item["tenant_id"].(string); got != want {
				t.Fatalf("id=%s tenant_id=%v, want %q", id, item["tenant_id"], want)
			}
			seen[id] = true
		}
		for id := range allocationTenant {
			if !seen[id] {
				t.Fatalf("operator list missing %s: %s", id, w.Body)
			}
		}
	})

	t.Run("GET by id: any committed allocation, of any tenant", func(t *testing.T) {
		for _, id := range []string{allocationID, operatorForeignAllocationID} {
			w := call(h, "GET", "/v1/allocations/"+id, "", operatorRoleToken)
			if w.Code != 200 {
				t.Fatalf("%s status=%d body=%s", id, w.Code, w.Body)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["id"] != id {
				t.Fatalf("id=%v, want %s", body["id"], id)
			}
			// Package G3b4: reading any tenant's allocation by id names that
			// tenant, so an operator that read the id off a list can also
			// read this field off the object directly.
			if got, want := body["tenant_id"], allocationTenant[id]; got != want {
				t.Fatalf("id=%s tenant_id=%v, want %q", id, got, want)
			}
		}
		// internal/service/service.go Get keeps `!a.Committed`, so the hold is
		// 404 for an operator exactly as it is for the tenant holding it.
		w := call(h, "GET", "/v1/allocations/"+operatorHoldAllocationID, "", operatorRoleToken)
		if w.Code != 404 {
			t.Fatalf("uncommitted hold status=%d body=%s", w.Code, w.Body)
		}
	})

	t.Run("GET capacity: any pool by id, and an unknown id is still 404", func(t *testing.T) {
		w := call(h, "GET", "/v1/pools/pool_prod_euc1/capacity", "", operatorRoleToken)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["pool_id"] != "pool_prod_euc1" {
			t.Fatalf("pool_id=%v", body["pool_id"])
		}
		w = call(h, "GET", "/v1/pools/pool_does_not_exist/capacity", "", operatorRoleToken)
		if w.Code != 404 {
			t.Fatalf("unknown pool status=%d body=%s", w.Code, w.Body)
		}
		if code := operatorErrorCode(t, w); code != "not_found" {
			t.Fatalf("code=%q, want not_found", code)
		}
	})

	// The cursor walk is the property ADR 0011 says widening the filter must
	// not break: page() sorts on the item id and advances by `id > cursor`, so
	// a cross-tenant list has to visit every row exactly once and terminate.
	t.Run("limit=1 paging visits every row exactly once and terminates", func(t *testing.T) {
		seen := map[string]int{}
		cursor := ""
		for requests := 0; ; requests++ {
			if requests > 10 {
				t.Fatal("the cursor walk did not terminate")
			}
			path := "/v1/allocations?limit=1"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			w := call(h, "GET", path, "", operatorRoleToken)
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			var page struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
				NextCursor *string `json:"next_cursor"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > 1 {
				t.Fatalf("limit=1 returned %d items", len(page.Items))
			}
			for _, item := range page.Items {
				seen[item.ID]++
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
		want := []string{allocationID, operatorForeignAllocationID}
		if len(seen) != len(want) {
			t.Fatalf("the walk saw %v, want exactly %v", seen, want)
		}
		for _, id := range want {
			if seen[id] != 1 {
				t.Fatalf("%s was visited %d times, want exactly once", id, seen[id])
			}
		}
	})
}

// TestOperatorFindingsAreTheDomainView is package G3b3's half of the boundary,
// through the real handler. Nothing in internal/transport changed: the findings
// handler projects whatever Service.Findings returns, and what changed is that
// for an operator that is now one row per domain-level fact rather than nothing
// at all.
func TestOperatorFindingsAreTheDomainView(t *testing.T) {
	h, _, _, _ := operatorBoundary(t)

	t.Run("one row per domain-level fact, across tenants", func(t *testing.T) {
		w := call(h, "GET", "/v1/findings", "", operatorRoleToken)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		ids := listedIDs(t, w)
		want := []string{operatorSharedFindingID, operatorOwnFindingID, operatorAllocFindingID}
		if len(ids) != len(want) {
			t.Fatalf("operator findings = %v, want exactly %v", ids, want)
		}
		for _, id := range want {
			if !ids[id] {
				t.Fatalf("operator findings = %v, missing %s", ids, id)
			}
		}
		// The two tenants' copies of one unmanaged resource are one row, and it
		// is the smaller of their two ids -- "finding_b_shared_orders" is the
		// other member and must not appear as a row of its own.
		if ids["finding_b_shared_orders"] {
			t.Fatalf("both copies of one resource reached the operator: %v", ids)
		}
	})

	t.Run("the developer still sees its own three rows and nothing else", func(t *testing.T) {
		w := call(h, "GET", "/v1/findings", "", testToken)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		ids := listedIDs(t, w)
		if len(ids) != 3 || !ids["finding_b_shared_orders"] || !ids[operatorOwnFindingID] || !ids[operatorAllocFindingID] {
			t.Fatalf("tenant findings = %v, want its own three rows", ids)
		}
		if ids[operatorSharedFindingID] {
			t.Fatalf("a tenant saw another tenant's finding: %v", ids)
		}
	})

	// Package G3b4: the developer's own findings response is byte-for-byte
	// what it always was -- none of the four operator-only keys ever reaches
	// a tenant, whether or not an operator is configured beside it.
	t.Run("a tenant's response carries none of the operator-only fields", func(t *testing.T) {
		w := call(h, "GET", "/v1/findings", "", testToken)
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 0 {
			t.Fatalf("no items to check: %s", w.Body)
		}
		for _, item := range page.Items {
			for _, key := range []string{"tenant_id", "domain_id", "resource_type", "resource_id"} {
				if _, ok := item[key]; ok {
					t.Fatalf("%s reached a tenant's findings response: %s", key, w.Body)
				}
			}
		}
	})

	// Package G3b4: an operator's response gains domain_id on every row,
	// resource_type/resource_id on the grouped occupancy rows that carry
	// them, and tenant_id ONLY on the allocation-scoped row -- absent, not
	// null, on the two rows with no allocation (ADR 0011's "a grouped
	// occupancy row has no recipient tenant").
	t.Run("an operator's response carries domain_id, the resource identity, and tenant_id only where allocation_id is set", func(t *testing.T) {
		w := call(h, "GET", "/v1/findings", "", operatorRoleToken)
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		byID := map[string]map[string]any{}
		for _, item := range page.Items {
			id, _ := item["id"].(string)
			byID[id] = item
		}
		for _, id := range []string{operatorSharedFindingID, operatorOwnFindingID, operatorAllocFindingID} {
			item, ok := byID[id]
			if !ok {
				t.Fatalf("missing %s: %s", id, w.Body)
			}
			if item["domain_id"] != "corporate-connected" {
				t.Fatalf("%s domain_id=%v, want corporate-connected", id, item["domain_id"])
			}
		}
		// The two occupancy (domain-level) rows: resource identity present,
		// tenant_id absent -- they have no one recipient.
		for id, resourceID := range map[string]string{
			operatorSharedFindingID: "vpc-0shared000000000",
			operatorOwnFindingID:    "vpc-0second000000000",
		} {
			item := byID[id]
			if item["resource_type"] != "vpc" {
				t.Fatalf("%s resource_type=%v, want vpc", id, item["resource_type"])
			}
			if item["resource_id"] != resourceID {
				t.Fatalf("%s resource_id=%v, want %s", id, item["resource_id"], resourceID)
			}
			if _, ok := item["tenant_id"]; ok {
				t.Fatalf("%s tenant_id=%v, want the key absent (no recipient)", id, item["tenant_id"])
			}
		}
		// The allocation-scoped row: tenant_id present and correct (it names
		// the hold's owning tenant), resource identity absent -- the fan-out
		// never sets it for a finding that names an allocation.
		alloc := byID[operatorAllocFindingID]
		if alloc["tenant_id"] != "orders-team" {
			t.Fatalf("%s tenant_id=%v, want orders-team", operatorAllocFindingID, alloc["tenant_id"])
		}
		for _, key := range []string{"resource_type", "resource_id"} {
			if _, ok := alloc[key]; ok {
				t.Fatalf("%s %s=%v, want the key absent", operatorAllocFindingID, key, alloc[key])
			}
		}
	})

	// The same cursor property TestOperatorGrantedReads asserts for
	// allocations, over a list whose rows are not all stored rows: two groups
	// and one allocation-scoped finding. A representative chosen by map
	// iteration would make the walk skip or repeat a row here.
	t.Run("limit=1 paging visits every row exactly once and terminates", func(t *testing.T) {
		seen := map[string]int{}
		cursor := ""
		for requests := 0; ; requests++ {
			if requests > 10 {
				t.Fatal("the cursor walk did not terminate")
			}
			path := "/v1/findings?limit=1"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			w := call(h, "GET", path, "", operatorRoleToken)
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			var page struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
				NextCursor *string `json:"next_cursor"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > 1 {
				t.Fatalf("limit=1 returned %d items", len(page.Items))
			}
			for _, item := range page.Items {
				seen[item.ID]++
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
		want := []string{operatorSharedFindingID, operatorOwnFindingID, operatorAllocFindingID}
		if len(seen) != len(want) {
			t.Fatalf("the walk saw %v, want exactly %v", seen, want)
		}
		for _, id := range want {
			if seen[id] != 1 {
				t.Fatalf("%s was visited %d times, want exactly once", id, seen[id])
			}
		}
	})
}

// operatorReadLogLine is the shape secureRead's slog.Info line decodes into.
// Unrecognized keys (slog's own "time" and "level") are ignored by
// json.Unmarshal, which is exactly what is wanted here: the test asserts on
// the attributes package G3b4 adds, not on slog's own record shape.
type operatorReadLogLine struct {
	Msg     string `json:"msg"`
	Subject string `json:"subject"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Status  int    `json:"status"`
	Rows    *int   `json:"rows"`
}

// TestOperatorReadsAreLoggedButTenantReadsAreNot is package G3b4's read
// logging proof. It captures the default slog logger into a buffer-backed
// JSON handler (the same technique internal/transport/auth_test.go's
// TestExtraCredentialsIgnoredAndWarnedInOIDCMode uses) and restores it
// afterward, so this test does not leak its logger into any other test in
// the package.
func TestOperatorReadsAreLoggedButTenantReadsAreNot(t *testing.T) {
	h, ledger, allocationID, _ := operatorBoundary(t)

	eventCount := func() int {
		n := 0
		if err := ledger.View(context.Background(), func(st *domain.State) error { n = len(st.Events); return nil }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := eventCount()

	var buf bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prior)

	// A tenant's reads -- including of the exact allocation the operator is
	// about to read -- must produce no read-audit line at all, and the
	// bearer token must never appear in the log regardless of who is making
	// the request.
	if w := call(h, "GET", "/v1/allocations", "", testToken); w.Code != 200 {
		t.Fatalf("tenant list status=%d body=%s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/v1/allocations/"+allocationID, "", testToken); w.Code != 200 {
		t.Fatalf("tenant get status=%d body=%s", w.Code, w.Body)
	}
	if strings.Contains(buf.String(), `"operator read"`) {
		t.Fatalf("a tenant read was logged: %s", buf.String())
	}
	if strings.Contains(buf.String(), testToken) {
		t.Fatal("a credential appeared in the log")
	}
	buf.Reset()

	// An operator's reads: a list (rows recorded), a by-id read (no rows
	// field -- "not applicable", never a fabricated 0 or 1), and a refused
	// read (GET /v1/operations/{id}, withheld from the operator in v1) --
	// "successful or failed" means the refusal is logged too, with its real
	// (404) status.
	// The list is asked for with a query string. The log records the route and
	// never the query: a cursor is an id of somebody's object, and whatever a
	// caller puts in a parameter must not be what ends up in the log.
	if w := call(h, "GET", "/v1/allocations?limit=50&cursor=YQ", "", operatorRoleToken); w.Code != 200 {
		t.Fatalf("operator list status=%d body=%s", w.Code, w.Body)
	}
	// "YQ" is the base64url cursor for "a", which sorts before every id, so the
	// page is the whole list.
	if strings.Contains(buf.String(), "cursor=") || strings.Contains(buf.String(), "limit=") || strings.Contains(buf.String(), "?") {
		t.Fatalf("the query string reached the log: %s", buf.String())
	}
	if w := call(h, "GET", "/v1/allocations/"+allocationID, "", operatorRoleToken); w.Code != 200 {
		t.Fatalf("operator get status=%d body=%s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/v1/operations/op_does_not_exist", "", operatorRoleToken); w.Code != 404 {
		t.Fatalf("operator failed read status=%d body=%s", w.Code, w.Body)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 operator read log lines, got %d: %s", len(lines), buf.String())
	}
	var decoded []operatorReadLogLine
	for _, line := range lines {
		var l operatorReadLogLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("decode log line: %v (%s)", err, line)
		}
		decoded = append(decoded, l)
	}

	list, get, failed := decoded[0], decoded[1], decoded[2]
	if list.Msg != "operator read" || list.Subject != "operator" || list.Method != "GET" || list.Path != "/v1/allocations" || list.Status != 200 {
		t.Fatalf("list log line = %+v", list)
	}
	if list.Rows == nil || *list.Rows != 2 {
		t.Fatalf("list log line rows = %v, want 2 (both tenants' committed allocations)", list.Rows)
	}
	if get.Path != "/v1/allocations/"+allocationID || get.Status != 200 {
		t.Fatalf("get log line = %+v", get)
	}
	if get.Rows != nil {
		t.Fatalf("get log line rows = %v, want absent (not a list endpoint)", *get.Rows)
	}
	if failed.Path != "/v1/operations/op_does_not_exist" || failed.Status != 404 {
		t.Fatalf("failed-read log line = %+v", failed)
	}
	if failed.Rows != nil {
		t.Fatalf("failed-read log line rows = %v, want absent", *failed.Rows)
	}
	if strings.Contains(buf.String(), operatorRoleToken) {
		t.Fatal("a credential appeared in the log")
	}

	// Reads -- granted or refused -- never reach the ledger. Every
	// Ledger.Update is a stop-the-world rewrite (ADR 0010); read logging must
	// not turn one into a write.
	if after := eventCount(); after != before {
		t.Fatalf("operator reads wrote to the ledger: events %d -> %d", before, after)
	}
}

// listedIDs decodes a page envelope into the set of item ids it carries.
func listedIDs(t *testing.T, w *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v (body=%s)", err, w.Body)
	}
	out := map[string]bool{}
	for _, item := range page.Items {
		out[item.ID] = true
	}
	return out
}

// TestTenantAndInternalFieldsWithOperatorConfigured is
// TestTenantAndInternalFields' sibling (that test is untouched, per package
// G3b1's contract): both of its properties -- a foreign allocation id is
// 404, and never appears in a list -- must hold unchanged for an ordinary
// tenant caller once an operator identity is configured beside it. The
// operator's existence must change nothing for a tenant.
func TestTenantAndInternalFieldsWithOperatorConfigured(t *testing.T) {
	h, ledger, _, _ := operatorBoundary(t)
	now := time.Now().UTC()
	a := domain.Allocation{ID: "alloc_other", TenantID: "other", Committed: true, CreatedAt: now}
	if err := ledger.Update(context.Background(), func(s *domain.State) error { s.Allocations[a.ID] = a; return nil }); err != nil {
		t.Fatal(err)
	}
	w := call(h, "GET", "/v1/allocations/alloc_other", "", testToken)
	if w.Code != 404 {
		t.Fatalf("cross tenant status%d", w.Code)
	}
	w = call(h, "GET", "/v1/allocations", "", testToken)
	if strings.Contains(w.Body.String(), "alloc_other") {
		t.Fatal("cross tenant list leak")
	}
	// Both properties above are unchanged by package G3b2 and must stay so.
	// What G3b2 adds is the other side of the same coin, asserted here so the
	// two are read together: the allocation the tenant may not learn exists is
	// one the operator reads by id and lists, on the same request path, in the
	// same process. A change that broke the tenant's 404 by widening something
	// shared would have to break this test's second half to do it.
	w = call(h, "GET", "/v1/allocations/alloc_other", "", operatorRoleToken)
	if w.Code != 200 {
		t.Fatalf("operator get of another tenant's allocation: status=%d body=%s", w.Code, w.Body)
	}
	w = call(h, "GET", "/v1/allocations", "", operatorRoleToken)
	if !strings.Contains(w.Body.String(), "alloc_other") {
		t.Fatalf("the operator's list did not contain the allocation: %s", w.Body)
	}
}

// TestTenantAllocationResponsesCarryNoOperatorOnlyFieldsWithOperatorConfigured
// is TestPublicAllocationHidesAdapterState's sibling through the real
// handlers rather than the projection function directly: with an operator
// identity configured AND its data present (operatorBoundary seeds a second
// tenant's allocation the operator can read), the developer's own GET (list
// and by id) responses gain no key. This is the "byte-identical to today's"
// half of package G3b4's contract -- TestOperatorGrantedReads above is the
// operator's half.
func TestTenantAllocationResponsesCarryNoOperatorOnlyFieldsWithOperatorConfigured(t *testing.T) {
	h, _, allocationID, _ := operatorBoundary(t)

	w := call(h, "GET", "/v1/allocations/"+allocationID, "", testToken)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["tenant_id"]; ok {
		t.Fatalf("tenant_id reached a tenant's own GET: %s", w.Body)
	}

	w = call(h, "GET", "/v1/allocations", "", testToken)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 {
		t.Fatalf("no items to check: %s", w.Body)
	}
	for _, item := range page.Items {
		if _, ok := item["tenant_id"]; ok {
			t.Fatalf("tenant_id reached a tenant's own list: %s", w.Body)
		}
	}
}

func TestEligiblePoolsCursorPastEndStaysEmpty200(t *testing.T) {
	h, _ := boundary(t)
	// "pool_prod_euc1" is the only pool this principal (tenant orders-team)
	// is eligible for; any cursor that sorts after it is past the end.
	cursor := base64.RawURLEncoding.EncodeToString([]byte("zzzzzzzzzz"))
	w := call(h, "GET", "/v1/pools?cursor="+cursor, "", testToken)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var page struct {
		Items      []any   `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("expected an empty page past the end, got %+v", page)
	}
}

// --- DELETE /v1/operations/{id}: cancelling a stuck reservation (ADR 0013,
// package H8c) -------------------------------------------------------------

// reservationStuckFindingID replicates internal/service/service.go's private
// findingID(reservationStuckCode, allocationID) exactly (sha256 of the code,
// a NUL byte, and the allocation id, hex-encoded and prefixed "finding_").
// cancelCheck looks a finding up at this exact key, so a test that wants its
// seeded finding to be found has to compute the same key; the two literals
// (reservationStuckCode = "reservation_stuck") are pinned by
// internal/service/abandon.go and internal/service/cancel_test.go.
func reservationStuckFindingID(allocationID string) string {
	h := sha256.Sum256([]byte("reservation_stuck\x00" + allocationID))
	return "finding_" + hex.EncodeToString(h[:])
}

// stuckReservationBoundary seeds, directly on the ledger, exactly the rows a
// real stuck reservation leaves (ADR 0013): an uncommitted RESERVE
// allocation, its pending operation, and the open reservation_stuck finding
// that is the platform's own verdict and the one thing that makes the hold
// cancellable. It deliberately writes the ledger by hand rather than driving
// the worker's real detection path (internal/service/cancel_test.go already
// covers that end to end) -- what this boundary exists to test is the HTTP
// route above a state that is a given.
func stuckReservationBoundary(t *testing.T) (http.Handler, *storage.MemoryLedger, string, string) {
	t.Helper()
	return stuckReservationBoundaryWithInventory(t, pendingInventory{})
}

// cancelInventory lets a test control what Service.CancelReservation's calls
// to Inventory.CancelReservation answer, to reach the two refusal codes
// cancelRemoveError classifies (internal/service/cancel.go) without a real
// NetBox: cancel_inventory_refused for a definite refusal, cancel_uncertain
// (retryable) for anything uncertainInventoryError recognises, most notably
// domain.ErrInventoryUncertain. Embedding pendingInventory keeps every other
// method's no-op behaviour so only CancelReservation needs overriding.
type cancelInventory struct {
	pendingInventory
	err error
}

func (c cancelInventory) CancelReservation(context.Context, domain.Allocation, string) error {
	return c.err
}

// stuckReservationBoundaryWithInventory is stuckReservationBoundary with the
// inventory adapter the app is built against left to the caller, so a test
// can reach the refusal codes that come from Inventory.CancelReservation's
// own answer rather than from the fence's ledger checks.
func stuckReservationBoundaryWithInventory(t *testing.T, inv domain.Inventory) (http.Handler, *storage.MemoryLedger, string, string) {
	t.Helper()
	cfg, err := config.Load("../../examples/config/pools.yaml", "", "development")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identities = []domain.Principal{{Subject: "developer", TenantID: "orders-team", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}}}
	auth, err := NewAuth(context.Background(), config.Settings{Environment: "development", AuthMode: "local", LocalToken: testToken, LocalSubject: "developer"}, cfg.Identities)
	if err != nil {
		t.Fatal(err)
	}
	ledger := storage.NewMemoryLedger()
	app := service.New(cfg, ledger, inv, completeObserver{})
	h := New(app, auth, ledger, cfg).Handler()

	allocationID, operationID := "alloc_stuck_reservation", "op_stuck_reservation"
	now := time.Now().UTC()
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		st.Allocations[allocationID] = domain.Allocation{
			ID: allocationID, TenantID: "orders-team", DomainID: "orders-team-domain", Committed: false, State: domain.Reserved, Revision: 1,
			Request:   domain.Request{AllocationKey: "cancel-boundary", Scope: "vpc", Environment: "prod", Region: "eu-central-1", AccountID: "123456789012", PrefixLength: 22},
			CIDR:      "10.64.32.0/22",
			PoolID:    "pool_prod_euc1",
			CreatedAt: now, UpdatedAt: now,
		}
		st.Operations[operationID] = domain.Operation{ID: operationID, Type: "RESERVE", Status: "PENDING", AllocationID: allocationID, TenantID: "orders-team", CreatedAt: now, UpdatedAt: now}
		fid := reservationStuckFindingID(allocationID)
		st.Findings[fid] = domain.Finding{
			ID: fid, TenantID: "orders-team", DomainID: "orders-team-domain", AllocationID: allocationID,
			Code: "reservation_stuck", Severity: "CRITICAL", Status: "OPEN",
			FirstObservedAt: now.Add(-time.Hour), LastObservedAt: now,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return h, ledger, allocationID, operationID
}

// TestCancelOperationWithdrawsAStuckReservation is the route's success path:
// a tenant's own DELETE of a stuck reservation's operation answers 200 with
// the whole CancelReport, the allocation row is gone, GET of the operation
// keeps answering FAILED reservation_cancelled forever afterward, and the
// finding resolves.
func TestCancelOperationWithdrawsAStuckReservation(t *testing.T) {
	h, ledger, allocationID, operationID := stuckReservationBoundary(t)
	w := call(h, "DELETE", "/v1/operations/"+operationID, "", testToken)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var report map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatalf("cancel report is not JSON: %v: %s", err, w.Body)
	}
	alloc, _ := report["allocation"].(map[string]any)
	if alloc["id"] != allocationID {
		t.Fatalf("report=%v, want allocation.id=%s", report, allocationID)
	}
	if report["deleted"] != true || report["fenced"] != true || report["removed"] != true || report["finding_resolved"] != true {
		t.Fatalf("report did not show a completed withdrawal: %v", report)
	}
	if err := ledger.View(context.Background(), func(st *domain.State) error {
		if _, held := st.Allocations[allocationID]; held {
			t.Fatal("allocation row survived the cancel")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The route does not shadow or break GET /v1/operations/{id}: it keeps
	// answering the FAILED operation this DELETE just wrote, forever.
	w = call(h, "GET", "/v1/operations/"+operationID, "", testToken)
	if w.Code != 200 {
		t.Fatalf("GET after cancel: status=%d body=%s", w.Code, w.Body)
	}
	var op map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	errBody, _ := op["error"].(map[string]any)
	if op["status"] != "FAILED" || errBody == nil || errBody["code"] != "reservation_cancelled" {
		t.Fatalf("GET after cancel = %v, want FAILED reservation_cancelled", op)
	}

	// A repeat DELETE converges rather than refusing (idempotent by
	// definition): it reports the same completed withdrawal again.
	w = call(h, "DELETE", "/v1/operations/"+operationID, "", testToken)
	if w.Code != 200 {
		t.Fatalf("repeat cancel: status=%d body=%s", w.Code, w.Body)
	}
}

// TestCancelOperationIgnoresARequestBody is the record's own answer to
// "ignored or refused": DELETE needs no body (docs/API_V1.md section 5), and
// cancelOperation never reads r.Body at all, so one sent along changes
// nothing about the outcome -- exactly like release's DELETE
// /v1/allocations/{id}, which also never reads a body.
func TestCancelOperationIgnoresARequestBody(t *testing.T) {
	h, _, _, operationID := stuckReservationBoundary(t)
	w := call(h, "DELETE", "/v1/operations/"+operationID, `{"reason":"unexpected but harmless"}`, testToken)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s, want a body to be silently ignored", w.Code, w.Body)
	}
}

// TestCancelOperationRefusalsCarryTheirOwnStatusCodeAndRetryableFlag tables
// over the refusal codes package H8b's review note lists, each reached
// through the real service (never a fake), and checks that the transport's
// existing error writer (failure, unchanged by this package) passes the
// status, code and retryable flag through exactly as Service.CancelReservation
// set them.
func TestCancelOperationRefusalsCarryTheirOwnStatusCodeAndRetryableFlag(t *testing.T) {
	cases := []struct {
		name      string
		seed      func(*domain.State, string, string)
		status    int
		code      string
		retryable bool
	}{
		{
			name: "unknown operation id",
			seed: func(st *domain.State, allocationID, operationID string) {
				delete(st.Operations, operationID)
			},
			status: 404, code: "not_found", retryable: false,
		},
		{
			name: "committed allocation refused absolutely",
			seed: func(st *domain.State, allocationID, operationID string) {
				a := st.Allocations[allocationID]
				a.Committed = true
				st.Allocations[allocationID] = a
			},
			status: 409, code: "cancel_committed", retryable: false,
		},
		{
			name: "no open reservation_stuck finding",
			seed: func(st *domain.State, allocationID, operationID string) {
				delete(st.Findings, reservationStuckFindingID(allocationID))
			},
			status: 409, code: "reservation_not_stuck", retryable: false,
		},
		{
			name: "operation already succeeded",
			seed: func(st *domain.State, allocationID, operationID string) {
				o := st.Operations[operationID]
				o.Status = "SUCCEEDED"
				st.Operations[operationID] = o
			},
			status: 409, code: "cancel_operation_succeeded", retryable: false,
		},
		{
			name: "not a reservation",
			seed: func(st *domain.State, allocationID, operationID string) {
				o := st.Operations[operationID]
				o.Type = "VERIFY_BINDING"
				st.Operations[operationID] = o
			},
			status: 409, code: "cancel_not_a_reservation", retryable: false,
		},
		{
			// cancelCheck's !held branch: the operation still names an
			// allocation the ledger no longer holds, and it is not this
			// cancel's own fence (cancelFenced is false), so this is a ledger
			// state the record says "nothing in this project can produce" --
			// tested anyway because the refusal exists to answer it if one
			// ever does.
			name: "operation names an allocation the ledger does not hold",
			seed: func(st *domain.State, allocationID, operationID string) {
				delete(st.Allocations, allocationID)
			},
			status: 409, code: "cancel_no_allocation", retryable: false,
		},
		{
			// A RESERVE that is FAILED for a reason other than this cancel's
			// own fence code hits cancelCheck's default branch: a terminal
			// status this cancel did not write, which is a different refusal
			// from "already fenced by this cancel" (cancelFenced) and from
			// "succeeded" (its own code).
			name: "operation already terminal for an unrelated reason",
			seed: func(st *domain.State, allocationID, operationID string) {
				o := st.Operations[operationID]
				o.Status = "FAILED"
				o.Error = domain.Err(409, "domain_busy", "an unrelated failure wrote this operation's terminal status")
				st.Operations[operationID] = o
			},
			status: 409, code: "cancel_operation_terminal", retryable: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, ledger, allocationID, operationID := stuckReservationBoundary(t)
			if err := ledger.Update(context.Background(), func(st *domain.State) error {
				tc.seed(st, allocationID, operationID)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			w := call(h, "DELETE", "/v1/operations/"+operationID, "", testToken)
			if w.Code != tc.status {
				t.Fatalf("status=%d, want %d; body=%s", w.Code, tc.status, w.Body)
			}
			var envelope struct {
				Error struct {
					Code      string         `json:"code"`
					Retryable bool           `json:"retryable"`
					Details   map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != tc.code {
				t.Fatalf("code=%q, want %q; body=%s", envelope.Error.Code, tc.code, w.Body)
			}
			if envelope.Error.Retryable != tc.retryable {
				t.Fatalf("retryable=%v, want %v; body=%s", envelope.Error.Retryable, tc.retryable, w.Body)
			}
			if w.Header().Get("X-Request-ID") == "" {
				t.Error("missing X-Request-ID")
			}
			// H9: every case in this table is a refusal cancelCheck makes
			// inside the fence's own ledger transaction, before
			// CancelReservation ever builds a *CancelReport (cancel.go:
			// "return out, apiErr(...)" happens before "report :=
			// cancelReport(...)"). cancelProgressDetails must add nothing to
			// such a refusal's details -- there is no progress to report.
			if len(envelope.Error.Details) != 0 {
				t.Errorf("a pre-fence refusal carries progress details, want none: %v", envelope.Error.Details)
			}
		})
	}
}

// TestCancelOperationInventoryRefusalsCarryTheirOwnStatusCodeAndRetryableFlag
// covers the two H8b codes that come from Inventory.CancelReservation's own
// answer rather than from the fence's ledger checks (cancelRemoveError,
// internal/service/cancel.go): a definite refusal (409
// cancel_inventory_refused, not retryable) and an uncertain one (503
// cancel_uncertain, retryable). Neither is reachable through the boundary's
// ordinary no-op pendingInventory, so this uses cancelInventory to control
// what the port answers. Both also cover work-plan package H9's
// cancelProgressDetails: this run wrote the fence itself before the
// inventory answered, so error.details must carry fenced: true,
// already_fenced: false, removed: false, deleted: false, and the fenced
// allocation/operation ids -- the fence's own progress, established before
// the removal that then failed or came back uncertain.
func TestCancelOperationInventoryRefusalsCarryTheirOwnStatusCodeAndRetryableFlag(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{
			name:   "inventory refuses to remove the prefix",
			err:    errors.New("duplicate allocation marker in NetBox"),
			status: 409, code: "cancel_inventory_refused", retryable: false,
		},
		{
			name:   "inventory answer is uncertain",
			err:    fmt.Errorf("timeout talking to NetBox: %w", domain.ErrInventoryUncertain),
			status: 503, code: "cancel_uncertain", retryable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, ledger, allocationID, operationID := stuckReservationBoundaryWithInventory(t, cancelInventory{err: tc.err})
			w := call(h, "DELETE", "/v1/operations/"+operationID, "", testToken)
			if w.Code != tc.status {
				t.Fatalf("status=%d, want %d; body=%s", w.Code, tc.status, w.Body)
			}
			var envelope struct {
				Error struct {
					Code      string         `json:"code"`
					Retryable bool           `json:"retryable"`
					Details   map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != tc.code {
				t.Fatalf("code=%q, want %q; body=%s", envelope.Error.Code, tc.code, w.Body)
			}
			if envelope.Error.Retryable != tc.retryable {
				t.Fatalf("retryable=%v, want %v; body=%s", envelope.Error.Retryable, tc.retryable, w.Body)
			}
			// H9: the fence had already succeeded when the removal failed or
			// came back uncertain, so error.details carries that progress --
			// fenced true (this run wrote it, not a resumed one),
			// already_fenced false, removed false (the removal never
			// answered cleanly), deleted false (the delete step was never
			// reached) -- plus the ids of the allocation and operation this
			// run fenced.
			want := map[string]any{
				"allocation_id": allocationID, "operation_id": operationID,
				"fenced": true, "already_fenced": false, "removed": false, "deleted": false,
			}
			for k, v := range want {
				if got := envelope.Error.Details[k]; got != v {
					t.Errorf("details[%q]=%v, want %v; details=%v", k, got, v, envelope.Error.Details)
				}
			}
			// Neither outcome deletes the ledger row: a refused or uncertain
			// removal stops the run before the delete (ADR 0013), so the fence
			// is written -- the operation is FAILED reservation_cancelled --
			// but the allocation stays exactly so a retry has something to
			// converge from.
			if err := ledger.View(context.Background(), func(st *domain.State) error {
				if _, held := st.Allocations[allocationID]; !held {
					t.Fatal("an inventory refusal or uncertainty deleted the allocation row")
				}
				o, ok := st.Operations[operationID]
				if !ok || o.Status != "FAILED" || o.Error == nil || o.Error.Code != "reservation_cancelled" {
					t.Fatalf("fence was not written before the inventory answer: %+v", o)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestCancelProgressDetails is a direct, unit-level test of
// cancelProgressDetails (work-plan package H9), beside the HTTP-level tests
// above: report == nil returns err untouched, report != nil with a
// *domain.APIError attaches the six fields, and -- the case no HTTP-level
// test can reach, because Service.CancelReservation never actually returns
// a non-nil report beside a non-APIError error -- report != nil with a
// PLAIN error (not a *domain.APIError) must not panic and must return err
// unchanged, since there is no APIError to attach Details to. A mutant that
// replaces the errors.As guard with an unconditional type assertion
// (apierr := err.(*domain.APIError)) compiles clean and passes every
// HTTP-level cancel test, because none of them ever supplies a non-APIError
// error alongside a report; this case is what kills it.
func TestCancelProgressDetails(t *testing.T) {
	report := &service.CancelReport{
		Allocation: service.CancelledAllocation{ID: "alloc_1"},
		Operation:  service.CancelledOperation{ID: "op_1"},
		Fenced:     true, AlreadyFenced: false, Removed: false, Deleted: false,
	}

	t.Run("nil report returns err untouched", func(t *testing.T) {
		want := domain.Err(409, "cancel_committed", "allocation alloc_1 is committed")
		got := cancelProgressDetails(want, nil)
		if got != want {
			t.Fatalf("got %v, want the same error value untouched", got)
		}
		if apierr, ok := got.(*domain.APIError); !ok || apierr.Details != nil {
			t.Fatalf("a nil report must add no details: %+v", got)
		}
	})

	t.Run("APIError with a report gains the progress fields", func(t *testing.T) {
		want := domain.Err(503, "cancel_uncertain", "the inventory did not answer")
		got := cancelProgressDetails(want, report)
		apierr, ok := got.(*domain.APIError)
		if !ok {
			t.Fatalf("got %T, want *domain.APIError", got)
		}
		if apierr.Code != "cancel_uncertain" || apierr.Status != 503 {
			t.Fatalf("status/code changed: %+v", apierr)
		}
		wantDetails := map[string]any{
			"allocation_id": "alloc_1", "operation_id": "op_1",
			"fenced": true, "already_fenced": false, "removed": false, "deleted": false,
		}
		for k, v := range wantDetails {
			if apierr.Details[k] != v {
				t.Errorf("details[%q]=%v, want %v; details=%v", k, apierr.Details[k], v, apierr.Details)
			}
		}
	})

	t.Run("a report beside a non-APIError error is returned unchanged, not panicking", func(t *testing.T) {
		plain := errors.New("not an APIError")
		got := cancelProgressDetails(plain, report)
		if got != plain {
			t.Fatalf("got %v, want the plain error returned unchanged", got)
		}
	})
}

// TestCancelOperationRefusesAnotherTenantAndAnOperatorIdenticallyToUnknown is
// ADR 0013's "no tenant, and no operator, can affect another tenant's hold":
// another tenant's token and the operator role both answer the exact 404
// body an unknown operation id answers, which is what makes the refusal leak
// nothing an existing read (GET /v1/operations/{id}) did not already leak.
func TestCancelOperationRefusesAnotherTenantAndAnOperatorIdenticallyToUnknown(t *testing.T) {
	h, ledger, allocationID, operationID := stuckReservationBoundary(t)
	cfg, err := config.Load("../../examples/config/pools.yaml", "", "development")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identities = []domain.Principal{
		{Subject: "developer", TenantID: "orders-team", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}},
		{Subject: "another-tenant", TenantID: "payments-team", Accounts: []string{"210987654321"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}},
		{Subject: "operator", Role: domain.OperatorRole},
	}
	auth, err := NewAuth(context.Background(), config.Settings{
		Environment: "development", AuthMode: "local", LocalToken: testToken, LocalSubject: "developer",
		LocalExtraCredentials: "another-tenant:" + anotherTenantToken + ",operator:" + operatorRoleToken,
	}, cfg.Identities)
	if err != nil {
		t.Fatal(err)
	}
	app := service.New(cfg, ledger, pendingInventory{}, completeObserver{})
	h = New(app, auth, ledger, cfg).Handler()

	unknown := call(h, "DELETE", "/v1/operations/op_does_not_exist", "", testToken)
	want := unknown.Body.String()
	if unknown.Code != 404 {
		t.Fatalf("baseline unknown-id status=%d body=%s", unknown.Code, want)
	}

	// DELETE /v1/operations/{id} is a write and must be registered through
	// secure, never secureRead (docs/API_V1.md, this route's own comment in
	// internal/transport/http.go): secureRead's whole purpose is to log an
	// "operator read" line for an operator principal, and mislabelling a
	// write as a read in the audit log would be wrong even though the
	// service already refuses the operator with 404 either way. Capture the
	// default logger exactly as TestOperatorReadsAreLoggedButTenantReadsAreNot
	// does, so a route registered through secureRead by mistake is caught
	// here even though its response body is unaffected.
	var logbuf bytes.Buffer
	priorLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logbuf, nil)))
	defer slog.SetDefault(priorLogger)

	for name, token := range map[string]string{"another tenant": anotherTenantToken, "operator": operatorRoleToken} {
		w := call(h, "DELETE", "/v1/operations/"+operationID, "", token)
		if w.Code != 404 {
			t.Fatalf("%s: status=%d body=%s", name, w.Code, w.Body)
		}
		var envelope struct {
			Error struct{ Code string } `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "not_found" {
			t.Fatalf("%s: body=%s", name, w.Body)
		}
	}
	if strings.Contains(logbuf.String(), `"operator read"`) {
		t.Fatalf("the operator's refused DELETE was logged as an operator read -- this route must go through secure, not secureRead: %s", logbuf.String())
	}

	// Neither refusal wrote anything: the hold this tenant owns is untouched.
	if err := ledger.View(context.Background(), func(st *domain.State) error {
		if _, held := st.Allocations[allocationID]; !held {
			t.Fatal("another tenant's or the operator's refused DELETE removed the allocation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCancelOperationUnauthenticatedUsesTheExisting401 is the plain
// no-credential case every secure() endpoint already answers; cancelOperation
// adds no authentication of its own.
func TestCancelOperationUnauthenticatedUsesTheExisting401(t *testing.T) {
	h, _, _, operationID := stuckReservationBoundary(t)
	w := call(h, "DELETE", "/v1/operations/"+operationID, "", "wrong-token")
	if w.Code != 401 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}

// TestOperationsPathMethodHandlingIsUnchangedByCancel pins what a PUT or POST
// to /v1/operations/{id} answers today (nothing is registered for either
// method, so net/http.ServeMux answers 405) and shows adding the DELETE
// route did not change that -- the route addition must widen the accepted
// methods by exactly one.
func TestOperationsPathMethodHandlingIsUnchangedByCancel(t *testing.T) {
	h, _, _, operationID := stuckReservationBoundary(t)
	for _, method := range []string{"PUT", "POST", "PATCH"} {
		w := call(h, method, "/v1/operations/"+operationID, "", testToken)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/operations/{id}: status=%d, want %d", method, w.Code, http.StatusMethodNotAllowed)
		}
	}
}
