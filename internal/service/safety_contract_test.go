package service

// These tests encode v1 safety guarantees at the service boundary. They use
// the in-memory ledger only to make adverse external observations deterministic;
// they do not assert on service-private state transitions or hash functions.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

type safetyInventory struct {
	networks   []domain.Network
	deletes    int
	ensures    int
	incomplete bool
	adopts     []domain.Allocation
	adoptID    string
	// adoptErr is the adapter's own answer: a refusal it reached on evidence it
	// read, or an error that says nothing at all. Only an adapter can tell those
	// apart, so a test that wants one of them says which.
	adoptErr error
	abandons []domain.Allocation
	abandon  func(domain.Allocation, string, domain.PriorInventory) error
	cancels  []domain.Allocation
	cancel   func(domain.Allocation, string) error
	// ensureErr and ensureEmpty (package H4) are Ensure's own answer, exactly
	// as adoptErr is Adopt's: a refusal reached on evidence read, an error
	// that says nothing at all, or -- defensively -- no id and no error.
	// Both are nil/false by default, which is today's unconditional success.
	ensureErr   error
	ensureEmpty bool
	// onEnsure runs inside the adapter call, after it has been counted and
	// before it answers. It is the only place a test can make something else
	// happen while a request is between persisting its hold and committing it
	// -- the window ADR 0013 says a cancel's fence can land in.
	onEnsure func()
}

func (i *safetyInventory) Snapshot(context.Context, domain.Domain) (domain.InventorySnapshot, error) {
	return domain.InventorySnapshot{Networks: append([]domain.Network(nil), i.networks...), Complete: !i.incomplete}, nil
}
func (i *safetyInventory) Ensure(_ context.Context, a domain.Allocation, _ string) (string, error) {
	i.ensures++
	if i.onEnsure != nil {
		i.onEnsure()
	}
	if i.ensureErr != nil {
		return "", i.ensureErr
	}
	if i.ensureEmpty {
		return "", nil
	}
	i.networks = append(i.networks, domain.Network{ID: a.ID, CIDR: a.CIDR, AllocationID: a.ID})
	return "netbox-" + a.ID, nil
}

// Adopt records the call and refuses unless a test sets adoptID. These tests
// exist to prove that no path other than the reviewed one mints a committed
// allocation, so a double that succeeded by default would weaken exactly the
// guarantee the file is here to hold.
func (i *safetyInventory) Adopt(_ context.Context, a domain.Allocation, _ string) (string, error) {
	i.adopts = append(i.adopts, a)
	if i.adoptErr != nil {
		return "", i.adoptErr
	}
	if i.adoptID == "" {
		return "", errors.New("adopt not configured")
	}
	i.networks = append(i.networks, domain.Network{ID: a.ID, CIDR: a.CIDR, AllocationID: a.ID})
	return i.adoptID, nil
}

// AbandonAdoption records the call and refuses unless a test sets abandon.
// These tests exist to prove that no path other than the reviewed one mints a
// committed allocation and that nothing removes one; a double that cleared an
// inventory object by default would weaken exactly that.
func (i *safetyInventory) AbandonAdoption(_ context.Context, a domain.Allocation, operationID string, prior domain.PriorInventory) error {
	i.abandons = append(i.abandons, a)
	if i.abandon == nil {
		return errors.New("abandon not configured")
	}
	return i.abandon(a, operationID, prior)
}

// CancelReservation records the call and refuses unless a test sets cancel.
// These tests exist to prove that no path other than the reviewed one mints a
// committed allocation and that nothing removes one; a double that deleted an
// inventory object by default would weaken exactly that, and this is the one
// port operation that deletes.
func (i *safetyInventory) CancelReservation(_ context.Context, a domain.Allocation, operationID string) error {
	i.cancels = append(i.cancels, a)
	if i.cancel == nil {
		return errors.New("cancel not configured")
	}
	return i.cancel(a, operationID)
}
func (i *safetyInventory) Sync(context.Context, domain.Allocation) error { return nil }
func (i *safetyInventory) Delete(context.Context, domain.Allocation) error {
	i.deletes++
	return nil
}

type safetyObserver struct {
	observation domain.Observation
	err         error
}

func (o *safetyObserver) Observe(context.Context, domain.Domain) (domain.Observation, error) {
	copy := o.observation
	copy.Resources = append([]domain.Resource(nil), o.observation.Resources...)
	return copy, o.err
}

func safetyPrincipal() domain.Principal {
	return domain.Principal{
		TenantID: "t", Accounts: []string{"123456789012"},
		Environments: []string{"prod"}, Regions: []string{"eu"},
	}
}

func safetyRequest(key string) domain.Request {
	return domain.Request{
		AllocationKey: key, Scope: "vpc", Environment: "prod", Region: "eu",
		PrefixLength: 24, Description: "first", Labels: map[string]string{"service": "orders"},
	}
}

func safetyObservation(now time.Time, resources ...domain.Resource) domain.Observation {
	return domain.Observation{
		DomainID: "d", Generation: "g", Complete: true,
		StartedAt: now.Add(-time.Minute), FinishedAt: now, Resources: resources,
	}
}

func safetyConfig() domain.Config {
	return domain.Config{
		PolicyVersion: "test",
		Domains: []domain.Domain{{
			ID: "d", CoverageGeneration: "g",
			CloudCoverage: []domain.Cell{{AccountID: "123456789012", Regions: []string{"eu"}}},
		}},
		Pools: []domain.Pool{{
			ID: "p", DomainID: "d", CIDR: "10.0.0.0/8", AddressFamily: "ipv4",
			Scope: "vpc", Environment: "prod", Region: "eu", EligibleTenants: []string{"t"},
			EligibleAccounts: []string{"123456789012"}, AllowedPrefixLengths: []int{24},
		}},
	}
}

func safetyService(now time.Time, ledger *storage.MemoryLedger, inventory *safetyInventory, observer *safetyObserver) *Service {
	cfg := safetyConfig()
	cfg.Lifecycle = domain.Lifecycle{
		QuarantineHours: 1, RequiredAbsenceScans: 2, MinScanSpacing: 300, MaxObservationAge: 3600,
	}
	s := New(cfg, ledger, inventory, observer)
	s.SetClock(func() time.Time { return now })
	return s
}

func safetyAPIError(t *testing.T, err error, wantStatus int, wantCode string) {
	t.Helper()
	var got *domain.APIError
	if !errors.As(err, &got) || got.Status != wantStatus || got.Code != wantCode {
		t.Fatalf("want API error %d/%s, got %#v", wantStatus, wantCode, err)
	}
}

func safetyAllocation(now time.Time, id, scope, cidr, parent, az string) domain.Allocation {
	return domain.Allocation{
		Request: domain.Request{
			AllocationKey: id + "-key", Scope: scope, Environment: "prod", Region: "eu",
			AccountID: "123456789012", AddressFamily: "ipv4", PrefixLength: 24,
			ParentAllocationID: parent, AvailabilityZoneID: az,
		},
		ID: id, TenantID: "t", DomainID: "d", CIDR: cidr, PoolID: "p", State: domain.Reserved,
		Revision: 1, PolicyVersion: "test", InventoryID: "netbox-" + id, InventorySync: "CURRENT",
		CreatedAt: now, UpdatedAt: now, Committed: true,
	}
}

func putSafetyAllocation(t *testing.T, ledger *storage.MemoryLedger, a domain.Allocation) {
	t.Helper()
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		st.Allocations[a.ID] = a
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func safetyState(t *testing.T, ledger *storage.MemoryLedger, id string) domain.Allocation {
	t.Helper()
	st, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, ok := st.Allocations[id]
	if !ok {
		t.Fatalf("missing allocation %q", id)
	}
	return a
}

// ADR 0013's third property, and it is new because the actor is new: a cancel
// is the first write a tenant can reach that removes a ledger row, so "no
// tenant, and no operator, can affect another tenant's hold" belongs beside the
// other guarantees of this boundary and not only in the file that implements
// it. The refusal rests on the tenant comparison Service.Operation already
// makes -- an operator is a principal with no tenant (ADR 0011) and is refused
// by a comparison that does not know the role exists. The fixture is a real
// stuck reservation (cancel_test.go).
func TestSafetyAnotherTenantAndAnOperatorCannotCancelAHold(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for name, principal := range map[string]func() domain.Principal{
		"another_tenant": tenantUPrincipal,
		"an_operator":    operatorPrincipal,
	} {
		t.Run(name, func(t *testing.T) {
			c := stuckReservation(t, now)
			report, err := c.service.CancelReservation(context.Background(), principal(), c.operation.ID)
			safetyAPIError(t, err, 404, "not_found")
			if report != nil {
				t.Fatalf("a refused cancel returned a report: %#v", report)
			}
			if len(c.inventory.cancels) != 0 {
				t.Fatalf("a refused cancel reached the inventory: %#v", c.inventory.cancels)
			}
			held, ok := ledgerState(t, c.ledger).Allocations[c.allocation.ID]
			if !ok || held.Committed {
				t.Fatalf("another principal's cancel changed the hold: %#v", held)
			}
		})
	}
}

func TestSafetySameHTTPKeyWithChangedMutableFieldsConflicts(t *testing.T) {
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	ledger, inventory := storage.NewMemoryLedger(), &safetyInventory{}
	observer := &safetyObserver{observation: safetyObservation(now)}
	s := safetyService(now, ledger, inventory, observer)

	if _, _, status, err := s.Reserve(context.Background(), safetyPrincipal(), safetyRequest("orders"), "same-http-key"); err != nil || status != 201 {
		t.Fatalf("initial reservation: status=%d err=%v", status, err)
	}
	retry := safetyRequest("orders")
	retry.Description = "different mutable description"
	retry.Labels = map[string]string{"service": "billing"}
	_, _, _, err := s.Reserve(context.Background(), safetyPrincipal(), retry, "same-http-key")
	safetyAPIError(t, err, 409, "idempotency_mismatch")
}

func TestSafetyRetiredPOSTReplayNeverReturnsPriorAllocation(t *testing.T) {
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	ledger, inventory := storage.NewMemoryLedger(), &safetyInventory{}
	s := safetyService(now, ledger, inventory, &safetyObserver{observation: safetyObservation(now)})
	req := safetyRequest("retired-orders")
	a, _, status, err := s.Reserve(context.Background(), safetyPrincipal(), req, "original-http-key")
	if err != nil || status != 201 {
		t.Fatalf("initial reservation: status=%d err=%v", status, err)
	}
	if _, status, err = s.Release(context.Background(), safetyPrincipal(), a.ID); err != nil || status != 202 {
		t.Fatalf("release: status=%d err=%v", status, err)
	}
	_, _, _, err = s.Reserve(context.Background(), safetyPrincipal(), req, "original-http-key")
	safetyAPIError(t, err, 409, "allocation_key_retired")
}

func TestSafetyReservationRejectsUntrustworthyCompleteObservations(t *testing.T) {
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	for name, observation := range map[string]domain.Observation{
		"missing_started_at": {DomainID: "d", Generation: "g", Complete: true, FinishedAt: now},
		"wrong_generation":   {DomainID: "d", Generation: "old-generation", Complete: true, StartedAt: now.Add(-time.Minute), FinishedAt: now},
	} {
		t.Run(name, func(t *testing.T) {
			ledger, inventory := storage.NewMemoryLedger(), &safetyInventory{}
			s := safetyService(now, ledger, inventory, &safetyObserver{observation: observation})
			_, _, _, err := s.Reserve(context.Background(), safetyPrincipal(), safetyRequest("untrusted-"+name), "key-"+name)
			safetyAPIError(t, err, 503, "dependency_unavailable")
		})
	}
}

func TestSafetyBindingRequiresPrimaryCIDRParentAndAZ(t *testing.T) {
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	baseTags := func(id, key string) map[string]string {
		return map[string]string{"platform-ipam:allocation-id": id, "platform-ipam:allocation-key": key}
	}
	cases := []struct {
		name     string
		setup    func(*storage.MemoryLedger)
		id       string
		binding  domain.Binding
		resource domain.Resource
	}{
		{
			name: "vpc_secondary_match_cannot_activate",
			id:   "alloc-vpc",
			setup: func(l *storage.MemoryLedger) {
				putSafetyAllocation(t, l, safetyAllocation(now, "alloc-vpc", "vpc", "10.0.0.0/24", "", ""))
			},
			binding:  domain.Binding{Provider: "aws", ResourceType: "vpc", ResourceID: "vpc-1", AccountID: "123456789012", Region: "eu"},
			resource: domain.Resource{Type: "vpc", ID: "vpc-1", AccountID: "123456789012", Region: "eu", CIDR: "10.0.1.0/24", CIDRs: []string{"10.0.1.0/24", "10.0.0.0/24"}, Tags: baseTags("alloc-vpc", "alloc-vpc-key")},
		},
		{
			name: "subnet_wrong_parent_cannot_activate",
			id:   "alloc-subnet-parent",
			setup: func(l *storage.MemoryLedger) {
				parent := safetyAllocation(now, "alloc-parent", "vpc", "10.1.0.0/16", "", "")
				parent.State = domain.Active
				parent.Binding = &domain.Binding{Provider: "aws", ResourceType: "vpc", ResourceID: "vpc-parent", AccountID: "123456789012", Region: "eu"}
				putSafetyAllocation(t, l, parent)
				putSafetyAllocation(t, l, safetyAllocation(now, "alloc-subnet-parent", "subnet", "10.1.1.0/24", parent.ID, "euc1-az1"))
			},
			binding:  domain.Binding{Provider: "aws", ResourceType: "subnet", ResourceID: "subnet-1", AccountID: "123456789012", Region: "eu"},
			resource: domain.Resource{Type: "subnet", ID: "subnet-1", AccountID: "123456789012", Region: "eu", CIDR: "10.1.1.0/24", CIDRs: []string{"10.1.1.0/24"}, ParentID: "vpc-other", ZoneID: "euc1-az1", Tags: baseTags("alloc-subnet-parent", "alloc-subnet-parent-key")},
		},
		{
			name: "subnet_wrong_az_cannot_activate",
			id:   "alloc-subnet-az",
			setup: func(l *storage.MemoryLedger) {
				parent := safetyAllocation(now, "alloc-parent-az", "vpc", "10.2.0.0/16", "", "")
				parent.State = domain.Active
				parent.Binding = &domain.Binding{Provider: "aws", ResourceType: "vpc", ResourceID: "vpc-parent-az", AccountID: "123456789012", Region: "eu"}
				putSafetyAllocation(t, l, parent)
				putSafetyAllocation(t, l, safetyAllocation(now, "alloc-subnet-az", "subnet", "10.2.1.0/24", parent.ID, "euc1-az1"))
			},
			binding:  domain.Binding{Provider: "aws", ResourceType: "subnet", ResourceID: "subnet-az", AccountID: "123456789012", Region: "eu"},
			resource: domain.Resource{Type: "subnet", ID: "subnet-az", AccountID: "123456789012", Region: "eu", CIDR: "10.2.1.0/24", CIDRs: []string{"10.2.1.0/24"}, ParentID: "vpc-parent-az", ZoneID: "euc1-az2", Tags: baseTags("alloc-subnet-az", "alloc-subnet-az-key")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger, inventory := storage.NewMemoryLedger(), &safetyInventory{}
			tc.setup(ledger)
			s := safetyService(now, ledger, inventory, &safetyObserver{observation: safetyObservation(now, tc.resource)})
			a, op, status, err := s.Bind(context.Background(), safetyPrincipal(), tc.id, tc.binding, "bind-"+tc.name)
			if err != nil || status != 202 || a != nil || op == nil || op.Status == operationSucceeded {
				t.Fatalf("invalid candidate must never activate: allocation=%#v operation=%#v status=%d err=%v", a, op, status, err)
			}
			if got := safetyState(t, ledger, tc.id); got.State != domain.Reserved || got.Binding != nil {
				t.Fatalf("invalid binding changed allocation: %#v", got)
			}
		})
	}
}

func TestSafetyReleaseWinsOverDelayedBindingObservation(t *testing.T) {
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	ledger, inventory := storage.NewMemoryLedger(), &safetyInventory{}
	a := safetyAllocation(now, "alloc-release-race", "vpc", "10.3.0.0/24", "", "")
	putSafetyAllocation(t, ledger, a)
	observer := &safetyObserver{observation: safetyObservation(now)}
	s := safetyService(now, ledger, inventory, observer)
	binding := domain.Binding{Provider: "aws", ResourceType: "vpc", ResourceID: "vpc-race", AccountID: "123456789012", Region: "eu"}
	if got, op, status, err := s.Bind(context.Background(), safetyPrincipal(), a.ID, binding, "bind-race"); err != nil || status != 202 || got != nil || op == nil {
		t.Fatalf("expected unresolved binding: allocation=%#v operation=%#v status=%d err=%v", got, op, status, err)
	}
	if _, status, err := s.Release(context.Background(), safetyPrincipal(), a.ID); err != nil || status != 202 {
		t.Fatalf("release: status=%d err=%v", status, err)
	}
	observer.observation = safetyObservation(now, domain.Resource{
		Type: "vpc", ID: "vpc-race", AccountID: "123456789012", Region: "eu", CIDR: "10.3.0.0/24",
		Tags: map[string]string{"platform-ipam:allocation-id": a.ID, "platform-ipam:allocation-key": a.AllocationKey},
	})
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := safetyState(t, ledger, a.ID); got.State != domain.Quarantined || got.Binding != nil {
		t.Fatalf("release was bypassed by delayed binding result: %#v", got)
	}
}

func TestSafetyReclaimRequiresAbsenceAfterReleaseAndNoNewUnknown(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for name, observations := range map[string][]domain.Observation{
		"absence_before_release": {
			safetyObservation(now.Add(-20 * time.Minute)), safetyObservation(now.Add(-10 * time.Minute)),
		},
		"incomplete_latest_resets_streak": {
			safetyObservation(now.Add(-20 * time.Minute)), safetyObservation(now.Add(-10 * time.Minute)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			ledger, inventory := storage.NewMemoryLedger(), &safetyInventory{}
			a := safetyAllocation(now, "alloc-reclaim-"+name, "vpc", "10.4.0.0/24", "", "")
			releasedAt := now.Add(-5 * time.Minute)
			quarantineUntil := now.Add(-time.Minute)
			if name == "incomplete_latest_resets_streak" {
				releasedAt = now.Add(-30 * time.Minute)
			}
			a.State, a.ReleaseRequestedAt, a.QuarantineUntil = domain.Quarantined, &releasedAt, &quarantineUntil
			putSafetyAllocation(t, ledger, a)
			if err := ledger.Update(context.Background(), func(st *domain.State) error {
				st.Observations["d"] = append([]domain.Observation(nil), observations...)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			unknown := domain.Observation{DomainID: "d", Generation: "g", Complete: false, StartedAt: now.Add(-time.Minute), FinishedAt: now, Error: "access denied"}
			s := safetyService(now, ledger, inventory, &safetyObserver{observation: unknown})
			if name == "absence_before_release" {
				if err := s.reclaimTick(context.Background()); err != nil {
					t.Fatalf("reclaim tick: %v", err)
				}
			} else if err := s.Tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if inventory.deletes != 0 || safetyState(t, ledger, a.ID).State != domain.Quarantined {
				t.Fatalf("reclaimed without qualifying current absence evidence: deletes=%d state=%s", inventory.deletes, safetyState(t, ledger, a.ID).State)
			}
		})
	}
}
