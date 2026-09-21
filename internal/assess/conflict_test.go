package assess

import (
	"testing"
)

func mustAssess(t *testing.T, in Input, opts Options) Report {
	t.Helper()
	report, err := Assess(in, opts)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	return report
}

func findConflict(t *testing.T, report Report, kind Kind, idA, idB string) *Conflict {
	t.Helper()
	for i := range report.Conflicts {
		c := &report.Conflicts[i]
		if c.Kind != kind {
			continue
		}
		if len(c.Sides) != 2 {
			t.Fatalf("conflict %s has %d sides, want 2", c.ID, len(c.Sides))
		}
		got := map[string]bool{c.Sides[0].VPCID: true, c.Sides[1].VPCID: true}
		if got[idA] && got[idB] {
			return c
		}
	}
	return nil
}

// --- equal-cidr, positive and negative ---

func TestEqualCIDRInTwoVPCsIsAConflict(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2),
	}}
	report := mustAssess(t, in, Options{})
	c := findConflict(t, report, KindEqualCIDR, "vpc-a", "vpc-b")
	if c == nil {
		t.Fatalf("no equal-cidr conflict found; conflicts=%+v", report.Conflicts)
	}
	if c.Intersection != "10.0.0.0/16" {
		t.Errorf("Intersection = %q, want 10.0.0.0/16", c.Intersection)
	}
}

func TestSameCIDRTwiceInOneVPCIsNotAConflict(t *testing.T) {
	assoc1, assoc2 := "assoc-1", "assoc-2"
	in := Input{Records: []ResourceRecord{
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-a", CIDR: "10.0.0.0/16", AssociationID: &assoc1, SourceFile: "networks.csv", SourceRow: 1},
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-a", CIDR: "10.1.0.0/16", AssociationID: &assoc2, SourceFile: "networks.csv", SourceRow: 2},
	}}
	report := mustAssess(t, in, Options{})
	if len(report.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none: two associations of the same VPC never conflict with each other", report.Conflicts)
	}
}

// --- contains, positive and negative ---

func TestContainsCrossVPCIsAConflict(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.1.0/24", "networks.csv", 2),
	}}
	report := mustAssess(t, in, Options{})
	c := findConflict(t, report, KindContains, "vpc-a", "vpc-b")
	if c == nil {
		t.Fatalf("no contains conflict found; conflicts=%+v", report.Conflicts)
	}
	if c.Intersection != "10.0.1.0/24" {
		t.Errorf("Intersection = %q, want the inner CIDR 10.0.1.0/24", c.Intersection)
	}
	var outerSide, innerSide *Side
	for i := range c.Sides {
		switch c.Sides[i].Position {
		case PositionOuter:
			outerSide = &c.Sides[i]
		case PositionInner:
			innerSide = &c.Sides[i]
		}
	}
	if outerSide == nil || innerSide == nil {
		t.Fatalf("sides missing outer/inner role: %+v", c.Sides)
	}
	if outerSide.VPCID != "vpc-a" || innerSide.VPCID != "vpc-b" {
		t.Errorf("outer=%s inner=%s, want outer=vpc-a inner=vpc-b", outerSide.VPCID, innerSide.VPCID)
	}
}

func TestAdjacentBlocksDoNotConflict(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/17", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.128.0/17", "networks.csv", 2),
	}}
	report := mustAssess(t, in, Options{})
	if len(report.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none: adjacent /17 blocks share no address", report.Conflicts)
	}
}

// --- subnet inside its own VPC: never a conflict ---

func TestSubnetInsideItsOwnVPCIsNotAConflict(t *testing.T) {
	assoc := "assoc-1"
	obsAt := "2026-09-20T00:00:00Z"
	in := Input{Records: []ResourceRecord{
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-a", CIDR: "10.0.0.0/16", AssociationID: &assoc, ObservedAt: &obsAt, SourceFile: "networks.csv", SourceRow: 1},
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeSubnet, ResourceID: "subnet-a1", CIDR: "10.0.1.0/24", ParentID: "vpc-a", SourceFile: "networks.csv", SourceRow: 2},
	}}
	report := mustAssess(t, in, Options{})
	if len(report.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none: subnets are never compared", report.Conflicts)
	}
	for _, n := range report.Notes {
		if n.Kind == NoteSubnetParentMissing || n.Kind == NoteSubnetOutsideParent {
			t.Errorf("unexpected note %+v for a subnet correctly inside its own VPC", n)
		}
	}
}

func TestSubnetWithNoParentProducesCoverageNote(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeSubnet, ResourceID: "subnet-a1", CIDR: "10.0.1.0/24", ParentID: "vpc-missing", SourceFile: "networks.csv", SourceRow: 1},
	}}
	report := mustAssess(t, in, Options{})
	found := false
	for _, n := range report.Notes {
		if n.Kind == NoteSubnetParentMissing {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes = %+v, want a subnet-parent-missing note", report.Notes)
	}
}

func TestSubnetOutsideParentAssociationsProducesDataErrorNote(t *testing.T) {
	assoc := "assoc-1"
	in := Input{Records: []ResourceRecord{
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-a", CIDR: "10.0.0.0/24", AssociationID: &assoc, SourceFile: "networks.csv", SourceRow: 1},
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeSubnet, ResourceID: "subnet-a1", CIDR: "10.9.9.0/28", ParentID: "vpc-a", SourceFile: "networks.csv", SourceRow: 2},
	}}
	report := mustAssess(t, in, Options{})
	found := false
	for _, n := range report.Notes {
		if n.Kind == NoteSubnetOutsideParent {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes = %+v, want a subnet-outside-parent note", report.Notes)
	}
}

// --- secondary associations ---

func TestSecondaryAssociationLabelledOnItsOwnSide(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-a", CIDR: "10.0.0.0/16", Primary: TriTrue, SourceFile: "networks.csv", SourceRow: 1},
		{AccountID: "222222222222", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-b", CIDR: "10.0.0.0/16", Primary: TriFalse, SourceFile: "networks.csv", SourceRow: 2},
	}}
	report := mustAssess(t, in, Options{})
	c := findConflict(t, report, KindEqualCIDR, "vpc-a", "vpc-b")
	if c == nil {
		t.Fatalf("no conflict found")
	}
	for _, s := range c.Sides {
		switch s.VPCID {
		case "vpc-a":
			if s.Primary != TriTrue {
				t.Errorf("vpc-a side Primary = %q, want true", s.Primary)
			}
		case "vpc-b":
			if s.Primary != TriFalse {
				t.Errorf("vpc-b side Primary = %q, want false", s.Primary)
			}
		}
	}
}

// --- two VPCs in one account and region still conflict ---

func TestTwoVPCsInOneAccountStillConflict(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
		vpcRec("111111111111", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2),
	}}
	report := mustAssess(t, in, Options{})
	c := findConflict(t, report, KindEqualCIDR, "vpc-a", "vpc-b")
	if c == nil {
		t.Fatalf("conflicts = %+v, want a conflict between two VPCs of the same account", report.Conflicts)
	}
}

// --- duplicate observations ---

func TestDuplicateObservationExcludedAndCounted(t *testing.T) {
	assoc := "assoc-1"
	in := Input{Records: []ResourceRecord{
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-a", CIDR: "10.0.0.0/16", AssociationID: &assoc, SourceFile: "networks.csv", SourceRow: 1},
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-a", CIDR: "10.0.0.0/16", AssociationID: &assoc, SourceFile: "networks.csv", SourceRow: 2},
		{AccountID: "111111111111", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-a", CIDR: "10.0.0.0/16", AssociationID: &assoc, SourceFile: "networks.csv", SourceRow: 3},
	}}
	report := mustAssess(t, in, Options{})
	if len(report.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none: one VPC's own duplicate rows never conflict with each other", report.Conflicts)
	}
	if report.Summary.DuplicateObservations != 2 {
		t.Errorf("DuplicateObservations = %d, want 2 (three rows collapse to one)", report.Summary.DuplicateObservations)
	}
}

func TestDuplicateObservationCanonicalRecordIsLowestFileThenRow(t *testing.T) {
	assoc := "assoc-1"
	dup := ResourceRecord{AccountID: "111111111111", Region: "us-east-1", Type: TypeVPC, ResourceID: "vpc-a", CIDR: "10.0.0.0/16", AssociationID: &assoc}
	r1 := dup
	r1.SourceFile, r1.SourceRow = "b.csv", 1
	r2 := dup
	r2.SourceFile, r2.SourceRow = "a.csv", 5
	survivors, dupCount := dedupeAssociations([]ResourceRecord{r1, r2})
	if dupCount != 1 {
		t.Fatalf("dupCount = %d, want 1", dupCount)
	}
	if len(survivors) != 1 || survivors[0].SourceFile != "a.csv" || survivors[0].SourceRow != 5 {
		t.Fatalf("survivor = %+v, want a.csv row 5 (lowest file name)", survivors)
	}
}

// --- fixed ranges ---

func TestFixedRangeReportedSeparatelyAndNeverConfirmed(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "192.0.2.0/24", "networks.csv", 1),
	}}
	matrix := Matrix{Version: 1,
		Groups:          []Group{{ID: "g1", Members: []string{"111111111111"}}},
		MustCommunicate: [][2]string{{"g1", "g1"}},
	}
	opts := Options{
		Fixed:  []FixedRange{{CIDR: "192.0.2.0/24", Description: "on-prem DC1", Owner: "netops", SourceFile: "fixed.json", SourceRow: 1}},
		Matrix: &matrix,
	}
	report := mustAssess(t, in, opts)
	var fixedConflicts int
	for _, c := range report.Conflicts {
		if !c.Fixed {
			continue
		}
		fixedConflicts++
		if c.Impact == ImpactConfirmed {
			t.Errorf("fixed conflict %s has impact confirmed, want never confirmed", c.ID)
		}
	}
	if fixedConflicts != 1 {
		t.Fatalf("fixed conflicts = %d, want 1", fixedConflicts)
	}
	if report.Summary.TotalRelationships != 0 {
		t.Errorf("TotalRelationships = %d, want 0 (fixed-involving relationships are counted separately)", report.Summary.TotalRelationships)
	}
	if report.Summary.FixedRelationships != 1 {
		t.Errorf("FixedRelationships = %d, want 1", report.Summary.FixedRelationships)
	}
}
