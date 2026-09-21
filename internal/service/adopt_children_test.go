package service

// Adoption's third overlap exemption (ADR 0010, amended 2026-09-20): a pinned
// VPC may contain its own observed child subnets. Before it, the two
// exemptions the record shipped with refused every VPC that had any -- an
// organization inventory imports a VPC together with its subnets and the
// observer sees all of them -- so the two-run procedure the runbook describes
// had never worked.
//
// These tests hold the exemption to the shape that justifies it: everything
// inside the pin must be accounted for by an observed child of the reviewed
// VPC, and every other way of being inside it is still a refusal. They sit
// beside adopt_test.go and use its fixtures; nothing there changes, in
// particular TestAdoptRefusesASecondUnmanagedPrefix, whose nested /25 has no
// observed subnet behind it and must keep refusing.

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

const (
	childACIDR       = "10.7.0.0/26"
	childBCIDR       = "10.7.0.64/26"
	childANetworkID  = "4301"
	childBNetworkID  = "4302"
	childAResourceID = "subnet-child-a"
	childBResourceID = "subnet-child-b"
	childAZone       = "euc1-az1"
	childBZone       = "euc1-az2"
)

// childNetwork is one of the reviewed VPC's subnets as the import left it: an
// unmanaged prefix carrying the import tag and nothing else.
func childNetwork(id, cidr string) domain.Network {
	return domain.Network{ID: id, CIDR: cidr, Imported: true, ImportBatch: adoptedBatch}
}

// childResource is one of the reviewed VPC's subnets as the observer sees it:
// a subnet whose parent is the reviewed VPC, in the request's account and
// region, claimed by nobody.
func childResource(id, cidr, zone string) domain.Resource {
	return domain.Resource{
		Type: "subnet", ID: id, AccountID: "123456789012", Region: "eu",
		CIDR: cidr, CIDRs: []string{cidr}, ParentID: adoptedResourceID, ZoneID: zone,
	}
}

// vpcWithChildren is the ordinary case the exemption exists for: the VPC and
// two of its subnets, all imported and all observed.
func vpcWithChildren() ([]domain.Network, []domain.Resource) {
	return []domain.Network{
			adoptedNetwork(),
			childNetwork(childANetworkID, childACIDR),
			childNetwork(childBNetworkID, childBCIDR),
		}, []domain.Resource{
			adoptedResource(),
			childResource(childAResourceID, childACIDR, childAZone),
			childResource(childBResourceID, childBCIDR, childBZone),
		}
}

func TestAdoptAVPCThatContainsItsOwnObservedChildren(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	networks, resources := vpcWithChildren()
	s, ledger, inventory := adoptFixture(now, networks, resources)

	a, op, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 || a == nil || op == nil {
		t.Fatalf("adopting a VPC with two children: allocation=%#v operation=%#v status=%d err=%v", a, op, status, err)
	}
	if !a.Committed || a.CIDR != adoptedCIDR || a.InventoryID != adoptedNetworkID {
		t.Fatalf("adopted allocation: %#v", *a)
	}
	// Exactly one object was converted: the VPC's own prefix. The children are
	// exempt from the overlap rules, not adopted by this run.
	if len(inventory.adopts) != 1 || inventory.adopts[0].CIDR != adoptedCIDR {
		t.Fatalf("the children were adopted too: %#v", inventory.adopts)
	}
	st := ledgerState(t, ledger)
	if len(st.Allocations) != 1 {
		t.Fatalf("want one allocation for the VPC alone, got %d", len(st.Allocations))
	}
}

// The exemption asks the cloud, not the operator. An observed child the
// inventory does not know about is a partial import, not an ownership
// ambiguity: the VPC's whole range is what the adoption claims, the child is
// inside it and claimed by nobody, and it stays visible afterwards because the
// reconciler keeps reporting it (see
// TestAnAdoptedVPCsChildrenAreStillReportedAsUnmanagedOccupancy). Refusing
// here would make adoption depend on the completeness of a NetBox import that
// the platform cannot verify in the first place.
func TestAdoptAcceptsAnObservedChildWithNoImportedPrefix(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	s, _, inventory := adoptFixture(now,
		[]domain.Network{adoptedNetwork()},
		[]domain.Resource{adoptedResource(), childResource(childAResourceID, childACIDR, childAZone)})

	a, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 || a == nil || !a.Committed {
		t.Fatalf("adopting a VPC whose child was never imported: allocation=%#v status=%d err=%v", a, status, err)
	}
	if len(inventory.adopts) != 1 {
		t.Fatalf("inventory calls: %#v", inventory.adopts)
	}
}

// Everything inside the pin must be accounted for by an observed child of the
// reviewed VPC. Each case below is a way of being inside it that is not that,
// and each must still refuse and still write nothing.
func TestAdoptRefusesWhatTheChildExemptionDoesNotCover(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	stranger := childResource(childAResourceID, childACIDR, childAZone)
	stranger.ParentID = "vpc-somebody-elses"

	claimed := childResource(childAResourceID, childACIDR, childAZone)
	claimed.Tags = map[string]string{"platform-ipam:allocation-id": "alloc-someone-else"}

	otherAccount := childResource(childAResourceID, childACIDR, childAZone)
	otherAccount.AccountID = "210987654321"

	otherRegion := childResource(childAResourceID, childACIDR, childAZone)
	otherRegion.Region = "us"

	// The primary CIDR is elsewhere and only a secondary association reaches
	// into the pin. A secondary is occupancy evidence and never what a resource
	// issues, so this is not a child however well its parent matches.
	secondary := childResource(childAResourceID, "10.9.0.0/26", childAZone)
	secondary.CIDRs = []string{"10.9.0.0/26", childACIDR}

	// A VPC nested inside the pinned VPC is a second VPC, whatever it claims
	// its parent is: only a subnet can be a child.
	nestedVPC := domain.Resource{
		Type: "vpc", ID: "vpc-nested", AccountID: "123456789012", Region: "eu",
		CIDR: childACIDR, CIDRs: []string{childACIDR}, ParentID: adoptedResourceID,
	}

	ownedChild := childNetwork(childANetworkID, childACIDR)
	ownedChild.Owned = true
	claimedChild := childNetwork(childANetworkID, childACIDR)
	claimedChild.AllocationID = "alloc-someone-else"
	unimportedChild := childNetwork(childANetworkID, childACIDR)
	unimportedChild.Imported = false

	cases := map[string]struct {
		networks  []domain.Network
		resources []domain.Resource
	}{
		"a subnet of another VPC": {
			[]domain.Network{adoptedNetwork(), childNetwork(childANetworkID, childACIDR)},
			[]domain.Resource{adoptedResource(), stranger},
		},
		"a subnet claimed by an allocation": {
			[]domain.Network{adoptedNetwork(), childNetwork(childANetworkID, childACIDR)},
			[]domain.Resource{adoptedResource(), claimed},
		},
		"a subnet in another account": {
			[]domain.Network{adoptedNetwork(), childNetwork(childANetworkID, childACIDR)},
			[]domain.Resource{adoptedResource(), otherAccount},
		},
		"a subnet in another region": {
			[]domain.Network{adoptedNetwork(), childNetwork(childANetworkID, childACIDR)},
			[]domain.Resource{adoptedResource(), otherRegion},
		},
		"a subnet whose secondary CIDR matches": {
			[]domain.Network{adoptedNetwork()},
			[]domain.Resource{adoptedResource(), secondary},
		},
		"a second VPC nested inside the pin": {
			[]domain.Network{adoptedNetwork()},
			[]domain.Resource{adoptedResource(), nestedVPC},
		},
		"an imported prefix no observed child holds": {
			[]domain.Network{adoptedNetwork(), childNetwork(childANetworkID, childACIDR)},
			[]domain.Resource{adoptedResource()},
		},
		"an imported prefix at a CIDR no child holds": {
			[]domain.Network{adoptedNetwork(), childNetwork("4399", "10.7.0.128/26")},
			[]domain.Resource{adoptedResource(), childResource(childAResourceID, childACIDR, childAZone)},
		},
		"an owned prefix inside the pin": {
			[]domain.Network{adoptedNetwork(), ownedChild},
			[]domain.Resource{adoptedResource(), childResource(childAResourceID, childACIDR, childAZone)},
		},
		"a prefix inside the pin carrying an allocation id": {
			[]domain.Network{adoptedNetwork(), claimedChild},
			[]domain.Resource{adoptedResource(), childResource(childAResourceID, childACIDR, childAZone)},
		},
		"a prefix inside the pin without the import tag": {
			[]domain.Network{adoptedNetwork(), unimportedChild},
			[]domain.Resource{adoptedResource(), childResource(childAResourceID, childACIDR, childAZone)},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, ledger, inventory := adoptFixture(now, tc.networks, tc.resources)
			_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
			safetyAPIError(t, err, 409, "adoption_refused")
			assertNothingWritten(t, ledger, inventory)
		})
	}
}

// The exemption is for a pinned VPC and for nothing else. A subnet has no
// children this platform models, so a pinned subnet keeps exactly the two
// exemptions it had: the space inside it belongs to whoever put it there.
func TestTheChildExemptionDoesNotApplyToAPinnedSubnet(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	const nestedCIDR = "10.7.0.16/28"
	networks := []domain.Network{
		{ID: "4200", CIDR: adoptedCIDR, AllocationID: "alloc-parent"},
		childNetwork(childANetworkID, childACIDR),
		childNetwork("4400", nestedCIDR),
	}
	parentResource := domain.Resource{
		Type: "vpc", ID: "vpc-parent", AccountID: "123456789012", Region: "eu",
		CIDR: adoptedCIDR, CIDRs: []string{adoptedCIDR},
		Tags: map[string]string{"platform-ipam:allocation-id": "alloc-parent", "platform-ipam:allocation-key": "alloc-parent-key"},
	}
	subnet := domain.Resource{
		Type: "subnet", ID: childAResourceID, AccountID: "123456789012", Region: "eu",
		CIDR: childACIDR, CIDRs: []string{childACIDR}, ParentID: "vpc-parent", ZoneID: childAZone,
	}
	// A subnet observed inside the pinned subnet, naming it as its parent. If
	// the exemption were scope-blind this would be waved through.
	nested := domain.Resource{
		Type: "subnet", ID: "subnet-nested", AccountID: "123456789012", Region: "eu",
		CIDR: nestedCIDR, CIDRs: []string{nestedCIDR}, ParentID: childAResourceID, ZoneID: childAZone,
	}

	s, ledger, inventory := adoptFixture(now, networks, []domain.Resource{parentResource, subnet, nested})
	parent := safetyAllocation(now, "alloc-parent", "vpc", adoptedCIDR, "", "")
	parent.State = domain.Active
	parent.Binding = &domain.Binding{Provider: "aws", ResourceType: "vpc", ResourceID: "vpc-parent", AccountID: "123456789012", Region: "eu"}
	putSafetyAllocation(t, ledger, parent)
	inventory.adoptID = childANetworkID

	request := domain.Request{
		AllocationKey: "orders-subnet", Scope: "subnet", Environment: "prod", Region: "eu",
		AccountID: "123456789012", PrefixLength: 26, ParentAllocationID: "alloc-parent", AvailabilityZoneID: childAZone,
	}
	pin := Adoption{Operator: adoptingOperator, CIDR: childACIDR, NetworkID: childANetworkID, ResourceID: childAResourceID}

	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), request, pin)
	safetyAPIError(t, err, 409, "adoption_refused")
	if len(inventory.adopts) != 0 {
		t.Fatalf("a refused subnet adoption called Inventory.Adopt: %#v", inventory.adopts)
	}
}

// The exemption changes what an operator may adopt and nothing about what the
// allocator may choose. chooseCIDR has no exemptions at all: a child subnet's
// prefix and its resource are occupancy, and an ordinary reservation still
// steps over them.
func TestAnOrdinaryReservationStillRefusesTheSpaceAChildOccupies(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("a pool with nothing but the child's space left is exhausted", func(t *testing.T) {
		networks, resources := vpcWithChildren()
		s, _, _ := adoptFixture(now, networks, resources)
		s.cfg.Pools[0].CIDR = adoptedCIDR
		_, _, _, err := s.Reserve(ctx, safetyPrincipal(), safetyRequest("orders"), "consumer-key-1")
		safetyAPIError(t, err, 409, "pool_exhausted")
	})

	t.Run("a chosen candidate steps over a child's occupancy", func(t *testing.T) {
		s, _, _ := adoptFixture(now,
			[]domain.Network{childNetwork(childANetworkID, childACIDR)},
			[]domain.Resource{childResource(childAResourceID, childACIDR, childAZone)})
		s.cfg.Pools[0].CIDR = adoptedCIDR
		s.cfg.Pools[0].AllowedPrefixLengths = []int{25}
		request := safetyRequest("orders")
		request.PrefixLength = 25
		a, _, status, err := s.Reserve(ctx, safetyPrincipal(), request, "consumer-key-2")
		if err != nil || status != 201 || a == nil {
			t.Fatalf("reservation: allocation=%#v status=%d err=%v", a, status, err)
		}
		if a.CIDR != "10.7.0.128/25" {
			t.Fatalf("the allocator chose %s, which overlaps the child's occupancy at %s", a.CIDR, childACIDR)
		}
	})
}

// adoptedResources names the one resource an adoption reviewed, and the
// children are not it. That is correct: until each child is adopted in its own
// right it is occupancy nobody owns, and the finding is how its tenants learn
// there is still work to do.
func TestAnAdoptedVPCsChildrenAreStillReportedAsUnmanagedOccupancy(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	networks, resources := vpcWithChildren()
	s, ledger, _ := adoptFixture(now, networks, resources)
	ctx := context.Background()

	if _, _, status, err := s.Adopt(ctx, safetyPrincipal(), safetyRequest("orders"), adoptPin()); err != nil || status != 201 {
		t.Fatalf("adopt: status=%d err=%v", status, err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	occupancy := findingsWithCode(t, ledger, "unmanaged_occupancy")
	reported := map[string]bool{}
	for _, f := range occupancy {
		if f.Status == "OPEN" {
			reported[f.ResourceID] = true
		}
	}
	if !reported[childAResourceID] || !reported[childBResourceID] {
		t.Fatalf("want both children reported as unmanaged occupancy, got %#v", occupancy)
	}
	if reported[adoptedResourceID] {
		t.Fatalf("the adopted VPC itself was reported as unmanaged occupancy: %#v", occupancy)
	}
	assertNoOpenCritical(t, s, "after adopting a VPC with children")
}

// The whole point of the amendment: the two-run procedure the runbook and the
// adopt help text describe can now actually be run. The VPC is adopted while
// its children are only occupancy; the owning team tags it; the reconciler
// promotes it; and only then are the children adoptable -- on the existing
// subnet rules, with no fourth exemption.
func TestASubnetIsAdoptableOnceItsParentVPCIsActive(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()
	networks, resources := vpcWithChildren()
	s, ledger, inventory := adoptFixture(now, networks, resources)

	vpc, _, status, err := s.Adopt(ctx, safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 {
		t.Fatalf("adopting the parent VPC: status=%d err=%v", status, err)
	}

	// What internal/netbox/adopt.go leaves behind: the same prefix, now
	// carrying the ownership fields and still carrying the import tag and
	// batch. The double appends rather than converts, so the snapshot is
	// restated here to match the adapter's real effect.
	operationID := ""
	for _, o := range ledgerState(t, ledger).Operations {
		if o.AllocationID == vpc.ID {
			operationID = o.ID
		}
	}
	adoptedVPCNetwork := domain.Network{
		ID: adoptedNetworkID, CIDR: adoptedCIDR, AllocationID: vpc.ID, OperationID: operationID,
		Imported: true, Owned: true, ImportBatch: adoptedBatch,
	}
	inventory.networks = []domain.Network{
		adoptedVPCNetwork,
		childNetwork(childANetworkID, childACIDR),
		childNetwork(childBNetworkID, childBCIDR),
	}

	// A subnet attempted now is still refused: the parent has no binding yet,
	// which is exactly what `adopt` reports as waiting_for_parent.
	subnetRequest := func(key, zone string) domain.Request {
		return domain.Request{
			AllocationKey: key, Scope: "subnet", Environment: "prod", Region: "eu",
			AccountID: "123456789012", PrefixLength: 26,
			ParentAllocationID: vpc.ID, AvailabilityZoneID: zone,
		}
	}
	pinA := Adoption{Operator: adoptingOperator, CIDR: childACIDR, NetworkID: childANetworkID, ResourceID: childAResourceID}
	inventory.adoptID = childANetworkID
	_, _, _, err = s.Adopt(ctx, safetyPrincipal(), subnetRequest("orders-subnet-a", childAZone), pinA)
	safetyAPIError(t, err, 409, "adoption_refused")

	// The owning team tags the VPC; the existing reconciler promotes it.
	tagged := adoptedResource()
	tagged.Tags = map[string]string{
		"platform-ipam:allocation-id":  vpc.ID,
		"platform-ipam:allocation-key": vpc.AllocationKey,
	}
	s.observer.(*safetyObserver).observation = safetyObservation(now, tagged,
		childResource(childAResourceID, childACIDR, childAZone),
		childResource(childBResourceID, childBCIDR, childBZone))
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick after tagging: %v", err)
	}
	if active := safetyState(t, ledger, vpc.ID); active.State != domain.Active || active.Binding == nil {
		t.Fatalf("the tagged VPC did not become ACTIVE: %#v", active)
	}

	subnetA, _, status, err := s.Adopt(ctx, safetyPrincipal(), subnetRequest("orders-subnet-a", childAZone), pinA)
	if err != nil || status != 201 || subnetA == nil || !subnetA.Committed {
		t.Fatalf("adopting the first subnet: allocation=%#v status=%d err=%v", subnetA, status, err)
	}
	if subnetA.CIDR != childACIDR || subnetA.ParentAllocationID != vpc.ID || subnetA.AvailabilityZoneID != childAZone {
		t.Fatalf("adopted subnet: %#v", *subnetA)
	}

	// The sibling is managed now, and a managed sibling does not overlap the
	// next candidate at all, so it changes nothing. Proving that is cheap and
	// the alternative -- a rule that looked at siblings -- would be wrong.
	inventory.networks = []domain.Network{
		adoptedVPCNetwork,
		{ID: childANetworkID, CIDR: childACIDR, AllocationID: subnetA.ID, Imported: true, Owned: true, ImportBatch: adoptedBatch},
		childNetwork(childBNetworkID, childBCIDR),
	}
	inventory.adoptID = childBNetworkID
	pinB := Adoption{Operator: adoptingOperator, CIDR: childBCIDR, NetworkID: childBNetworkID, ResourceID: childBResourceID}
	subnetB, _, status, err := s.Adopt(ctx, safetyPrincipal(), subnetRequest("orders-subnet-b", childBZone), pinB)
	if err != nil || status != 201 || subnetB == nil || !subnetB.Committed {
		t.Fatalf("adopting the second subnet beside an adopted sibling: allocation=%#v status=%d err=%v", subnetB, status, err)
	}
	if subnetB.CIDR != childBCIDR || subnetB.ParentAllocationID != vpc.ID {
		t.Fatalf("adopted sibling subnet: %#v", *subnetB)
	}
}

// The onboarding import has to tell a managed VPC from a managed subnet, and
// the inventory records no scope: domain.Network.ParentAllocationID stands in
// for one, read from the platform_parent_allocation_id custom field the
// adapter writes for every managed prefix. That substitution is exact only
// while this holds -- a vpc request may carry no parent, a subnet request must
// carry one -- so the rule gets a test of its own rather than a comment. If a
// third scope is ever added, this is what fails.
func TestAVPCIsExactlyAnAllocationWithNoParent(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()

	withParent := safetyRequest("orders")
	withParent.ParentAllocationID = "alloc-anything"
	withZone := safetyRequest("orders")
	withZone.AvailabilityZoneID = childAZone
	parentless := safetyRequest("orders")
	parentless.Scope = "subnet"
	parentless.PrefixLength = 26
	for name, request := range map[string]domain.Request{
		"a vpc naming a parent":             withParent,
		"a vpc naming an availability zone": withZone,
		"a subnet naming no parent":         parentless,
	} {
		t.Run(name, func(t *testing.T) {
			s, ledger, inventory := adoptReady(now)
			_, _, _, err := s.Reserve(ctx, safetyPrincipal(), request, "consumer-key-"+name)
			safetyAPIError(t, err, 422, "invalid_request")
			assertNothingWritten(t, ledger, inventory)
		})
	}

	// And the other direction, on a real adoption: what the adapter writes into
	// platform_parent_allocation_id for a vpc-scoped allocation is the empty
	// string, which is what the import reads as "this is a VPC".
	s, _, _ := adoptReady(now)
	a, _, status, err := s.Adopt(ctx, safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 {
		t.Fatalf("adopt: status=%d err=%v", status, err)
	}
	if a.Scope != "vpc" || a.ParentAllocationID != "" {
		t.Fatalf("a vpc-scoped allocation carries a parent: %#v", *a)
	}
}

// --- the worker's recovery agrees, because it calls the same functions ------

// crashedVPCAdoption is crashedAdoption's counterpart for a VPC that has
// children: the same durable hold, planned through the real path, with the
// adapter's answer lost.
func crashedVPCAdoption(t *testing.T, now time.Time, networks []domain.Network, resources []domain.Resource) adoptionUnderRecovery {
	t.Helper()
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{
		networks: networks, adoptID: adoptedNetworkID,
		adoptErr: errors.New("the adapter never answered"),
	}
	observer := &safetyObserver{observation: safetyObservation(now, resources...)}
	s := adoptService(now, ledger, inventory, observer)

	a, op, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 202 || a == nil || a.Committed || op == nil || op.Status != operationPending {
		t.Fatalf("planning the adoption of a VPC with children: allocation=%#v operation=%#v status=%d err=%v", a, op, status, err)
	}
	inventory.adoptErr, inventory.adopts = nil, nil
	return adoptionUnderRecovery{s, ledger, inventory, observer, *a}
}

func TestAdoptionRecoveryExemptsTheReviewedVPCsChildren(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	networks, resources := vpcWithChildren()
	c := crashedVPCAdoption(t, now, networks, resources)

	if err := c.service.recoverAdoptions(context.Background()); err != nil {
		t.Fatalf("recover adoptions: %v", err)
	}
	got := safetyState(t, c.ledger, c.allocation.ID)
	if !got.Committed || got.InventoryID != adoptedNetworkID {
		t.Fatalf("recovery did not commit a VPC whose children are its own: %#v", got)
	}
	if f := findingsWithCode(t, c.ledger, "adoption_stuck"); len(f) != 0 {
		t.Fatalf("recovering a VPC with children raised a finding: %#v", f)
	}
}

// Recovery reads the same rules from the other side: evidence that stops being
// a child while the operation is pending leaves the adoption stuck rather than
// committing it on the strength of the review alone.
func TestAdoptionRecoveryRefusesAChildThatIsNoLongerOne(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	networks, resources := vpcWithChildren()

	stranger := childResource(childAResourceID, childACIDR, childAZone)
	stranger.ParentID = "vpc-somebody-elses"

	cases := map[string][]domain.Resource{
		"a child now belongs to another VPC": {adoptedResource(), stranger, childResource(childBResourceID, childBCIDR, childBZone)},
		// The child is gone from the cloud while its imported prefix remains:
		// an orphan prefix inside the pin that nothing accounts for.
		"a child's imported prefix is orphaned": {adoptedResource(), childResource(childBResourceID, childBCIDR, childBZone)},
	}
	for name, observed := range cases {
		t.Run(name, func(t *testing.T) {
			c := crashedVPCAdoption(t, now, networks, resources)
			c.observer.observation = safetyObservation(now, observed...)

			if err := c.service.recoverAdoptions(context.Background()); err != nil {
				t.Fatalf("recover adoptions: %v", err)
			}
			assertStillPending(t, c.ledger, c.allocation.ID)
			found := findingsWithCode(t, c.ledger, "adoption_stuck")
			if len(found) != 1 || found[0].Severity != "CRITICAL" || found[0].Status != "OPEN" {
				t.Fatalf("want one open CRITICAL adoption_stuck finding, got %#v", found)
			}
		})
	}
}

// The three edges of the exemption that nothing else exercises, asked of the
// predicate itself.
func TestTheChildPredicateIsStrictAtItsEdges(t *testing.T) {
	pin := netip.MustParsePrefix(adoptedCIDR)
	// The account is filled in by validateRequest from the principal on the real
	// path; the predicate is asked directly here, so the request carries it.
	request := safetyRequest("orders")
	request.AccountID = "123456789012"
	child := childResource("subnet-a", "10.7.0.0/26", "euw1-az1")
	if !childOfReviewedVPC(child, request, pin, adoptedResourceID) {
		t.Fatal("the fixture child is not a child: the cases below would prove nothing")
	}

	// No reviewed resource id, no children: an orphan subnet whose parent id is
	// empty must never match an empty reviewed id.
	orphan := child
	orphan.ParentID = ""
	if childOfReviewedVPC(orphan, request, pin, "") {
		t.Fatal("a subnet without a parent matched an empty reviewed resource id")
	}

	// A "child" wider than the pin is not inside it, even though its first
	// address is.
	wider := child
	wider.CIDR, wider.CIDRs = "10.7.0.0/23", []string{"10.7.0.0/23"}
	if childOfReviewedVPC(wider, request, pin, adoptedResourceID) {
		t.Fatal("a subnet wider than the reviewed VPC counted as its child")
	}
}

// AWS lets one subnet span its whole VPC, so a child's CIDR may equal the pin.
// That must never turn a SECOND imported prefix at the pinned CIDR into an
// exempt child: two prefixes at the reviewed CIDR is an ambiguity about which
// object is being adopted, and the inventory network exemption is strictly
// inside the pin for that reason.
func TestASecondPrefixAtThePinnedCIDRIsNotAChildEvenUnderAWholeVPCSubnet(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	duplicate := childNetwork("4299", adoptedCIDR)
	spanning := childResource("subnet-whole-vpc", adoptedCIDR, "euw1-az1")
	s, ledger, inventory := adoptFixture(now,
		[]domain.Network{adoptedNetwork(), duplicate},
		[]domain.Resource{adoptedResource(), spanning})

	_, _, _, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	assertNothingWritten(t, ledger, inventory)
}
