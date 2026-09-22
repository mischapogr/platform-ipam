package onboardcmd

import (
	"reflect"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/onboard"
)

// Package C10 (docs/WORK_PLAN.md), found by M9b1's review: networkRowsForCIDR
// ordered a CIDR's rows by SourceRow alone. Two input files each restart
// their own row numbering at 1 (package M1b1), so a multi-file import whose
// row numbers collide at one CIDR let the group's order -- and so
// mergeNetworkDescription's, entryOccupancy's AWS-field choice's, and
// networkContributors' order -- depend on which file happened to come first
// in table.Networks, i.e. on the order the files were named on the command
// line. Fixed: sort by SourceFile, then SourceRow, then (for a genuine tie --
// one source row producing several NetworkRows because its CIDR cell held
// several networks, or two files that happen to share both a name and a row
// number) ResourceID then AccountID, with sort.SliceStable.

func collisionRow(file string, sourceRow int, cidr, resourceID, accountID, name string) onboard.NetworkRow {
	return onboard.NetworkRow{
		SourceFile: file, SourceRow: sourceRow, CIDR: cidr,
		ResourceID: resourceID, AccountID: accountID, Region: "eu-central-1",
		Type: "vpc", Name: name, Description: "owns " + name,
	}
}

// TestNetworkRowsForCIDRSingleFileOrderUnchanged pins today's behaviour for
// the common, single-file case. It was run and shown green against the
// code as it stood BEFORE this package's sort change; it must stay green
// after, because a single file's rows all share one SourceFile value, so
// sorting by SourceFile first can never move them relative to each other --
// the group's order, and so mergeNetworkDescription's output, must be byte
// for byte what it was.
func TestNetworkRowsForCIDRSingleFileOrderUnchanged(t *testing.T) {
	rows := []onboard.NetworkRow{
		collisionRow("networks.csv", 3, "10.0.5.0/24", "vpc-ccc", "000000000003", "vpc-ccc"),
		collisionRow("networks.csv", 1, "10.0.5.0/24", "vpc-aaa", "000000000001", "vpc-aaa"),
		collisionRow("networks.csv", 2, "10.0.5.0/24", "vpc-bbb", "000000000002", "vpc-bbb"),
	}
	got := networkRowsForCIDR(rows, "10.0.5.0/24")
	want := []onboard.NetworkRow{rows[1], rows[2], rows[0]} // SourceRow 1, 2, 3
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("networkRowsForCIDR order = %+v, want %+v", got, want)
	}
	// Package M9c dropped the account/resource-id roll-up from
	// mergeNetworkDescription's output (docs/WORK_PLAN.md package M9c,
	// ADR 0016's deferred "fifth package") and appends a self-consistency
	// fingerprint, so the literal string this test originally pinned no
	// longer matches -- listed here as the golden this package's own copy-
	// back updates, not silently regenerated: the body computed below is
	// unchanged from before M9c except for the removed "accounts .../
	// resource ids ..." segment, and the fingerprint is computed the same
	// way withDescriptionFingerprint computes it, so this still pins order
	// (SourceRow 1, 2, 3, not file order) and content, just not the two
	// segments M9c intentionally removed.
	wantBody := "vpc-aaa; vpc-bbb; vpc-ccc | source row 1: owns vpc-aaa | source row 2: owns vpc-bbb | source row 3: owns vpc-ccc"
	wantDescription := wantBody + " (" + descriptionFingerprint(wantBody) + ")"
	if desc := mergeNetworkDescription(got); desc != wantDescription {
		t.Fatalf("mergeNetworkDescription = %q, want %q", desc, wantDescription)
	}
}

// TestNetworkRowsForCIDRStableAcrossFileOrder is the collision case M9b1's
// review found: two files that each restart SourceRow at 1 produce two
// NetworkRows tied on SourceRow at the same CIDR. Before this package,
// networkRowsForCIDR sorted by SourceRow alone, so with only two rows tied
// on the sort key, Go's small-slice insertion sort never swaps them -- it
// leaves them in whatever order table.Networks already had, which is simply
// the order the files were named on the command line. The fix must make
// networkRowsForCIDR's group, and mergeNetworkDescription's output built
// from it, independent of that order, and repeatable run after run.
func TestNetworkRowsForCIDRStableAcrossFileOrder(t *testing.T) {
	fromA := collisionRow("a.csv", 1, "10.0.5.0/24", "vpc-aaa", "000000000001", "vpc-a-name")
	fromB := collisionRow("b.csv", 1, "10.0.5.0/24", "vpc-bbb", "000000000002", "vpc-b-name")

	aFirst := networkRowsForCIDR([]onboard.NetworkRow{fromA, fromB}, "10.0.5.0/24")
	bFirst := networkRowsForCIDR([]onboard.NetworkRow{fromB, fromA}, "10.0.5.0/24")
	if !reflect.DeepEqual(aFirst, bFirst) {
		t.Fatalf("networkRowsForCIDR's order depends on which file came first in the table:\naFirst=%+v\nbFirst=%+v", aFirst, bFirst)
	}
	want := []onboard.NetworkRow{fromA, fromB} // a.csv sorts before b.csv
	if !reflect.DeepEqual(aFirst, want) {
		t.Fatalf("networkRowsForCIDR = %+v, want %+v (sorted by SourceFile, then SourceRow)", aFirst, want)
	}

	descAFirst := mergeNetworkDescription(aFirst)
	descBFirst := mergeNetworkDescription(bFirst)
	if descAFirst != descBFirst {
		t.Fatalf("mergeNetworkDescription depends on the files' order:\n%q\n%q", descAFirst, descBFirst)
	}

	// Across two runs with the identical input order: the result must be
	// exactly repeatable, not merely equal-by-chance once.
	again := networkRowsForCIDR([]onboard.NetworkRow{fromA, fromB}, "10.0.5.0/24")
	if !reflect.DeepEqual(aFirst, again) {
		t.Fatalf("networkRowsForCIDR is not repeatable across runs: %+v vs %+v", aFirst, again)
	}
}

// TestEntryOccupancyDeterministicAcrossFileOrder repeats the collision at
// the entryOccupancy level the package spec names explicitly: the merged
// Description, the single-account AWS fields and the Contributors order must
// all be byte-identical regardless of which of two colliding files is given
// first.
func TestEntryOccupancyDeterministicAcrossFileOrder(t *testing.T) {
	rowA := contributorRow("10.0.5.0/24", "000000000001", "vpc-aaa", 1)
	rowA.SourceFile = "a.csv"
	rowA.Name = "vpc-a-name"
	rowB := contributorRow("10.0.5.0/24", "000000000002", "vpc-bbb", 1)
	rowB.SourceFile = "b.csv"
	rowB.Name = "vpc-b-name"

	tableAFirst := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{rowA, rowB}}
	tableBFirst := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{rowB, rowA}}

	occAFirst := occupancyFor(t, tableAFirst, "batch-1")
	occBFirst := occupancyFor(t, tableBFirst, "batch-1")
	if !reflect.DeepEqual(occAFirst, occBFirst) {
		t.Fatalf("entryOccupancy depends on the files' order:\n%s\n%s", pretty(t, occAFirst), pretty(t, occBFirst))
	}
	if len(occAFirst) != 1 {
		t.Fatalf("want one write, got %d", len(occAFirst))
	}
	// Both accounts are present, so networkAWSFields must stay ambiguous
	// (empty) either way -- this pins that the fix did not accidentally
	// change which row "wins" when there is more than one with an account.
	if occAFirst[0].AWSAccountID != "" {
		t.Fatalf("AWSAccountID = %q, want empty: two rows carry an account so none should win", occAFirst[0].AWSAccountID)
	}
}

// Range rows have no source file, so two files whose row numbers collide are
// ordered by the row's own content; the order never depends on which file was
// named first.
func TestRangeRowsForCIDRStableAcrossFileOrder(t *testing.T) {
	a := onboard.RangeRow{SourceRow: 1, CIDR: "192.0.2.0/24", Description: "alpha", Owner: "team-a"}
	b := onboard.RangeRow{SourceRow: 1, CIDR: "192.0.2.0/24", Description: "beta", Owner: "team-b"}
	first := rangeCIDRRowsForCIDR([]onboard.RangeRow{a, b}, "192.0.2.0/24")
	second := rangeCIDRRowsForCIDR([]onboard.RangeRow{b, a}, "192.0.2.0/24")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("order depends on input order:\n%+v\n%+v", first, second)
	}
	if first[0].Description != "alpha" {
		t.Fatalf("expected alpha first, got %+v", first)
	}
	sa := onboard.RangeRow{SourceRow: 2, StartAddress: "192.0.2.10", EndAddress: "192.0.2.20", Description: "alpha"}
	sb := onboard.RangeRow{SourceRow: 2, StartAddress: "192.0.2.10", EndAddress: "192.0.2.20", Description: "beta"}
	if !reflect.DeepEqual(rangeRowsForSpan([]onboard.RangeRow{sa, sb}, "192.0.2.10", "192.0.2.20"), rangeRowsForSpan([]onboard.RangeRow{sb, sa}, "192.0.2.10", "192.0.2.20")) {
		t.Fatal("span order depends on input order")
	}
}
