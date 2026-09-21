package adoptcmd

import (
	"context"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
)

func TestPerformApplyRefusesEverythingWhenOneRecordIsRefusedAndAdoptsNothing(t *testing.T) {
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-good", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	// vpc-bad has no configured plan response, so PlanAdoption on it errors --
	// simulating a genuine refusal from the service.
	svc.setPlan("team-a", "vpc-bad", nil, domain.Err(409, "adoption_refused", "occupied"))

	records := []Record{vpcRecord(2, "vpc-good"), vpcRecord(3, "vpc-bad")}
	report, err := performApply(context.Background(), svc, testConfig(), records, "alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	if !report.Plan.HasRefusals() {
		t.Fatalf("want the pre-flight plan to report a refusal: %#v", report.Plan)
	}
	if len(report.Outcomes) != 0 {
		t.Fatalf("a refused pre-flight plan must write nothing: %#v", report.Outcomes)
	}
	if len(svc.adoptCalls) != 0 {
		t.Fatalf("Adopt must not be called when the pre-flight plan is refused: %#v", svc.adoptCalls)
	}
}

func TestPerformApplyOrdersParentsBeforeSubnetsGivenSubnetFirstInput(t *testing.T) {
	svc := newFakeService()
	svc.setPlan("team-a", "subnet-a", freshVerdict("10.1.0.0/26", "pool1", "d1", 26), nil)
	svc.setPlan("team-a", "vpc-orders", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	svc.setAdopt("team-a", "vpc-orders", &domain.Allocation{ID: "alloc_vpc", CIDR: "10.1.0.0/24"}, 201, nil)
	svc.setAdopt("team-a", "subnet-a", &domain.Allocation{ID: "alloc_subnet", CIDR: "10.1.0.0/26"}, 201, nil)

	// Subnet listed first in the input file.
	records := []Record{subnetRecord(2, "subnet-a", "vpc-orders"), vpcRecord(3, "vpc-orders")}
	report, err := performApply(context.Background(), svc, testConfig(), records, "alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	// The parent goes first whatever the input order. Its subnet is NOT
	// attempted in the same run: the VPC was adopted a moment ago, is RESERVED
	// and unbound, and the service exempts a parent VPC from its subnet's
	// overlap rule only on the evidence of a binding.
	if len(svc.adoptCalls) != 1 || svc.adoptCalls[0].request.Scope != "vpc" {
		t.Fatalf("want exactly the parent adopted, got %#v", svc.adoptCalls)
	}
	if report.Outcomes[0].Scope != "vpc" || report.Outcomes[0].Status != OutcomeAdopted {
		t.Fatalf("parent outcome: %#v", report.Outcomes[0])
	}
	if report.Outcomes[1].Scope != "subnet" || report.Outcomes[1].Status != OutcomeWaitingForParent || report.Outcomes[1].Message == "" {
		t.Fatalf("subnet outcome: %#v", report.Outcomes[1])
	}
}

// The second stage of the same adoption: the owning team has tagged the VPC,
// the worker has bound it, and a re-run replays the parent and adopts the
// subnet under the parent's allocation id.
func TestPerformApplyAdoptsASubnetOnceItsParentIsBound(t *testing.T) {
	svc := newFakeService()
	bound := domain.Allocation{ID: "alloc_vpc", CIDR: "10.1.0.0/24", Request: domain.Request{AllocationKey: "vpc-orders"}, Binding: &domain.Binding{ResourceID: "vpc-1"}}
	svc.list["team-a"] = []domain.Allocation{bound}
	svc.setPlan("team-a", "vpc-orders", &service.AdoptionVerdict{Allocation: bound, Existing: true}, nil)
	svc.setPlan("team-a", "subnet-a", freshVerdict("10.1.0.0/26", "pool1", "d1", 26), nil)
	svc.setAdopt("team-a", "vpc-orders", &bound, 200, nil)
	svc.setAdopt("team-a", "subnet-a", &domain.Allocation{ID: "alloc_subnet", CIDR: "10.1.0.0/26"}, 201, nil)

	records := []Record{subnetRecord(2, "subnet-a", "vpc-orders"), vpcRecord(3, "vpc-orders")}
	report, err := performApply(context.Background(), svc, testConfig(), records, "alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	if len(svc.adoptCalls) != 2 || svc.adoptCalls[1].request.Scope != "subnet" || svc.adoptCalls[1].request.ParentAllocationID != "alloc_vpc" {
		t.Fatalf("the subnet was not adopted under its bound parent: %#v", svc.adoptCalls)
	}
	for _, o := range report.Outcomes {
		if o.Status != OutcomeAdopted {
			t.Fatalf("outcomes: %#v", report.Outcomes)
		}
	}
}

// A waiting subnet does not stop the run: other networks in the file are
// still adopted, and the run still does not exit as complete.
func TestPerformApplyAWaitingSubnetDoesNotStopTheRun(t *testing.T) {
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-orders", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	svc.setPlan("team-a", "vpc-billing", freshVerdict("10.2.0.0/24", "pool1", "d1", 24), nil)
	svc.setAdopt("team-a", "vpc-orders", &domain.Allocation{ID: "alloc_orders", CIDR: "10.1.0.0/24", Request: domain.Request{AllocationKey: "vpc-orders"}}, 201, nil)
	svc.setAdopt("team-a", "vpc-billing", &domain.Allocation{ID: "alloc_billing", CIDR: "10.2.0.0/24", Request: domain.Request{AllocationKey: "vpc-billing"}}, 201, nil)
	records := []Record{vpcRecord(2, "vpc-orders"), subnetRecord(3, "subnet-a", "vpc-orders"), vpcRecord(4, "vpc-billing")}
	report, err := performApply(context.Background(), svc, testConfig(), records, "alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	statuses := map[string]OutcomeStatus{}
	for _, o := range report.Outcomes {
		statuses[o.AllocationKey] = o.Status
	}
	if statuses["vpc-orders"] != OutcomeAdopted || statuses["vpc-billing"] != OutcomeAdopted || statuses["subnet-a"] != OutcomeWaitingForParent {
		t.Fatalf("outcomes: %#v", report.Outcomes)
	}
}

func TestPerformApplySubnetWithNoParentAnywhereIsRefusedBeforeAnyWrite(t *testing.T) {
	svc := newFakeService()
	records := []Record{subnetRecord(2, "subnet-a", "vpc-nowhere")}
	report, err := performApply(context.Background(), svc, testConfig(), records, "alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	if !report.Plan.HasRefusals() || report.Plan.Records[0].RefusalCode != "invalid_parent" {
		t.Fatalf("plan: %#v", report.Plan)
	}
	if len(svc.adoptCalls) != 0 {
		t.Fatalf("Adopt must not be called: %#v", svc.adoptCalls)
	}
}

func TestPerformApplyStopsAtFirstFailureAndReportsNotAttempted(t *testing.T) {
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-1", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	svc.setPlan("team-a", "vpc-2", freshVerdict("10.2.0.0/24", "pool1", "d1", 24), nil)
	svc.setPlan("team-a", "vpc-3", freshVerdict("10.3.0.0/24", "pool1", "d1", 24), nil)
	svc.setAdopt("team-a", "vpc-1", &domain.Allocation{ID: "alloc_1", CIDR: "10.1.0.0/24"}, 201, nil)
	svc.setAdopt("team-a", "vpc-2", nil, 0, domain.Err(409, "adoption_conflict", "the adopted network is not the inventory object that was reviewed"))
	// vpc-3 has no Adopt response configured; it must never be called.

	records := []Record{vpcRecord(2, "vpc-1"), vpcRecord(3, "vpc-2"), vpcRecord(4, "vpc-3")}
	report, err := performApply(context.Background(), svc, testConfig(), records, "alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	if len(report.Outcomes) != 3 {
		t.Fatalf("want 3 outcomes, got %d: %#v", len(report.Outcomes), report.Outcomes)
	}
	if report.Outcomes[0].Status != OutcomeAdopted || report.Outcomes[0].AllocationID != "alloc_1" {
		t.Fatalf("outcome 0: %#v", report.Outcomes[0])
	}
	if report.Outcomes[1].Status != OutcomeFailed || report.Outcomes[1].RefusalCode != "adoption_conflict" {
		t.Fatalf("outcome 1: %#v", report.Outcomes[1])
	}
	if report.Outcomes[2].Status != OutcomeNotAttempted {
		t.Fatalf("outcome 2: %#v", report.Outcomes[2])
	}
	if len(svc.adoptCalls) != 2 {
		t.Fatalf("want exactly 2 Adopt calls (vpc-3 never attempted), got %d: %#v", len(svc.adoptCalls), svc.adoptCalls)
	}
}

func TestPerformApplyA202StopsTheRunWithAClearMessage(t *testing.T) {
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-1", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	svc.setPlan("team-a", "vpc-2", freshVerdict("10.2.0.0/24", "pool1", "d1", 24), nil)
	svc.setAdopt("team-a", "vpc-1", &domain.Allocation{ID: "alloc_1"}, 202, nil)

	records := []Record{vpcRecord(2, "vpc-1"), vpcRecord(3, "vpc-2")}
	report, err := performApply(context.Background(), svc, testConfig(), records, "alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	if report.Outcomes[0].Status != OutcomeFailed || report.Outcomes[0].HTTPStatus != 202 {
		t.Fatalf("outcome 0: %#v", report.Outcomes[0])
	}
	if report.Outcomes[0].Message == "" {
		t.Fatal("a 202 outcome must explain what happens next and that re-running apply is safe")
	}
	// The allocation is invisible to GET until it commits, so the report is
	// the only place an operator learns the id an adoption_stuck finding names.
	if report.Outcomes[0].AllocationID != "alloc_1" {
		t.Fatalf("a pending outcome must carry the allocation id: %#v", report.Outcomes[0])
	}
	if report.Outcomes[1].Status != OutcomeNotAttempted {
		t.Fatalf("outcome 1: %#v", report.Outcomes[1])
	}
}

func TestPerformApplySecondRunReplaysWithNoNewAdoptCalls(t *testing.T) {
	svc := newFakeService()
	committed := &domain.Allocation{ID: "alloc_1", CIDR: "10.1.0.0/24", Committed: true}
	verdict := freshVerdict("10.1.0.0/24", "pool1", "d1", 24)
	verdict.Existing, verdict.Allocation.Committed, verdict.Allocation.ID = true, true, "alloc_1"
	svc.setPlan("team-a", "vpc-1", verdict, nil)
	svc.setAdopt("team-a", "vpc-1", committed, 200, nil)

	records := []Record{vpcRecord(2, "vpc-1")}
	report, err := performApply(context.Background(), svc, testConfig(), records, "alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	if report.Outcomes[0].Status != OutcomeAdopted || report.Outcomes[0].HTTPStatus != 200 {
		t.Fatalf("outcome: %#v", report.Outcomes[0])
	}
}

func TestPerformApplyPropagatesTheOperatorAndTenantPrincipalNotTheOperator(t *testing.T) {
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-1", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	svc.setAdopt("team-a", "vpc-1", &domain.Allocation{ID: "alloc_1"}, 201, nil)

	_, err := performApply(context.Background(), svc, testConfig(), []Record{vpcRecord(2, "vpc-1")}, "ops:alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	if len(svc.adoptCalls) != 1 {
		t.Fatalf("want 1 Adopt call, got %d", len(svc.adoptCalls))
	}
	call := svc.adoptCalls[0]
	if call.pin.Operator != "ops:alice" {
		t.Fatalf("Adoption.Operator did not carry the --operator value verbatim: %#v", call.pin)
	}
	if call.principal.Subject != "team-a-prod" || call.principal.TenantID != "team-a" {
		t.Fatalf("the principal must be the tenant's configured identity, not the operator: %#v", call.principal)
	}
}

func TestPerformApplySeedFromLedgerFailureIsAnAdapterError(t *testing.T) {
	svc := newFakeService()
	svc.listErr = context.DeadlineExceeded
	_, err := performApply(context.Background(), svc, testConfig(), []Record{vpcRecord(2, "vpc-1")}, "alice")
	if err == nil {
		t.Fatal("want an error when the ledger cannot be listed to seed the parent lookup")
	}
}

// Waiting is per subnet, not per run: a subnet behind an unbound parent is
// skipped, and a later subnet whose parent is bound is still adopted.
func TestPerformApplyAdoptsALaterSubnetAfterAWaitingOne(t *testing.T) {
	svc := newFakeService()
	svc.list["team-a"] = []domain.Allocation{
		{ID: "alloc_unbound", Request: domain.Request{AllocationKey: "vpc-unbound"}},
		{ID: "alloc_bound", Request: domain.Request{AllocationKey: "vpc-bound"}, Binding: &domain.Binding{ResourceID: "vpc-1"}},
	}
	svc.setPlan("team-a", "subnet-late", freshVerdict("10.2.0.0/26", "pool1", "d1", 26), nil)
	svc.setAdopt("team-a", "subnet-late", &domain.Allocation{ID: "alloc_subnet", CIDR: "10.2.0.0/26"}, 201, nil)
	records := []Record{subnetRecord(2, "subnet-early", "vpc-unbound"), subnetRecord(3, "subnet-late", "vpc-bound")}
	report, err := performApply(context.Background(), svc, testConfig(), records, "alice")
	if err != nil {
		t.Fatalf("performApply: %v", err)
	}
	if len(report.Outcomes) != 2 || report.Outcomes[0].Status != OutcomeWaitingForParent || report.Outcomes[1].Status != OutcomeAdopted {
		t.Fatalf("outcomes: %#v", report.Outcomes)
	}
	if len(svc.adoptCalls) != 1 || svc.adoptCalls[0].request.ParentAllocationID != "alloc_bound" {
		t.Fatalf("adopt calls: %#v", svc.adoptCalls)
	}
}
