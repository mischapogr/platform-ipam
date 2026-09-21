package service

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

type testInventory struct {
	mu       sync.Mutex
	ensures  int
	adopts   []domain.Allocation
	adoptID  string
	networks []domain.Network
	abandons []domain.Allocation
	abandon  func(domain.Allocation, string, domain.PriorInventory) error
	cancels  []domain.Allocation
	cancel   func(domain.Allocation, string) error
}

func (f *testInventory) Snapshot(context.Context, domain.Domain) (domain.InventorySnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return domain.InventorySnapshot{Networks: append([]domain.Network(nil), f.networks...), Complete: true}, nil
}
func (f *testInventory) Ensure(_ context.Context, a domain.Allocation, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures++
	f.networks = append(f.networks, domain.Network{ID: a.ID, CIDR: a.CIDR, AllocationID: a.ID})
	return "nb_" + a.ID, nil
}

// Adopt records the call and refuses unless a test sets adoptID. A double that
// quietly succeeded would let a later change stop calling it without any test
// noticing, which is the one thing an adoption path must not be able to do.
func (f *testInventory) Adopt(_ context.Context, a domain.Allocation, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adopts = append(f.adopts, a)
	if f.adoptID == "" {
		return "", errors.New("adopt not configured")
	}
	f.networks = append(f.networks, domain.Network{ID: a.ID, CIDR: a.CIDR, AllocationID: a.ID})
	return f.adoptID, nil
}

// AbandonAdoption records the call and refuses unless a test sets abandon, for
// the reason Adopt refuses: nothing in internal/service calls it yet, so a
// double that succeeded by default would let the package that adds the caller
// reorder the fence, the clear and the delete without a test noticing.
func (f *testInventory) AbandonAdoption(_ context.Context, a domain.Allocation, operationID string, prior domain.PriorInventory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abandons = append(f.abandons, a)
	if f.abandon == nil {
		return errors.New("abandon not configured")
	}
	return f.abandon(a, operationID, prior)
}

// CancelReservation records the call and refuses unless a test sets cancel, for
// the reason AbandonAdoption refuses: nothing in internal/service calls it yet,
// so a double that succeeded by default would let the package that adds the
// caller reorder the fence, the removal and the delete without a test noticing
// -- and this one deletes where the abandon clears.
func (f *testInventory) CancelReservation(_ context.Context, a domain.Allocation, operationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, a)
	if f.cancel == nil {
		return errors.New("cancel not configured")
	}
	return f.cancel(a, operationID)
}
func (f *testInventory) Sync(context.Context, domain.Allocation) error   { return nil }
func (f *testInventory) Delete(context.Context, domain.Allocation) error { return nil }

type testObserver struct{ observation domain.Observation }

func (f testObserver) Observe(context.Context, domain.Domain) (domain.Observation, error) {
	return f.observation, nil
}

func testConfig() domain.Config {
	return domain.Config{PolicyVersion: "test", Domains: []domain.Domain{{ID: "d", CoverageGeneration: "g", CloudCoverage: []domain.Cell{{AccountID: "123456789012", Regions: []string{"eu"}}}}}, Pools: []domain.Pool{{ID: "p", DomainID: "d", CIDR: "10.0.0.0/16", AddressFamily: "ipv4", Scope: "vpc", Environment: "prod", Region: "eu", EligibleTenants: []string{"t"}, EligibleAccounts: []string{"123456789012"}, AllowedPrefixLengths: []int{24}}}, Lifecycle: domain.Lifecycle{MaxObservationAge: 600}}
}

func TestReserveUsesPermanentKeyAndExactCIDR(t *testing.T) {
	cfg := testConfig()
	inv := &testInventory{}
	started := time.Now().UTC().Add(-time.Second)
	obs := testObserver{observation: domain.Observation{DomainID: "d", Generation: "g", Complete: true, StartedAt: started, FinishedAt: started.Add(time.Second)}}
	s := New(cfg, storage.NewMemoryLedger(), inv, obs)
	p := domain.Principal{TenantID: "t", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu"}}
	r := domain.Request{AllocationKey: "orders", Scope: "vpc", Environment: "prod", Region: "eu", PrefixLength: 24, Description: "old", Labels: map[string]string{"x": "1"}}
	a, _, status, err := s.Reserve(context.Background(), p, r, "retry-1")
	if err != nil || status != 201 {
		t.Fatalf("reserve: status=%d err=%v", status, err)
	}
	r.Description = "changed"
	r.Labels = map[string]string{"x": "2"}
	b, _, status, err := s.Reserve(context.Background(), p, r, "retry-2")
	if err != nil || status != 200 {
		t.Fatalf("key recovery: status=%d err=%v", status, err)
	}
	if a.ID != b.ID || a.CIDR != b.CIDR {
		t.Fatalf("permanent identity changed: %#v %#v", a, b)
	}
	inv.mu.Lock()
	n := inv.ensures
	inv.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected one exact external create, got %d", n)
	}
}

func TestMemoryUpdateRollsBackOnError(t *testing.T) {
	l := storage.NewMemoryLedger()
	ctx := context.Background()
	id := domain.NewID("alloc")
	err := l.Update(ctx, func(st *domain.State) error { st.Allocations[id] = domain.Allocation{ID: id}; return context.Canceled })
	if err == nil {
		t.Fatal("expected closure error")
	}
	s, err := l.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Allocations) != 0 {
		t.Fatalf("failed transaction committed: %#v", s.Allocations)
	}
}

func TestReserveSubnetUsesCommittedParentRange(t *testing.T) {
	cfg := testConfig()
	cfg.SubnetPolicy.AllowedPrefixLengths = []int{26}
	inv := &testInventory{}
	started := time.Now().UTC().Add(-time.Second)
	obs := testObserver{observation: domain.Observation{DomainID: "d", Generation: "g", Complete: true, StartedAt: started, FinishedAt: started.Add(time.Second)}}
	s := New(cfg, storage.NewMemoryLedger(), inv, obs)
	p := domain.Principal{TenantID: "t", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu"}}
	parentReq := domain.Request{AllocationKey: "parent", Scope: "vpc", Environment: "prod", Region: "eu", PrefixLength: 24}
	parent, _, status, err := s.Reserve(context.Background(), p, parentReq, "parent-http")
	if err != nil || status != 201 {
		t.Fatalf("parent reserve: %d %v", status, err)
	}
	childReq := domain.Request{AllocationKey: "child", Scope: "subnet", Environment: "prod", Region: "eu", PrefixLength: 26, ParentAllocationID: parent.ID, AvailabilityZoneID: "euc1-az1"}
	child, _, status, err := s.Reserve(context.Background(), p, childReq, "child-http")
	if err != nil || status != 201 {
		t.Fatalf("child reserve: %d %v", status, err)
	}
	if child.CIDR == "" {
		t.Fatal("child CIDR missing")
	}
	parentPrefix, _ := netip.ParsePrefix(parent.CIDR)
	childPrefix, _ := netip.ParsePrefix(child.CIDR)
	if !parentPrefix.Contains(childPrefix.Addr()) {
		t.Fatalf("child %s outside parent %s", child.CIDR, parent.CIDR)
	}
}
