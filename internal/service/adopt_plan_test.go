package service

// PlanAdoption is the read-only verdict package F4's `adopt plan` needs, and
// these tests hold it to the same safety properties as adopt_test.go's Adopt
// tests, from the other side: every one of them proves PlanAdoption wrote
// nothing (no allocation, no operation, no event, no request, and never an
// Inventory.Adopt call) whatever verdict it returned.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

// assertPlanWroteNothing is assertNothingWritten's counterpart for
// PlanAdoption: identical shape, distinct name, so a failure names the
// function under test correctly.
func assertPlanWroteNothing(t *testing.T, ledger *storage.MemoryLedger, inventory *safetyInventory) {
	t.Helper()
	assertNothingWritten(t, ledger, inventory)
}

func TestPlanAdoptionOfAFreshRecordReportsTheVerifiedCandidateAndWritesNothing(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)

	v, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil {
		t.Fatalf("PlanAdoption: %v", err)
	}
	if v.Existing || v.Pending {
		t.Fatalf("a never-before-seen key is neither existing nor pending: %#v", v)
	}
	if v.Allocation.ID != "" || v.Allocation.Committed {
		t.Fatalf("a plan verdict must not look like a minted allocation: %#v", v.Allocation)
	}
	if v.Allocation.CIDR != adoptedCIDR || v.Allocation.PoolID != "p" || v.Allocation.DomainID != "d" || v.Allocation.TenantID != "t" {
		t.Fatalf("verdict did not report the verified candidate: %#v", v.Allocation)
	}
	if v.Allocation.PrefixLength != 24 {
		t.Fatalf("verdict did not derive the prefix length from the CIDR: %#v", v.Allocation)
	}
	assertPlanWroteNothing(t, ledger, inventory)
}

func TestPlanAdoptionOfAnAlreadyCommittedAdoptionReportsExistingNotPending(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)

	committed, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 {
		t.Fatalf("adopt: status=%d err=%v", status, err)
	}
	before := ledgerState(t, ledger)

	v, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil {
		t.Fatalf("PlanAdoption: %v", err)
	}
	if !v.Existing || v.Pending {
		t.Fatalf("a committed adoption must replay as existing and not pending: %#v", v)
	}
	if v.Allocation.ID != committed.ID || v.Allocation.CIDR != committed.CIDR || !v.Allocation.Committed {
		t.Fatalf("verdict did not report the committed allocation: %#v", v.Allocation)
	}
	after := ledgerState(t, ledger)
	if len(after.Allocations) != len(before.Allocations) || len(after.Operations) != len(before.Operations) ||
		len(after.Events) != len(before.Events) || len(after.Requests) != len(before.Requests) {
		t.Fatalf("PlanAdoption changed ledger state: before=%#v after=%#v", before, after)
	}
	if len(inventory.adopts) != 1 {
		t.Fatalf("PlanAdoption called Inventory.Adopt again: %#v", inventory.adopts)
	}
}

// TestPlanAdoptionOfAPendingAdoptionReportsExistingAndPending exercises the
// case a crash between the hold and the commit leaves behind: the ledger
// already carries an ADOPT_PLANNED hold for this exact key and pin, but no
// commit -- exactly the state TestAdoptDoesNotCommitWhenTheInventoryConverted
// AnotherObject in adopt_test.go leaves the ledger in.
func TestPlanAdoptionOfAPendingAdoptionReportsExistingAndPending(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)
	inventory.adoptID = "9999" // a foreign inventory id: the commit is refused, the hold stays pending

	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_conflict")
	before := ledgerState(t, ledger)
	if len(before.Allocations) != 1 {
		t.Fatalf("setup: want one pending hold, got %d allocations", len(before.Allocations))
	}

	v, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil {
		t.Fatalf("PlanAdoption: %v", err)
	}
	if !v.Existing || !v.Pending {
		t.Fatalf("a pending hold must report existing and pending: %#v", v)
	}
	if v.Allocation.Committed {
		t.Fatalf("a pending hold reported as committed: %#v", v.Allocation)
	}
	after := ledgerState(t, ledger)
	if len(after.Allocations) != len(before.Allocations) || len(after.Operations) != len(before.Operations) ||
		len(after.Events) != len(before.Events) {
		t.Fatalf("PlanAdoption changed ledger state: before=%#v after=%#v", before, after)
	}
}

func TestPlanAdoptionRefusesWithoutAnOperatorSubjectAndWritesNothing(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)

	anonymous := adoptPin()
	anonymous.Operator = ""
	_, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), anonymous)
	safetyAPIError(t, err, 422, "invalid_request")
	assertPlanWroteNothing(t, ledger, inventory)
}

func TestPlanAdoptionRefusesAnIncompleteSnapshotAndWritesNothing(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)
	inventory.incomplete = true

	_, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 503, "dependency_unavailable")
	assertPlanWroteNothing(t, ledger, inventory)
}

func TestPlanAdoptionRejectsUntrustworthyCompleteObservations(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for name, observation := range map[string]domain.Observation{
		"missing_started_at": {DomainID: "d", Generation: "g", Complete: true, FinishedAt: now, Resources: []domain.Resource{adoptedResource()}},
		"wrong_generation":   {DomainID: "d", Generation: "old-generation", Complete: true, StartedAt: now.Add(-time.Minute), FinishedAt: now, Resources: []domain.Resource{adoptedResource()}},
		"incomplete":         {DomainID: "d", Generation: "g", Complete: false, StartedAt: now.Add(-time.Minute), FinishedAt: now, Resources: []domain.Resource{adoptedResource()}},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := storage.NewMemoryLedger()
			inventory := &safetyInventory{networks: []domain.Network{adoptedNetwork()}, adoptID: adoptedNetworkID}
			s := adoptService(now, ledger, inventory, &safetyObserver{observation: observation})
			_, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
			safetyAPIError(t, err, 503, "dependency_unavailable")
			assertPlanWroteNothing(t, ledger, inventory)
		})
	}
}

func TestPlanAdoptionRefusesASecondUnmanagedPrefixAndWritesNothing(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	second := domain.Network{ID: "4243", CIDR: "10.7.0.128/25", Imported: true, ImportBatch: adoptedBatch}
	s, ledger, inventory := adoptFixture(now, []domain.Network{adoptedNetwork(), second}, []domain.Resource{adoptedResource()})

	_, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	assertPlanWroteNothing(t, ledger, inventory)
}

func TestPlanAdoptionRefusesASecondObservedResourceAndWritesNothing(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	second := domain.Resource{
		Type: "vpc", ID: "vpc-neighbour", AccountID: "123456789012", Region: "eu",
		CIDR: "10.7.0.128/25", CIDRs: []string{"10.7.0.128/25"},
	}
	s, ledger, inventory := adoptFixture(now, []domain.Network{adoptedNetwork()}, []domain.Resource{adoptedResource(), second})

	_, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	assertPlanWroteNothing(t, ledger, inventory)
}

func TestPlanAdoptionRefusesACIDRThatIsNotTheRequestedShape(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		cidr   string
		status int
		code   string
	}{
		"prefix_length_disagrees": {"10.7.0.0/25", 422, "invalid_request"},
		"not_canonical":           {"10.7.0.1/24", 422, "invalid_request"},
		"outside_the_pool":        {"192.168.4.0/24", 422, "policy_violation"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, ledger, inventory := adoptReady(now)
			pin := adoptPin()
			pin.CIDR = tc.cidr
			_, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), pin)
			safetyAPIError(t, err, tc.status, tc.code)
			assertPlanWroteNothing(t, ledger, inventory)
		})
	}
}

func TestPlanAdoptionRefusesWhenAnAllocationAlreadyHoldsTheCIDR(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)
	existing := safetyAllocation(now, "alloc-existing", "vpc", adoptedCIDR, "", "")
	putSafetyAllocation(t, ledger, existing)

	_, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	if len(inventory.adopts) != 0 {
		t.Fatalf("PlanAdoption called Inventory.Adopt: %#v", inventory.adopts)
	}
	st := ledgerState(t, ledger)
	if len(st.Allocations) != 1 || len(st.Operations) != 0 || len(st.Events) != 0 || len(st.Requests) != 0 {
		t.Fatalf("PlanAdoption wrote to the ledger: %d allocations, %d operations, %d events, %d requests",
			len(st.Allocations), len(st.Operations), len(st.Events), len(st.Requests))
	}
}

// A key that already names a consumer's allocation must be reported as a
// conflict, not silently treated as "would create a fresh adoption": the
// request hash cannot tell a replay of this adoption from a replay of
// something else, so PlanAdoption compares the existing record with the pin
// before anything else, exactly as Adopt does.
func TestPlanAdoptionRefusesAKeyThatAlreadyHoldsAnotherCIDR(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)

	reserved, _, status, err := s.Reserve(context.Background(), safetyPrincipal(), safetyRequest("orders"), "consumer-key-1")
	if err != nil || status != 201 || reserved.CIDR == adoptedCIDR {
		t.Fatalf("consumer reservation: allocation=%#v status=%d err=%v", reserved, status, err)
	}
	before := ledgerState(t, ledger)

	_, err = s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_conflict")
	if len(inventory.adopts) != 0 {
		t.Fatalf("PlanAdoption called Inventory.Adopt: %#v", inventory.adopts)
	}
	after := ledgerState(t, ledger)
	if len(after.Allocations) != len(before.Allocations) || len(after.Requests) != len(before.Requests) {
		t.Fatalf("PlanAdoption changed ledger state: before=%#v after=%#v", before, after)
	}
	for _, event := range allocationEvents(after, reserved.ID) {
		if strings.HasPrefix(event.Action, "ADOPT") {
			t.Fatalf("the consumer's allocation gained an adoption event: %#v", event)
		}
	}
}

func TestPlanAdoptionRefusesAPrincipalWithoutATenant(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptReady(now)
	p := safetyPrincipal()
	p.TenantID = ""

	_, err := s.PlanAdoption(context.Background(), p, safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 403, "forbidden")
	assertPlanWroteNothing(t, ledger, inventory)
}

// The point of PlanAdoption existing at all: apply calls Adopt afterwards for
// the very same record, and the two must agree -- plan cannot promise success
// that apply then refuses, or refuse a record apply would actually adopt.
func TestPlanAdoptionAgreesWithAdoptOnTheSameRecord(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, _, _ := adoptReady(now)

	v, err := s.PlanAdoption(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil {
		t.Fatalf("PlanAdoption: %v", err)
	}
	if v.Existing {
		t.Fatalf("want a fresh verdict, got %#v", v)
	}
	a, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 {
		t.Fatalf("adopt: status=%d err=%v", status, err)
	}
	if a.CIDR != v.Allocation.CIDR || a.PoolID != v.Allocation.PoolID || a.DomainID != v.Allocation.DomainID || a.PrefixLength != v.Allocation.PrefixLength {
		t.Fatalf("plan and apply disagreed: plan=%#v apply=%#v", v.Allocation, *a)
	}
}

// PlanAdoption repeats, outside the write transaction, guards that reserve
// applies inside it. Sharing pinnedCIDR keeps the overlap rules in one place,
// but the sequence around it is a second copy, and a second copy drifts. This
// is the test that notices: every scenario is built twice, identically, and
// plan on one must give exactly the answer Adopt gives on the other -- the same
// status and code for a refusal, the same CIDR and pool for an adoption.
func TestPlanAdoptionGivesAdoptsAnswerInEveryScenario(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	type world struct {
		service   *Service
		principal domain.Principal
		request   domain.Request
		pin       Adoption
	}
	ready := func() world {
		s, _, _ := adoptReady(now)
		return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
	}
	scenarios := map[string]func(t *testing.T) world{
		"adoptable": func(t *testing.T) world { return ready() },
		"second_unmanaged_prefix": func(t *testing.T) world {
			second := domain.Network{ID: "4243", CIDR: "10.7.0.128/25", Imported: true, ImportBatch: adoptedBatch}
			s, _, _ := adoptFixture(now, []domain.Network{adoptedNetwork(), second}, []domain.Resource{adoptedResource()})
			return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
		},
		"reviewed_network_missing": func(t *testing.T) world {
			s, _, _ := adoptFixture(now, nil, []domain.Resource{adoptedResource()})
			return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
		},
		"reviewed_resource_missing": func(t *testing.T) world {
			s, _, _ := adoptFixture(now, []domain.Network{adoptedNetwork()}, nil)
			return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
		},
		"second_resource": func(t *testing.T) world {
			other := adoptedResource()
			other.ID = "vpc-someone-else"
			s, _, _ := adoptFixture(now, []domain.Network{adoptedNetwork()}, []domain.Resource{adoptedResource(), other})
			return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
		},
		"prefix_length_disagrees": func(t *testing.T) world {
			w := ready()
			w.pin.CIDR = "10.7.0.0/25"
			return w
		},
		"outside_the_pool": func(t *testing.T) world {
			w := ready()
			w.pin.CIDR = "192.168.4.0/24"
			return w
		},
		"excluded_space": func(t *testing.T) world {
			w := ready()
			w.service.cfg.Pools[0].ExcludedCIDRs = []string{"10.7.0.0/23"}
			return w
		},
		"incomplete_snapshot": func(t *testing.T) world {
			s, _, inventory := adoptReady(now)
			inventory.incomplete = true
			return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
		},
		"no_operator": func(t *testing.T) world {
			w := ready()
			w.pin.Operator = ""
			return w
		},
		"no_tenant": func(t *testing.T) world {
			w := ready()
			w.principal.TenantID = ""
			return w
		},
		"key_already_holds_another_cidr": func(t *testing.T) world {
			w := ready()
			if _, _, status, err := w.service.Reserve(context.Background(), w.principal, safetyRequest("orders"), "consumer-key-1"); err != nil || status != 201 {
				t.Fatalf("seeding reservation: status=%d err=%v", status, err)
			}
			return w
		},
		"quota_exhausted": func(t *testing.T) world {
			w := ready()
			w.service.cfg.Pools[0].MaxAllocations = 1
			if _, _, status, err := w.service.Reserve(context.Background(), w.principal, safetyRequest("billing"), "consumer-key-2"); err != nil || status != 201 {
				t.Fatalf("seeding reservation: status=%d err=%v", status, err)
			}
			return w
		},
		"already_adopted": func(t *testing.T) world {
			w := ready()
			if _, _, status, err := w.service.Adopt(context.Background(), w.principal, w.request, w.pin); err != nil || status != 201 {
				t.Fatalf("seeding adoption: status=%d err=%v", status, err)
			}
			return w
		},
		// Adoption's third exemption (ADR 0010, amended 2026-09-20). plan and
		// apply share reviewedVPCChildren through pinnedCIDR, but the sequence
		// around it is a second copy in each, so the scenarios that matter most
		// for the amendment belong in exactly this test.
		"vpc_with_two_children": func(t *testing.T) world {
			networks, resources := vpcWithChildren()
			s, _, _ := adoptFixture(now, networks, resources)
			return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
		},
		"child_of_another_vpc": func(t *testing.T) world {
			stranger := childResource(childAResourceID, childACIDR, childAZone)
			stranger.ParentID = "vpc-somebody-elses"
			s, _, _ := adoptFixture(now,
				[]domain.Network{adoptedNetwork(), childNetwork(childANetworkID, childACIDR)},
				[]domain.Resource{adoptedResource(), stranger})
			return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
		},
		"orphan_imported_prefix_inside_the_pin": func(t *testing.T) world {
			s, _, _ := adoptFixture(now,
				[]domain.Network{adoptedNetwork(), childNetwork(childANetworkID, childACIDR)},
				[]domain.Resource{adoptedResource()})
			return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
		},
		// The window between an abandon's fence and its delete (ADR 0012,
		// package H2b). The hold is uncommitted with no pending operation,
		// which both replay scans would otherwise report as 202, so plan and
		// apply have to refuse it and refuse it the same way.
		"abandon_in_progress": func(t *testing.T) world {
			s, _, inventory := adoptReady(now)
			inventory.adoptErr = errors.New("the adapter never answered")
			a, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
			if err != nil || status != 202 || a == nil {
				t.Fatalf("seeding a stuck adoption: allocation=%#v status=%d err=%v", a, status, err)
			}
			inventory.abandon = func(domain.Allocation, string, domain.PriorInventory) error {
				return errors.New("the clear was interrupted")
			}
			if _, err := s.AbandonAdoption(context.Background(), a.ID, "", adoptingOperator, "seeding the window"); err == nil {
				t.Fatal("seeding the abandon window: the clear should have failed")
			}
			return world{s, safetyPrincipal(), safetyRequest("orders"), adoptPin()}
		},
	}
	outcome := func(cidr, pool string, err error) string {
		var api *domain.APIError
		if errors.As(err, &api) {
			return fmt.Sprintf("refused %d %s", api.Status, api.Code)
		}
		if err != nil {
			return "error " + err.Error()
		}
		return "ok " + cidr + " " + pool
	}
	for name, build := range scenarios {
		t.Run(name, func(t *testing.T) {
			planned, applied := build(t), build(t)
			v, planErr := planned.service.PlanAdoption(context.Background(), planned.principal, planned.request, planned.pin)
			a, _, _, adoptErr := applied.service.Adopt(context.Background(), applied.principal, applied.request, applied.pin)
			var plan, apply string
			if v != nil {
				plan = outcome(v.Allocation.CIDR, v.Allocation.PoolID, planErr)
			} else {
				plan = outcome("", "", planErr)
			}
			if a != nil {
				apply = outcome(a.CIDR, a.PoolID, adoptErr)
			} else {
				apply = outcome("", "", adoptErr)
			}
			if plan != apply {
				t.Fatalf("plan and apply disagree: plan=%q apply=%q", plan, apply)
			}
		})
	}
}
