package onboard

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// testDomainID is used by every test that wants a valid, VRF-configured
// domain; testCfg's single pool ("pool1") covers 10.0.0.0/8 unless a test
// overrides it.
const testDomainID = "d1"

func testCfg(pools ...domain.Pool) domain.Config {
	if len(pools) == 0 {
		pools = []domain.Pool{{ID: "pool1", DomainID: testDomainID, CIDR: "10.0.0.0/8"}}
	}
	return domain.Config{
		Domains: []domain.Domain{{ID: testDomainID, Backend: domain.Backend{Type: "netbox", VRFID: 100}}},
		Pools:   pools,
	}
}

func findRule(findings []Finding, rule string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

func rowsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- design section 5, row: two rows with the same CIDR ---

func TestPlanDuplicateCIDRCollapsesToOneWriteWithWarning(t *testing.T) {
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 2, CIDR: "10.9.0.0/24", AccountID: "111111111111"},
		{SourceRow: 1, CIDR: "10.9.0.0/24", AccountID: "222222222222"},
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 1 {
		t.Fatalf("Writes = %+v, want exactly 1 (duplicates collapsed)", report.Writes)
	}
	w := report.Writes[0]
	if w.Kind != WritePrefix || w.CIDR != "10.9.0.0/24" {
		t.Errorf("write = %+v, want prefix 10.9.0.0/24", w)
	}
	if !rowsEqual(w.SourceRows, []int{1, 2}) {
		t.Errorf("SourceRows = %v, want [1 2] (sorted, every source listed)", w.SourceRows)
	}

	dups := findRule(report.Findings, RuleDuplicateCIDR)
	if len(dups) != 1 || dups[0].Level != LevelWarning {
		t.Fatalf("duplicate findings = %+v, want exactly one warning", dups)
	}
	if !rowsEqual(dups[0].Rows, []int{1, 2}) {
		t.Errorf("duplicate finding Rows = %v, want [1 2]", dups[0].Rows)
	}
	if report.HasErrors() {
		t.Error("HasErrors() = true, want false (a duplicate is a warning, not an error)")
	}
}

// --- design section 5, row: row equals a configured pool CIDR ---

func TestPlanRowEqualsPoolCIDRIsError(t *testing.T) {
	cfg := testCfg(domain.Pool{ID: "pool1", DomainID: testDomainID, CIDR: "10.0.0.0/16"})
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "10.0.0.0/16"},
	}}
	report := Plan(table, cfg, testDomainID, domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none: apply must refuse this row", report.Writes)
	}
	found := findRule(report.Findings, RulePoolExactMatch)
	if len(found) != 1 || found[0].Level != LevelError {
		t.Fatalf("findings = %+v, want exactly one pool-exact-match error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true: apply would be refused")
	}
}

// --- design section 5, row: row contains a configured pool ---

func TestPlanRowContainsPoolIsError(t *testing.T) {
	cfg := testCfg(domain.Pool{ID: "pool1", DomainID: testDomainID, CIDR: "10.5.0.0/24"})
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "10.5.0.0/16"},
	}}
	report := Plan(table, cfg, testDomainID, domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none", report.Writes)
	}
	found := findRule(report.Findings, RulePoolAncestor)
	if len(found) != 1 || found[0].Level != LevelError {
		t.Fatalf("findings = %+v, want exactly one pool-ancestor error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
}

// --- design section 5, row: row overlaps a prefix carrying platform_allocation_id ---

func TestPlanRowOverlapsManagedNetworkIsError(t *testing.T) {
	// A managed *subnet* (it names a parent allocation). Nothing may be
	// imported inside one: a subnet is a leaf as far as this project models
	// address space. The vpc-scoped exception the 2026-09-20 amendment to
	// ADR 0010 adds is covered by TestPlanRowInsideManagedVPCIsImported below,
	// together with every overlap shape that still fails here.
	snap := domain.InventorySnapshot{Complete: true, Networks: []domain.Network{
		{ID: "1", CIDR: "10.1.0.0/24", AllocationID: "alloc-1", ParentAllocationID: "alloc-parent"},
	}}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "10.1.0.0/25"}, // subset, overlaps but is not equal
	}}
	report := Plan(table, testCfg(), testDomainID, snap)

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none", report.Writes)
	}
	found := findRule(report.Findings, RuleOverlapsManaged)
	if len(found) != 1 || found[0].Level != LevelError {
		t.Fatalf("findings = %+v, want exactly one overlaps-managed error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
}

// --- ADR 0010 as amended on 2026-09-20: a child of a managed VPC ---

// A VPC can be adopted before all of its subnets exist, so a subnet created
// afterwards has to be importable as occupancy or it could never be brought
// under management at all. The evidence is the snapshot's alone: a managed
// network naming no parent allocation is a vpc-scoped one (see
// domain.Network.ParentAllocationID), and onboard never opens the ledger.
func TestPlanRowInsideManagedVPCIsImported(t *testing.T) {
	snap := domain.InventorySnapshot{Complete: true, Networks: []domain.Network{
		{ID: "1", CIDR: "10.1.0.0/22", AllocationID: "alloc-vpc"},
	}}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "10.1.1.0/24"},
	}}
	report := Plan(table, testCfg(), testDomainID, snap)

	if report.HasErrors() {
		t.Fatalf("findings = %+v, want no error for a child of a managed VPC", report.Findings)
	}
	if len(report.Writes) != 1 || report.Writes[0].CIDR != "10.1.1.0/24" {
		t.Fatalf("Writes = %+v, want the child written as unmanaged occupancy", report.Writes)
	}
	found := findRule(report.Findings, RuleInsideManagedVPC)
	if len(found) != 1 || found[0].Level != LevelInfo {
		t.Fatalf("findings = %+v, want exactly one inside-managed-vpc info", found)
	}
	// The finding has to name the allocation, or an operator reading the
	// report cannot tell whose VPC they are writing inside.
	if !strings.Contains(found[0].Message, "alloc-vpc") {
		t.Errorf("message %q does not name the parent allocation", found[0].Message)
	}
	if len(findRule(report.Findings, RuleOverlapsManaged)) != 0 {
		t.Errorf("findings = %+v, want no overlaps-managed error", report.Findings)
	}
}

// The exception is exactly "strictly inside a managed VPC" and nothing near
// it. Each shape below still has to be refused: they describe space the
// platform has already issued, or a second prefix at a CIDR the snapshot
// already holds, which takes the whole domain's snapshot down.
//
// A prefix row cannot *partly* overlap a managed prefix -- two CIDR blocks
// either nest or are disjoint -- so that shape is a range's to exercise, and
// TestPlanRangeInsideManagedVPCIsImported does.
func TestPlanStillRefusesEveryOtherOverlapWithManagedSpace(t *testing.T) {
	vpc := domain.Network{ID: "1", CIDR: "10.1.0.0/22", AllocationID: "alloc-vpc"}
	subnet := domain.Network{ID: "2", CIDR: "10.1.1.0/24", AllocationID: "alloc-subnet", ParentAllocationID: "alloc-vpc"}
	cases := map[string]struct {
		networks []domain.Network
		row      string
	}{
		"equal to the managed VPC":             {[]domain.Network{vpc}, "10.1.0.0/22"},
		"containing the managed VPC":           {[]domain.Network{vpc}, "10.1.0.0/20"},
		"equal to a managed subnet":            {[]domain.Network{vpc, subnet}, "10.1.1.0/24"},
		"inside a managed subnet":              {[]domain.Network{vpc, subnet}, "10.1.1.0/25"},
		"inside a VPC but over a subnet":       {[]domain.Network{vpc, subnet}, "10.1.1.128/25"},
		"inside a VPC but containing a subnet": {[]domain.Network{vpc, subnet}, "10.1.0.0/23"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			snap := domain.InventorySnapshot{Complete: true, Networks: tc.networks}
			table := Table{Kind: KindNetworks, Networks: []NetworkRow{{SourceRow: 1, CIDR: tc.row}}}
			report := Plan(table, testCfg(), testDomainID, snap)

			if len(report.Writes) != 0 {
				t.Fatalf("Writes = %+v, want none", report.Writes)
			}
			found := findRule(report.Findings, RuleOverlapsManaged)
			if len(found) != 1 || found[0].Level != LevelError {
				t.Fatalf("findings = %+v, want exactly one overlaps-managed error", report.Findings)
			}
		})
	}
}

// "straddling the managed VPC" above is a /23 that covers the VPC's upper
// half and as much again outside it; this is its range equivalent, plus the
// range that is genuinely a child.
func TestPlanRangeInsideManagedVPCIsImported(t *testing.T) {
	vpc := domain.Network{ID: "1", CIDR: "10.4.0.0/24", AllocationID: "alloc-vpc"}
	cases := map[string]struct {
		start, end string
		refused    bool
	}{
		"strictly inside":          {"10.4.0.10", "10.4.0.20", false},
		"the whole managed prefix": {"10.4.0.0", "10.4.0.255", true},
		"reaching past the end":    {"10.4.0.10", "10.4.1.20", true},
		"starting before it":       {"10.3.255.250", "10.4.0.20", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			snap := domain.InventorySnapshot{Complete: true, Networks: []domain.Network{vpc}}
			table := Table{Kind: KindRanges, Ranges: []RangeRow{
				{SourceRow: 1, StartAddress: tc.start, EndAddress: tc.end},
			}}
			report := Plan(table, testCfg(), testDomainID, snap)

			if tc.refused {
				if len(report.Writes) != 0 || len(findRule(report.Findings, RuleOverlapsManaged)) != 1 {
					t.Fatalf("Writes = %+v, findings = %+v, want one overlaps-managed error and no write", report.Writes, report.Findings)
				}
				return
			}
			if report.HasErrors() || len(report.Writes) != 1 {
				t.Fatalf("Writes = %+v, findings = %+v, want the range written", report.Writes, report.Findings)
			}
			if found := findRule(report.Findings, RuleInsideManagedVPC); len(found) != 1 || found[0].Level != LevelInfo {
				t.Fatalf("findings = %+v, want exactly one inside-managed-vpc info", report.Findings)
			}
		})
	}
}

// A pool's own container prefix carries no allocation id, so it was never
// managed space to begin with -- but it also names no parent, and this rule
// must not start reading it as a VPC an import may write inside. The existing
// pool rules are what refuse a row that equals or contains a pool, and they
// have to keep being the ones that do it.
func TestPlanDoesNotTreatThePoolContainerAsAManagedVPC(t *testing.T) {
	cfg := testCfg(domain.Pool{ID: "pool1", DomainID: testDomainID, CIDR: "10.9.0.0/16"})
	snap := domain.InventorySnapshot{Complete: true, Networks: []domain.Network{
		{ID: "1", CIDR: "10.9.0.0/16", ParentPool: true},
	}}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{{SourceRow: 1, CIDR: "10.9.1.0/24"}}}
	report := Plan(table, cfg, testDomainID, snap)

	if report.HasErrors() || len(report.Writes) != 1 {
		t.Fatalf("Writes = %+v, findings = %+v, want an ordinary import inside the pool", report.Writes, report.Findings)
	}
	if found := findRule(report.Findings, RuleInsideManagedVPC); len(found) != 0 {
		t.Fatalf("the pool container was reported as a managed VPC: %+v", found)
	}
}

// --- design section 5, row: row already present in NetBox as unmanaged ---

func TestPlanRowAlreadyUnmanagedIsInfoAndSkipsWrite(t *testing.T) {
	snap := domain.InventorySnapshot{Complete: true, Networks: []domain.Network{
		{ID: "1", CIDR: "10.2.0.0/24"}, // no AllocationID, not a pool: unmanaged
	}}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "10.2.0.0/24"},
	}}
	report := Plan(table, testCfg(), testDomainID, snap)

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none: apply must be idempotent", report.Writes)
	}
	found := findRule(report.Findings, RuleAlreadyUnmanaged)
	if len(found) != 1 || found[0].Level != LevelInfo {
		t.Fatalf("findings = %+v, want exactly one already-unmanaged info", found)
	}
	if report.HasErrors() {
		t.Error("HasErrors() = true, want false: this is informational only")
	}
}

// A pool's own prefix (ParentPool == true, no AllocationID) must not be
// treated as "already unmanaged" -- that would let plan silently swallow a
// row that in fact equals or contains the pool.
func TestPlanAlreadyUnmanagedIgnoresThePoolsOwnPrefix(t *testing.T) {
	cfg := testCfg(domain.Pool{ID: "pool1", DomainID: testDomainID, CIDR: "10.0.0.0/16"})
	snap := domain.InventorySnapshot{Complete: true, Networks: []domain.Network{
		{ID: "1", CIDR: "10.0.0.0/16", ParentPool: true},
	}}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "10.0.0.0/16"},
	}}
	report := Plan(table, cfg, testDomainID, snap)

	if len(findRule(report.Findings, RuleAlreadyUnmanaged)) != 0 {
		t.Errorf("findings = %+v, must not classify the pool's own prefix as already-unmanaged", report.Findings)
	}
	if len(findRule(report.Findings, RulePoolExactMatch)) != 1 {
		t.Errorf("findings = %+v, want the pool-exact-match error instead", report.Findings)
	}
}

// --- design section 5, row: row outside every pool ---

func TestPlanRowOutsideEveryPoolIsInfoAndStillWritten(t *testing.T) {
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "192.168.1.0/24"}, // outside the 10.0.0.0/8 pool
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 1 || report.Writes[0].CIDR != "192.168.1.0/24" {
		t.Fatalf("Writes = %+v, want the row written despite being outside every pool", report.Writes)
	}
	found := findRule(report.Findings, RuleOutsidePool)
	if len(found) != 1 || found[0].Level != LevelInfo {
		t.Fatalf("findings = %+v, want exactly one outside-pool info", found)
	}
	if report.HasErrors() {
		t.Error("HasErrors() = true, want false")
	}
}

// --- design section 5, row: overlapping but unequal rows are allowed ---

func TestPlanOverlappingUnequalRowsAreAllowedWithNoFinding(t *testing.T) {
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "10.2.0.0/16"}, // VPC
		{SourceRow: 2, CIDR: "10.2.1.0/24"}, // one of its subnets
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 2 {
		t.Fatalf("Writes = %+v, want both rows written", report.Writes)
	}
	if len(report.Findings) != 0 {
		t.Errorf("Findings = %+v, want none: an overlapping-but-unequal VPC/subnet pair is allowed", report.Findings)
	}
}

// --- design section 5, row: range expanding to more than 4096 blocks ---

// internal/netbox/client.go's rangeCIDRs decomposition is provably bounded
// (the classic "address range to CIDR blocks" algorithm needs at most
// roughly 2*bits blocks -- about 62 for any IPv4 pair), so
// maxOnboardRangeBlocks (4096, mirroring internal/netbox/client.go's
// maxRangeBlocks) can never actually be reached from real IPv4 start/end
// input. This test exercises the exact mechanism Plan uses to decide
// RuleRangeTooLarge -- with a limit small enough to hit -- rather than
// through Plan itself, since no real IPv4 range can trigger the production
// threshold. Reported to the work-plan lead as a design/adapter mismatch:
// see the C3 report.
func TestRangeBlockCountHitsCapAndDoesNotAtTheRealThreshold(t *testing.T) {
	start := netip.MustParseAddr("10.0.0.1")
	end := netip.MustParseAddr("10.0.0.254")

	if blocks, hitCap := rangeBlockCount(start, end, 4); !hitCap {
		t.Fatalf("rangeBlockCount(%s-%s, limit=4) = (%d, %v), want hitCap=true", start, end, blocks, hitCap)
	}
	if blocks, hitCap := rangeBlockCount(start, end, maxOnboardRangeBlocks); hitCap {
		t.Errorf("rangeBlockCount(%s-%s, limit=%d) = (%d, %v); the production cap is unreachable for this IPv4 range", start, end, maxOnboardRangeBlocks, blocks, hitCap)
	}
}

// --- design section 5, row: a range that spans/contains a pool ---

func TestPlanRangeSpanningPoolIsError(t *testing.T) {
	cfg := testCfg(domain.Pool{ID: "pool1", DomainID: testDomainID, CIDR: "10.3.0.0/24"})
	table := Table{Kind: KindRanges, Ranges: []RangeRow{
		{SourceRow: 1, StartAddress: "10.3.0.0", EndAddress: "10.3.0.255"},
	}}
	report := Plan(table, cfg, testDomainID, domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none", report.Writes)
	}
	found := findRule(report.Findings, RuleRangeSpansPool)
	if len(found) != 1 || found[0].Level != LevelError {
		t.Fatalf("findings = %+v, want exactly one range-spans-pool error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
}

func TestPlanRangeOverlappingManagedNetworkIsError(t *testing.T) {
	// A managed *subnet*, as in TestPlanRowOverlapsManagedNetworkIsError: a
	// range inside a managed VPC is importable since the 2026-09-20 amendment
	// to ADR 0010, and inside a managed subnet it is not.
	snap := domain.InventorySnapshot{Complete: true, Networks: []domain.Network{
		{ID: "1", CIDR: "10.4.0.0/24", AllocationID: "alloc-2", ParentAllocationID: "alloc-parent"},
	}}
	table := Table{Kind: KindRanges, Ranges: []RangeRow{
		{SourceRow: 1, StartAddress: "10.4.0.10", EndAddress: "10.4.0.20"},
	}}
	report := Plan(table, testCfg(), testDomainID, snap)

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none", report.Writes)
	}
	found := findRule(report.Findings, RuleOverlapsManaged)
	if len(found) != 1 || found[0].Level != LevelError {
		t.Fatalf("findings = %+v, want exactly one overlaps-managed error", found)
	}
}

func TestPlanRangeWriteAndDuplicateCollapse(t *testing.T) {
	table := Table{Kind: KindRanges, Ranges: []RangeRow{
		{SourceRow: 2, StartAddress: "192.168.9.1", EndAddress: "192.168.9.50"},
		{SourceRow: 1, StartAddress: "192.168.9.1", EndAddress: "192.168.9.50"},
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 1 {
		t.Fatalf("Writes = %+v, want exactly 1 (duplicate ranges collapsed)", report.Writes)
	}
	w := report.Writes[0]
	if w.Kind != WriteRange || w.StartAddress != "192.168.9.1" || w.EndAddress != "192.168.9.50" {
		t.Errorf("write = %+v, want the range 192.168.9.1-192.168.9.50", w)
	}
	if !rowsEqual(w.SourceRows, []int{1, 2}) {
		t.Errorf("SourceRows = %v, want [1 2]", w.SourceRows)
	}
	if len(findRule(report.Findings, RuleDuplicateCIDR)) != 1 {
		t.Errorf("findings = %+v, want one duplicate-cidr warning", report.Findings)
	}
	// Outside the 10.0.0.0/8 pool: also expect the info finding, still written.
	if len(findRule(report.Findings, RuleOutsidePool)) != 1 {
		t.Errorf("findings = %+v, want one outside-pool info", report.Findings)
	}
}

// --- design section 5, row: account id not 12 digits; role ARN mismatch ---

func TestPlanAccountsInvalidAccountIDIsError(t *testing.T) {
	table := Table{Kind: KindAccounts, Accounts: []AccountRow{
		{SourceRow: 1, AccountID: "12345"},
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	found := findRule(report.Findings, RuleInvalidAccountID)
	if len(found) != 1 || found[0].Level != LevelError {
		t.Fatalf("findings = %+v, want exactly one invalid-account-id error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
}

func TestPlanAccountsRoleARNNotMatchingAccountIsError(t *testing.T) {
	table := Table{Kind: KindAccounts, Accounts: []AccountRow{
		{SourceRow: 1, AccountID: "123456789012", RoleARN: "arn:aws:iam::999999999999:role/Foo"},
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	found := findRule(report.Findings, RuleInvalidRoleARN)
	if len(found) != 1 || found[0].Level != LevelError {
		t.Fatalf("findings = %+v, want exactly one invalid-role-arn error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
}

func TestPlanAccountsValidRowProducesNoFindings(t *testing.T) {
	table := Table{Kind: KindAccounts, Accounts: []AccountRow{
		{SourceRow: 1, AccountID: "123456789012", RoleARN: "arn:aws:iam::123456789012:role/PlatformIpamReadOnly"},
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})
	if len(report.Findings) != 0 {
		t.Errorf("Findings = %+v, want none", report.Findings)
	}
	if report.HasErrors() {
		t.Error("HasErrors() = true, want false")
	}
}

// --- design section 5, row: unknown domainID / domain without a VRF ---

func TestPlanUnknownDomainIsTableLevelError(t *testing.T) {
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{{SourceRow: 1, CIDR: "10.1.0.0/24"}}}
	report := Plan(table, testCfg(), "does-not-exist", domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none: an unknown domain cannot be classified against", report.Writes)
	}
	found := findRule(report.Findings, RuleUnknownDomain)
	if len(found) != 1 || found[0].Level != LevelError || len(found[0].Rows) != 0 {
		t.Fatalf("findings = %+v, want exactly one table-level (no Rows) unknown-domain error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
}

func TestPlanDomainWithoutVRFIsTableLevelError(t *testing.T) {
	cfg := domain.Config{Domains: []domain.Domain{{ID: testDomainID}}} // Backend.VRFID left at zero
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{{SourceRow: 1, CIDR: "10.1.0.0/24"}}}
	report := Plan(table, cfg, testDomainID, domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none", report.Writes)
	}
	found := findRule(report.Findings, RuleDomainNoVRF)
	if len(found) != 1 || found[0].Level != LevelError || len(found[0].Rows) != 0 {
		t.Fatalf("findings = %+v, want exactly one table-level domain-no-vrf error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
}

// --- design section 5, row: IPv6 or non-canonical CIDR that reached Plan ---

func TestPlanDefenseInDepthRejectsNonCanonicalCIDR(t *testing.T) {
	// Package C1's normalizer refuses this (see normalize_test.go's
	// TestNormalizeRejectsNonCanonicalCIDR); Plan re-checks anyway rather
	// than trusting the caller assembled the Table through Normalize.
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "10.1.2.3/16"}, // host bits set
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none", report.Writes)
	}
	found := findRule(report.Findings, RuleInvalidCIDR)
	if len(found) != 1 || found[0].Level != LevelError {
		t.Fatalf("findings = %+v, want exactly one invalid-cidr error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
}

func TestPlanDefenseInDepthRejectsIPv6(t *testing.T) {
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "2001:db8::/32"},
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	found := findRule(report.Findings, RuleInvalidCIDR)
	if len(found) != 1 || found[0].Level != LevelError {
		t.Fatalf("findings = %+v, want exactly one invalid-cidr error", found)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
}

// --- C1 diagnostics carried through ---

func TestPlanCarriesNormalizeDiagnosticsAsFindings(t *testing.T) {
	table := Table{
		Kind:     KindNetworks,
		Networks: []NetworkRow{{SourceRow: 1, CIDR: "10.1.0.0/24"}},
		Diagnostics: []Diagnostic{
			{Level: LevelError, Row: 2, Column: "cidr", Message: `"10.9.9.9/33" is not a valid CIDR`},
			{Level: LevelWarning, Row: 3, Column: "account_id", Message: `padded`},
		},
	}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	norm := findRule(report.Findings, RuleNormalize)
	if len(norm) != 2 {
		t.Fatalf("normalize findings = %+v, want 2 (one per Diagnostic)", norm)
	}
	if !report.HasErrors() {
		t.Error("HasErrors() = false, want true: an error Diagnostic from normalization is an error in the plan")
	}
}

// --- determinism ---

func TestPlanFindingsAndWritesAreSortedStably(t *testing.T) {
	// All three CIDRs are outside the 10.0.0.0/8 pool, so each also raises an
	// outside-pool info Finding: this exercises Findings' sort order too, not
	// just Writes'.
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 3, CIDR: "192.168.30.0/24"},
		{SourceRow: 1, CIDR: "192.168.10.0/24"},
		{SourceRow: 2, CIDR: "192.168.20.0/24"},
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	want := []string{"192.168.10.0/24", "192.168.20.0/24", "192.168.30.0/24"}
	if len(report.Writes) != len(want) {
		t.Fatalf("Writes = %+v, want %d entries", report.Writes, len(want))
	}
	for i, w := range want {
		if report.Writes[i].CIDR != w {
			t.Errorf("Writes[%d].CIDR = %q, want %q (writes must be sorted by CIDR, not input order)", i, report.Writes[i].CIDR, w)
		}
	}
	if len(report.Findings) != len(want) {
		t.Fatalf("Findings = %+v, want %d outside-pool entries", report.Findings, len(want))
	}
	for i := range want {
		if report.Findings[i].CIDR != want[i] {
			t.Errorf("Findings[%d].CIDR = %q, want %q (findings sorted by CIDR)", i, report.Findings[i].CIDR, want[i])
		}
	}
}

// --- all-clean table ---

func TestPlanAllCleanTableYieldsNoErrorsAndExpectedWriteSet(t *testing.T) {
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{
		{SourceRow: 1, CIDR: "10.11.0.0/24"},
		{SourceRow: 2, CIDR: "10.12.0.0/24"},
	}}
	report := Plan(table, testCfg(), testDomainID, domain.InventorySnapshot{Complete: true})

	if report.HasErrors() {
		t.Fatalf("HasErrors() = true, want false: Findings=%+v", report.Findings)
	}
	if len(report.Writes) != 2 {
		t.Fatalf("Writes = %+v, want 2", report.Writes)
	}
	if report.Writes[0].CIDR != "10.11.0.0/24" || report.Writes[1].CIDR != "10.12.0.0/24" {
		t.Errorf("Writes = %+v, want [10.11.0.0/24 10.12.0.0/24]", report.Writes)
	}
	if len(report.Findings) != 0 {
		t.Errorf("Findings = %+v, want none (both rows are inside the pool, no duplicates, nothing managed)", report.Findings)
	}
}

// A ranges table whose rows are given as a CIDR (not start/end) must be
// validated exactly like a networks row -- this is what
// docs/ONBOARDING_IMPORT.md section 3 calls a ranges table's "cidr"
// required-column alternative.
func TestPlanRangesTableCIDRRowsUseThePrefixRules(t *testing.T) {
	cfg := testCfg(domain.Pool{ID: "pool1", DomainID: testDomainID, CIDR: "10.6.0.0/24"})
	table := Table{Kind: KindRanges, Ranges: []RangeRow{
		{SourceRow: 1, CIDR: "10.6.0.0/24"}, // equals the pool
	}}
	report := Plan(table, cfg, testDomainID, domain.InventorySnapshot{Complete: true})

	found := findRule(report.Findings, RulePoolExactMatch)
	if len(found) != 1 {
		t.Fatalf("findings = %+v, want the pool-exact-match rule applied to a ranges-table CIDR row too", report.Findings)
	}
}

// An unreadable inventory is not an empty one. Without this rule every
// "already present" and "overlaps managed" check passes vacuously.
func TestPlanRefusesIncompleteSnapshot(t *testing.T) {
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{{
		SourceRow: 2, AccountID: "111111111111", Region: "eu-central-1", CIDR: "10.64.8.0/22",
	}}}
	cfg := domain.Config{
		Domains: []domain.Domain{{ID: "d1", Backend: domain.Backend{Type: "netbox", VRFID: 7}}},
		Pools:   []domain.Pool{{ID: "p1", DomainID: "d1", CIDR: "10.64.0.0/16"}},
	}
	report := Plan(table, cfg, "d1", domain.InventorySnapshot{Complete: false})
	if !report.HasErrors() {
		t.Fatal("Plan accepted an incomplete inventory snapshot")
	}
	if len(report.Writes) != 0 {
		t.Errorf("writes = %d, want none", len(report.Writes))
	}
	if report.Findings[0].Rule != RuleIncompleteSnapshot {
		t.Errorf("rule = %q, want %q", report.Findings[0].Rule, RuleIncompleteSnapshot)
	}
}
