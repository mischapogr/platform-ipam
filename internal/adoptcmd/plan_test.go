package adoptcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
)

func testConfig() domain.Config {
	return domain.Config{
		Identities: []domain.Principal{
			{Subject: "team-a-prod", TenantID: "team-a", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}},
		},
	}
}

func vpcRecord(row int, key string) Record {
	return Record{
		SourceRow: row, TenantID: "team-a", AllocationKey: key, Scope: "vpc",
		Environment: "prod", Region: "eu-central-1", AccountID: "123456789012",
		CIDR: "10.1.0.0/24", ResourceID: "vpc-0abc", NetBoxPrefixID: "42",
	}
}
func subnetRecord(row int, key, parentKey string) Record {
	return Record{
		SourceRow: row, TenantID: "team-a", AllocationKey: key, Scope: "subnet",
		Environment: "prod", Region: "eu-central-1", AccountID: "123456789012",
		CIDR: "10.1.0.0/26", ResourceID: "subnet-0abc", NetBoxPrefixID: "43",
		ParentAllocationKey: parentKey, AvailabilityZoneID: "euc1-az1",
	}
}

// --- requestFor ---

func TestRequestForRejectsNonCanonicalCIDR(t *testing.T) {
	r := vpcRecord(2, "vpc-orders")
	r.CIDR = "10.1.0.1/24"
	_, err := requestFor(r)
	if err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("want a non-canonical CIDR error, got %v", err)
	}
}

func TestRequestForRejectsVPCWithParentOrAZ(t *testing.T) {
	r := vpcRecord(2, "vpc-orders")
	r.ParentAllocationKey = "something"
	if _, err := requestFor(r); err == nil {
		t.Fatal("want an error for a vpc record supplying parent_allocation_key")
	}
	r = vpcRecord(2, "vpc-orders")
	r.AvailabilityZoneID = "euc1-az1"
	if _, err := requestFor(r); err == nil {
		t.Fatal("want an error for a vpc record supplying availability_zone_id")
	}
}

func TestRequestForRejectsSubnetMissingParentOrAZ(t *testing.T) {
	r := subnetRecord(2, "subnet-a", "vpc-orders")
	r.ParentAllocationKey = ""
	if _, err := requestFor(r); err == nil {
		t.Fatal("want an error for a subnet record with no parent_allocation_key")
	}
	r = subnetRecord(2, "subnet-a", "vpc-orders")
	r.AvailabilityZoneID = ""
	if _, err := requestFor(r); err == nil {
		t.Fatal("want an error for a subnet record with no availability_zone_id")
	}
}

func TestRequestForDerivesPrefixLengthFromCIDR(t *testing.T) {
	req, err := requestFor(vpcRecord(2, "vpc-orders"))
	if err != nil {
		t.Fatalf("requestFor: %v", err)
	}
	if req.PrefixLength != 24 {
		t.Fatalf("want prefix length 24 derived from the CIDR, got %d", req.PrefixLength)
	}
}

// --- evaluateRecord ---

func freshVerdict(cidr, pool, dom string, bits int) *service.AdoptionVerdict {
	return &service.AdoptionVerdict{Allocation: domain.Allocation{
		Request: domain.Request{PrefixLength: bits}, CIDR: cidr, PoolID: pool, DomainID: dom,
	}}
}

func TestEvaluateRecordWouldAdopt(t *testing.T) {
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-orders", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	pl := newParentLookup()
	res := evaluateRecord(context.Background(), svc, testConfig(), vpcRecord(2, "vpc-orders"), pl, "alice")
	if res.Verdict != VerdictWouldAdopt || res.CIDR != "10.1.0.0/24" || res.PrefixLength != 24 {
		t.Fatalf("verdict: %#v", res)
	}
	if len(svc.planCalls) != 1 || svc.planCalls[0].pin.Operator != "alice" {
		t.Fatalf("PlanAdoption was not called with the operator: %#v", svc.planCalls)
	}
}

func TestEvaluateRecordWouldReplayAndPending(t *testing.T) {
	svc := newFakeService()
	committed := freshVerdict("10.1.0.0/24", "pool1", "d1", 24)
	committed.Existing = true
	committed.Allocation.Committed = true
	svc.setPlan("team-a", "vpc-orders", committed, nil)
	pl := newParentLookup()
	res := evaluateRecord(context.Background(), svc, testConfig(), vpcRecord(2, "vpc-orders"), pl, "alice")
	if res.Verdict != VerdictWouldReplay {
		t.Fatalf("want would_replay, got %#v", res)
	}

	pending := freshVerdict("10.1.0.0/24", "pool1", "d1", 24)
	pending.Existing, pending.Pending = true, true
	svc.setPlan("team-a", "vpc-pending", pending, nil)
	res = evaluateRecord(context.Background(), svc, testConfig(), vpcRecord(2, "vpc-pending"), pl, "alice")
	if res.Verdict != VerdictPending {
		t.Fatalf("want pending, got %#v", res)
	}
}

func TestEvaluateRecordRefusedWrapsAPIError(t *testing.T) {
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-orders", nil, domain.Err(409, "adoption_refused", "another network occupies the adopted CIDR"))
	pl := newParentLookup()
	res := evaluateRecord(context.Background(), svc, testConfig(), vpcRecord(2, "vpc-orders"), pl, "alice")
	if res.Verdict != VerdictRefused || res.RefusalCode != "adoption_refused" {
		t.Fatalf("verdict: %#v", res)
	}
}

func TestEvaluateRecordRefusesNonCanonicalCIDRWithoutCallingTheService(t *testing.T) {
	svc := newFakeService()
	pl := newParentLookup()
	r := vpcRecord(2, "vpc-orders")
	r.CIDR = "10.1.0.1/24"
	res := evaluateRecord(context.Background(), svc, testConfig(), r, pl, "alice")
	if res.Verdict != VerdictRefused || res.RefusalCode != "invalid_request" {
		t.Fatalf("verdict: %#v", res)
	}
	if len(svc.planCalls) != 0 {
		t.Fatalf("PlanAdoption must not be called for a structurally invalid record: %#v", svc.planCalls)
	}
}

func TestEvaluateRecordRefusesTenantWithoutIdentity(t *testing.T) {
	svc := newFakeService()
	pl := newParentLookup()
	res := evaluateRecord(context.Background(), svc, domain.Config{}, vpcRecord(2, "vpc-orders"), pl, "alice")
	if res.Verdict != VerdictRefused || !strings.Contains(res.Message, "no identity") {
		t.Fatalf("verdict: %#v", res)
	}
}

func TestEvaluateRecordSubnetParentInLedgerResolvesToRealID(t *testing.T) {
	svc := newFakeService()
	svc.setPlan("team-a", "subnet-a", freshVerdict("10.1.0.0/26", "pool1", "d1", 26), nil)
	pl := newParentLookup()
	pl.resolved[parentLookupKey("team-a", "vpc-orders")] = parentRef{id: "alloc_parent123", bound: true}
	res := evaluateRecord(context.Background(), svc, testConfig(), subnetRecord(2, "subnet-a", "vpc-orders"), pl, "alice")
	if res.Verdict != VerdictWouldAdopt {
		t.Fatalf("verdict: %#v", res)
	}
	if len(svc.planCalls) != 1 || svc.planCalls[0].request.ParentAllocationID != "alloc_parent123" {
		t.Fatalf("PlanAdoption was not given the resolved parent id: %#v", svc.planCalls)
	}
}

// A committed parent without a binding is what an adopted VPC looks like until
// its owning team has tagged it. The service would refuse the subnet as
// overlapping "another cloud resource"; plan says what is being waited for
// instead, and does not ask.
func TestEvaluateRecordSubnetWaitsForAParentWithoutABinding(t *testing.T) {
	svc := newFakeService()
	pl := newParentLookup()
	pl.record("team-a", "vpc-orders", domain.Allocation{ID: "alloc_parent123"})
	res := evaluateRecord(context.Background(), svc, testConfig(), subnetRecord(2, "subnet-a", "vpc-orders"), pl, "alice")
	if res.Verdict != VerdictWaitingForParent || res.RefusalCode != "" {
		t.Fatalf("want waiting_for_parent without a refusal code, got %#v", res)
	}
	if len(svc.planCalls) != 0 {
		t.Fatalf("PlanAdoption must not be called for a waiting subnet: %#v", svc.planCalls)
	}
	if (Report{Records: []RecordResult{res}}).HasRefusals() {
		t.Fatal("waiting for a parent must not count as a refusal")
	}
}

func TestParentLookupTreatsOnlyABoundParentAsReady(t *testing.T) {
	pl := newParentLookup()
	pl.record("team-a", "unbound", domain.Allocation{ID: "a1"})
	pl.record("team-a", "bound", domain.Allocation{ID: "a2", Binding: &domain.Binding{ResourceID: "vpc-1"}})
	if ref, _, found := pl.resolve("team-a", "unbound"); !found || ref.bound || ref.id != "a1" {
		t.Fatalf("unbound parent: %#v found=%v", ref, found)
	}
	if ref, _, found := pl.resolve("team-a", "bound"); !found || !ref.bound || ref.id != "a2" {
		t.Fatalf("bound parent: %#v found=%v", ref, found)
	}
}

func TestEvaluateRecordSubnetParentOnlyInFileIsDeferredNotRefused(t *testing.T) {
	svc := newFakeService()
	pl := newParentLookup()
	pl.noteFileRows([]Record{vpcRecord(2, "vpc-orders"), subnetRecord(3, "subnet-a", "vpc-orders")})
	res := evaluateRecord(context.Background(), svc, testConfig(), subnetRecord(3, "subnet-a", "vpc-orders"), pl, "alice")
	if res.Verdict != VerdictDeferred {
		t.Fatalf("want deferred_on_parent, got %#v", res)
	}
	if len(svc.planCalls) != 0 {
		t.Fatalf("PlanAdoption must not be called for a deferred subnet: %#v", svc.planCalls)
	}
}

func TestEvaluateRecordSubnetParentNeitherInFileNorLedgerIsRefused(t *testing.T) {
	svc := newFakeService()
	pl := newParentLookup()
	res := evaluateRecord(context.Background(), svc, testConfig(), subnetRecord(2, "subnet-a", "vpc-nowhere"), pl, "alice")
	if res.Verdict != VerdictRefused || res.RefusalCode != "invalid_parent" {
		t.Fatalf("verdict: %#v", res)
	}
}

func TestReportHasRefusals(t *testing.T) {
	rep := Report{Records: []RecordResult{{Verdict: VerdictWouldAdopt}, {Verdict: VerdictDeferred}, {Verdict: VerdictPending}}}
	if rep.HasRefusals() {
		t.Fatal("would_adopt/deferred/pending must not count as refusals")
	}
	rep.Records = append(rep.Records, RecordResult{Verdict: VerdictRefused})
	if !rep.HasRefusals() {
		t.Fatal("a refused record must be detected")
	}
}
