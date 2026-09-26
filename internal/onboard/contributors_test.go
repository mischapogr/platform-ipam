package onboard

import (
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Package M9b2 (ADR 0016, "the comparison, read-only"): Plan gains four
// findings, computed from the contributor lists a snapshot now carries
// against what this table's own rows for the same CIDR would write. These
// tests exercise contributorFindings and its callers inside Plan directly;
// internal/onboardcmd/contributors_test.go covers the create path
// (entryOccupancy) and the end-to-end wiring between the two.

func strp(s string) *string { return &s }

// contribRow is a collector-shaped networks row (post-M1b1: both new columns
// present), mirroring internal/onboardcmd/contributors_test.go's
// contributorRow so the two packages' fixtures read the same way.
func contribRow(cidr, account, resourceID string, sourceRow int) NetworkRow {
	return NetworkRow{
		SourceRow: sourceRow, CIDR: cidr, AccountID: account, Region: "eu-central-1",
		Type: "vpc", ResourceID: resourceID, Name: "vpc-" + resourceID,
		AssociationID: "assoc-" + resourceID, ObservedAt: "2026-09-22T10:00:00Z",
		SourceFile:                 "networks.csv",
		AssociationIDColumnPresent: true, ObservedAtColumnPresent: true,
	}
}

// oldFormatRow is a pre-M1b1 row: neither association_id nor observed_at was
// ever a column in its file, so both flags are false and both values are
// null once mapped -- never a blank cell, an absent column.
func oldFormatRow(cidr, account, resourceID string, sourceRow int) NetworkRow {
	return NetworkRow{
		SourceRow: sourceRow, CIDR: cidr, AccountID: account, Region: "eu-central-1",
		Type: "vpc", ResourceID: resourceID, Name: "vpc-" + resourceID,
		SourceFile: "legacy.csv",
	}
}

func identityOf(row NetworkRow) string { return MapNetworkRow(row).Identity() }

func planWithSnap(table Table, snap domain.InventorySnapshot) Report {
	return Plan(table, testCfg(domain.Pool{ID: "pool1", DomainID: testDomainID, CIDR: "10.0.0.0/8"}), testDomainID, snap)
}

// --- contributor-new -------------------------------------------------

func TestContributorNewForARowTheExistingListDoesNotCarry(t *testing.T) {
	a := contribRow("10.20.0.0/24", "000000000001", "vpc-a", 1)
	b := contribRow("10.20.0.0/24", "000000000002", "vpc-b", 2)
	existing := domain.Network{ID: "1", CIDR: "10.20.0.0/24", Contributors: []domain.Contributor{
		{Identity: identityOf(a), AccountID: a.AccountID, Region: a.Region, ResourceID: a.ResourceID,
			AssociationID: strp(a.AssociationID), ObservedAt: strp(a.ObservedAt), FirstSeenBatch: "batch-0", LastSeenBatch: "batch-0"},
	}}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{a, b}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true, Networks: []domain.Network{existing}})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none: this CIDR is already unmanaged occupancy", report.Writes)
	}
	news := findRule(report.Findings, RuleContributorNew)
	if len(news) != 1 || news[0].Level != LevelInfo {
		t.Fatalf("findings = %+v, want exactly one contributor-new info", report.Findings)
	}
	if !strings.Contains(news[0].Message, identityOf(b)) {
		t.Errorf("message %q does not name %s", news[0].Message, identityOf(b))
	}
	if !rowsEqual(news[0].Rows, []int{2}) {
		t.Errorf("Rows = %v, want [2] (b's own source row)", news[0].Rows)
	}
	// a is already a recorded contributor: it must not also be reported new.
	if strings.Contains(news[0].Message, identityOf(a)) {
		t.Errorf("message %q must not also name the already-recorded contributor %s", news[0].Message, identityOf(a))
	}
	if len(findRule(report.Findings, RuleContributorAbsent)) != 0 {
		t.Errorf("findings = %+v, want no contributor-absent: nothing recorded is missing from this table", report.Findings)
	}
}

// --- contributor-absent: a table is a scope, not a census -------------

func TestContributorAbsentIsWarningAndNeverAWrite(t *testing.T) {
	a := contribRow("10.20.1.0/24", "000000000001", "vpc-a", 1)
	b := contribRow("10.20.1.0/24", "000000000002", "vpc-b", 2)
	existing := domain.Network{ID: "1", CIDR: "10.20.1.0/24", Contributors: []domain.Contributor{
		{Identity: identityOf(a), AccountID: a.AccountID, ResourceID: a.ResourceID, Region: a.Region},
		{Identity: identityOf(b), AccountID: b.AccountID, ResourceID: b.ResourceID, Region: b.Region},
	}}
	// This table covers account 1 only -- vpc-a -- exactly the "a table
	// covering one account" scenario docs/WORK_PLAN.md's M9b2 block names.
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{a}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true, Networks: []domain.Network{existing}})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none", report.Writes)
	}
	if report.HasErrors() {
		t.Error("HasErrors() = true, want false: contributor-absent is a warning, never an error")
	}
	absent := findRule(report.Findings, RuleContributorAbsent)
	if len(absent) != 1 || absent[0].Level != LevelWarning {
		t.Fatalf("findings = %+v, want exactly one contributor-absent warning", report.Findings)
	}
	if !strings.Contains(absent[0].Message, identityOf(b)) {
		t.Errorf("message %q does not name the account-2 contributor %s the table did not cover", absent[0].Message, identityOf(b))
	}
	// Nothing about account 1's own, already-recorded contributor is "new" or
	// "absent": the table simply did not mention account 2, and that alone
	// must never be read as evidence account 2's VPC is gone.
	if len(findRule(report.Findings, RuleContributorNew)) != 0 {
		t.Errorf("findings = %+v, want no contributor-new: vpc-a is already recorded", report.Findings)
	}
}

// --- contributor-unknown: a prefix imported before ADR 0016 -----------

func TestContributorUnknownForAPrefixWithNoListAtAll(t *testing.T) {
	a := contribRow("10.20.2.0/24", "000000000001", "vpc-a", 1)
	// Contributors is nil (its zero value): a prefix EnsureOccupancy wrote
	// before package M9b1 ever ran.
	existing := domain.Network{ID: "1", CIDR: "10.20.2.0/24"}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{a}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true, Networks: []domain.Network{existing}})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none", report.Writes)
	}
	unknown := findRule(report.Findings, RuleContributorUnknown)
	if len(unknown) != 1 || unknown[0].Level != LevelInfo {
		t.Fatalf("findings = %+v, want exactly one contributor-unknown info", report.Findings)
	}
	for _, rule := range []string{RuleContributorNew, RuleContributorAbsent, RuleContributorStaleSource, RuleContributorUnreadable} {
		if len(findRule(report.Findings, rule)) != 0 {
			t.Errorf("findings = %+v, want no %s alongside contributor-unknown: there is nothing to compare against", report.Findings, rule)
		}
	}
}

// --- contributor-unreadable: its own case, never reconstructed --------

func TestContributorUnreadableIsItsOwnCaseNotUnknown(t *testing.T) {
	a := contribRow("10.20.3.0/24", "000000000001", "vpc-a", 1)
	existing := domain.Network{ID: "1", CIDR: "10.20.3.0/24", ContributorsUnreadable: true}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{a}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true, Networks: []domain.Network{existing}})

	if len(report.Writes) != 0 {
		t.Fatalf("Writes = %+v, want none", report.Writes)
	}
	unreadable := findRule(report.Findings, RuleContributorUnreadable)
	if len(unreadable) != 1 || unreadable[0].Level != LevelWarning {
		t.Fatalf("findings = %+v, want exactly one contributor-unreadable warning", report.Findings)
	}
	// docs/WORK_PLAN.md's M9b2 block: "not contributor-unknown, never
	// reconstructed silently" -- so no other contributor finding may appear
	// for this CIDR: an unreadable list gives no basis for "new" or "absent".
	for _, rule := range []string{RuleContributorUnknown, RuleContributorNew, RuleContributorAbsent, RuleContributorStaleSource} {
		if len(findRule(report.Findings, rule)) != 0 {
			t.Errorf("findings = %+v, want no %s: an unreadable list must never be silently reconstructed", report.Findings, rule)
		}
	}
}

// --- contributor-stale-source: unusable as removal evidence -----------

func TestContributorStaleSourceForANullObservedAt(t *testing.T) {
	a := oldFormatRow("10.20.4.0/24", "000000000001", "vpc-a", 1)
	existing := domain.Network{ID: "1", CIDR: "10.20.4.0/24", Contributors: []domain.Contributor{
		// Recorded with the SAME identity an old-format row produces (no
		// association segment): this is the "already recorded" half of the
		// scenario, so stale-source is raised on its own, not entangled with
		// contributor-new.
		{Identity: identityOf(a), AccountID: a.AccountID, ResourceID: a.ResourceID, Region: a.Region},
	}}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{a}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true, Networks: []domain.Network{existing}})

	stale := findRule(report.Findings, RuleContributorStaleSource)
	if len(stale) != 1 || stale[0].Level != LevelInfo {
		t.Fatalf("findings = %+v, want exactly one contributor-stale-source info", report.Findings)
	}
	if !strings.Contains(stale[0].Message, identityOf(a)) {
		t.Errorf("message %q does not name %s", stale[0].Message, identityOf(a))
	}
	if len(findRule(report.Findings, RuleContributorNew)) != 0 {
		t.Errorf("findings = %+v, want no contributor-new: this row is already a recorded contributor", report.Findings)
	}
}

// --- degenerate identity: matches only itself, never evidence ---------

// A table with no resource_id column collapses every row at one CIDR (and
// account, region) onto the same identity string. docs/WORK_PLAN.md's M9b1
// review: "M9b2 must treat like a null observation time" -- unusable as
// evidence, in neither direction.
func TestDegenerateIdentityNeverRaisesContributorNew(t *testing.T) {
	row := NetworkRow{SourceRow: 1, CIDR: "10.20.5.0/24", AccountID: "000000000001", Region: "eu-central-1",
		Type: "vpc", SourceFile: "no-resource-id.csv"} // ResourceID left empty: no such column
	existing := domain.Network{ID: "1", CIDR: "10.20.5.0/24", Contributors: []domain.Contributor{
		// A different degenerate entry recorded earlier: same identity string
		// (empty VPC segment), unrelated account -- deliberately NOT the same
		// account as row, to show the finding is suppressed by degeneracy
		// itself, not by an accidental identity match.
		{Identity: "aws:000000000099:eu-central-1::10.20.5.0/24", AccountID: "000000000099", Region: "eu-central-1"},
	}}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{row}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true, Networks: []domain.Network{existing}})

	if got := identityOf(row); !strings.HasSuffix(got, "::10.20.5.0/24") {
		t.Fatalf("fixture sanity: %q is not the degenerate identity shape this test needs", got)
	}
	if found := findRule(report.Findings, RuleContributorNew); len(found) != 0 {
		t.Errorf("findings = %+v, want no contributor-new for a degenerate identity", found)
	}
}

func TestDegenerateIdentityNeverRaisesContributorAbsent(t *testing.T) {
	existing := domain.Network{ID: "1", CIDR: "10.20.6.0/24", Contributors: []domain.Contributor{
		{Identity: "aws:000000000001:eu-central-1::10.20.6.0/24", AccountID: "000000000001", Region: "eu-central-1"},
	}}
	// This table names an entirely different, real VPC at the same CIDR --
	// nothing here could possibly correspond to the recorded degenerate
	// entry, and it must still never be reported absent: the table's own
	// rows could, in principle, be the same resource under the same
	// collapsed identity, so "gone" cannot be shown.
	other := contribRow("10.20.6.0/24", "000000000002", "vpc-real", 1)
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{other}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true, Networks: []domain.Network{existing}})

	if found := findRule(report.Findings, RuleContributorAbsent); len(found) != 0 {
		t.Errorf("findings = %+v, want no contributor-absent for a degenerate identity", found)
	}
	// The real VPC the table does name is legitimately new.
	if found := findRule(report.Findings, RuleContributorNew); len(found) != 1 {
		t.Errorf("findings = %+v, want exactly one contributor-new for the real VPC", found)
	}
}

// --- no existing prefix: nothing to compare, nothing reported ---------

func TestNoContributorFindingsForABrandNewPrefix(t *testing.T) {
	a := contribRow("10.20.7.0/24", "000000000001", "vpc-a", 1)
	b := contribRow("10.20.7.0/24", "000000000002", "vpc-b", 2)
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{a, b}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true})

	for _, rule := range []string{RuleContributorNew, RuleContributorAbsent, RuleContributorUnknown, RuleContributorStaleSource, RuleContributorUnreadable} {
		if found := findRule(report.Findings, rule); len(found) != 0 {
			t.Errorf("findings = %+v, want no %s: there is no existing prefix to compare against yet", report.Findings, rule)
		}
	}
	if len(report.Writes) != 1 {
		t.Fatalf("Writes = %+v, want the fresh prefix written", report.Writes)
	}
}

// --- RuleDuplicateCIDR names the contributor identities ----------------

func TestRuleDuplicateCIDRMessageNamesContributorIdentities(t *testing.T) {
	a := contribRow("10.20.8.0/24", "000000000001", "vpc-a", 1)
	b := contribRow("10.20.8.0/24", "000000000002", "vpc-b", 2)
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{a, b}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true})

	dups := findRule(report.Findings, RuleDuplicateCIDR)
	if len(dups) != 1 {
		t.Fatalf("findings = %+v, want exactly one duplicate-cidr warning", report.Findings)
	}
	for _, id := range []string{identityOf(a), identityOf(b)} {
		if !strings.Contains(dups[0].Message, id) {
			t.Errorf("message %q does not name contributor %s", dups[0].Message, id)
		}
	}
}

// A ranges table's CIDR rows never carry contributors, so their
// duplicate-cidr message must stay exactly what it was before this package:
// no identities, because there are none to name.
func TestRangeDuplicateCIDRMessageNamesNoContributors(t *testing.T) {
	table := Table{Kind: KindRanges, Ranges: []RangeRow{
		{SourceRow: 1, CIDR: "10.20.9.0/24"},
		{SourceRow: 2, CIDR: "10.20.9.0/24"},
	}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true})

	dups := findRule(report.Findings, RuleDuplicateCIDR)
	if len(dups) != 1 {
		t.Fatalf("findings = %+v, want exactly one duplicate-cidr warning", report.Findings)
	}
	if strings.Contains(dups[0].Message, "naming contributors") {
		t.Errorf("message %q must not claim contributors for a ranges table", dups[0].Message)
	}
}

// Package T2 (docs/WORK_PLAN.md, "test hygiene"): M9b2 left undecided what
// happens when the same VPC is named in two input files and neither row
// carries an observation time. ADR 0016
// (docs/decisions/0016-SOURCE_AWARE_REFRESH_AND_REMOVAL_OF_IMPORTED_OCCUPANCY.md, "What a
// re-import refreshes, and what it only reports") settles it:
// "`contributor-stale-source` -- an entry has a null `observed_at`." An
// entry is built one per input row -- ContributorsForRows (contributors.go)
// never collapses rows by identity, unlike the write-only "contributor-new"
// comparison above, which dedupes by identity because it is reporting a SET
// difference. Stale-source is not a set comparison: it is "is THIS row's
// provenance known", asked of every row that would contribute an entry, so
// the same VPC observed twice with no timestamp either time is two rows
// whose provenance is each individually unusable as removal evidence --
// both are reported, not collapsed into one. contributorFindings (plan.go)
// already raises the finding inside its per-entry loop over
// ContributorsForRows with no identity-keyed "seen" map (unlike
// contributor-new's own dedup), so this is a pinning test for existing,
// correct behaviour, not a new code path.
func TestContributorStaleSourceIsPerRowNotPerIdentity(t *testing.T) {
	// Two different files naming the very same VPC (identical account, region
	// and resource id -- so identityOf(first) == identityOf(second)) at the
	// same CIDR, neither ever having carried an observed_at column.
	first := oldFormatRow("10.20.10.0/24", "000000000001", "vpc-a", 1)
	first.SourceFile = "legacy-a.csv"
	second := oldFormatRow("10.20.10.0/24", "000000000001", "vpc-a", 1)
	second.SourceFile = "legacy-b.csv"
	if identityOf(first) != identityOf(second) {
		t.Fatalf("fixture sanity: %q != %q, want the same VPC identity from both files",
			identityOf(first), identityOf(second))
	}
	existing := domain.Network{ID: "1", CIDR: "10.20.10.0/24", Contributors: []domain.Contributor{
		// Already recorded under this identity, exactly as
		// TestContributorStaleSourceForANullObservedAt seeds it, so neither row
		// can also raise contributor-new -- this test isolates stale-source.
		{Identity: identityOf(first), AccountID: first.AccountID, ResourceID: first.ResourceID, Region: first.Region},
	}}
	table := Table{Kind: KindNetworks, Networks: []NetworkRow{first, second}}
	report := planWithSnap(table, domain.InventorySnapshot{Complete: true, Networks: []domain.Network{existing}})

	stale := findRule(report.Findings, RuleContributorStaleSource)
	if len(stale) != 2 {
		t.Fatalf("findings = %+v, want exactly two contributor-stale-source infos: "+
			"ADR 0016 raises the finding per row (each row's own provenance), not "+
			"once per identity, even though both rows share one identity", report.Findings)
	}
	for _, f := range stale {
		if f.Level != LevelInfo {
			t.Errorf("finding %+v: Level = %v, want LevelInfo", f, f.Level)
		}
		if !strings.Contains(f.Message, identityOf(first)) {
			t.Errorf("message %q does not name %s", f.Message, identityOf(first))
		}
		if len(f.Rows) != 1 || f.Rows[0] != 1 {
			// first and second both carry SourceRow 1 (each file numbers its
			// own rows from one) but different SourceFile -- confirmed
			// distinct findings above by count; each must still trace back to
			// its own row (1) rather than pointing nowhere or colliding.
			t.Errorf("finding %+v: Rows = %v, want exactly [1] (each file's own row 1)", f, f.Rows)
		}
	}
	if len(findRule(report.Findings, RuleContributorNew)) != 0 {
		t.Errorf("findings = %+v, want no contributor-new: this identity is already a recorded contributor", report.Findings)
	}
}
