package assess

import (
	"bytes"
	"encoding/json"
	"testing"
)

func sideFor(c *Conflict, vpcID string) *Side {
	for i := range c.Sides {
		if c.Sides[i].VPCID == vpcID {
			return &c.Sides[i]
		}
	}
	return nil
}

// TestImpactThreeValuesAgainstOneMatrixFixture exercises confirmed,
// potential and unknown from one matrix, including a group pair that must
// stay isolated and a VPC the matrix assigns twice (ambiguous).
func TestImpactThreeValuesAgainstOneMatrixFixture(t *testing.T) {
	matrix := Matrix{
		Version: 1,
		Groups: []Group{
			{ID: "prod-a", Members: []string{"111111111111"}},
			{ID: "prod-b", Members: []string{"222222222222"}},
			{ID: "sandbox", Members: []string{"333333333333"}},
			{ID: "no-decision", Members: []string{"444444444444"}},
			{ID: "ambiguous-1", Members: []string{"555555555555"}},
			{ID: "ambiguous-2", Members: []string{"555555555555"}},
		},
		MustCommunicate:  [][2]string{{"prod-a", "prod-b"}},
		MustStayIsolated: [][2]string{{"prod-a", "sandbox"}},
	}

	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-confirmed-a", "10.1.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-confirmed-b", "10.1.0.0/16", "networks.csv", 2),

		vpcRec("111111111111", "us-east-1", "vpc-isolated-a", "10.2.0.0/16", "networks.csv", 3),
		vpcRec("333333333333", "us-east-1", "vpc-isolated-b", "10.2.0.0/16", "networks.csv", 4),

		vpcRec("444444444444", "us-east-1", "vpc-undecided-a", "10.3.0.0/16", "networks.csv", 5),
		vpcRec("666666666666", "us-east-1", "vpc-undecided-b", "10.3.0.0/16", "networks.csv", 6), // 666... is unassigned: not in any group

		vpcRec("555555555555", "us-east-1", "vpc-ambiguous-a", "10.4.0.0/16", "networks.csv", 7),
		vpcRec("777777777777", "us-east-1", "vpc-ambiguous-b", "10.4.0.0/16", "networks.csv", 8),
	}}

	report := mustAssess(t, in, Options{Matrix: &matrix})

	cConfirmed := findConflict(t, report, KindEqualCIDR, "vpc-confirmed-a", "vpc-confirmed-b")
	if cConfirmed == nil || cConfirmed.Impact != ImpactConfirmed || cConfirmed.MatrixRelation != RelationMustCommunicate {
		t.Fatalf("confirmed case: got %+v", cConfirmed)
	}

	cIsolated := findConflict(t, report, KindEqualCIDR, "vpc-isolated-a", "vpc-isolated-b")
	if cIsolated == nil || cIsolated.Impact != ImpactPotential || cIsolated.MatrixRelation != RelationMustStayIsolated {
		t.Fatalf("isolated case: got %+v", cIsolated)
	}
	if cIsolated.IsolationNote == "" {
		t.Errorf("isolated conflict has no IsolationNote")
	}

	cUndecided := findConflict(t, report, KindEqualCIDR, "vpc-undecided-a", "vpc-undecided-b")
	if cUndecided == nil || cUndecided.Impact != ImpactPotential {
		t.Fatalf("one-side-assigned case: got %+v, want potential (at least one side assigned, matrix does not decide)", cUndecided)
	}

	cAmbiguous := findConflict(t, report, KindEqualCIDR, "vpc-ambiguous-a", "vpc-ambiguous-b")
	if cAmbiguous == nil || cAmbiguous.Impact != ImpactUnknown {
		t.Fatalf("ambiguous case: got %+v, want unknown", cAmbiguous)
	}
	sideAmbiguous := sideFor(cAmbiguous, "vpc-ambiguous-a")
	if sideAmbiguous == nil || !sideAmbiguous.Ambiguous {
		t.Errorf("side for vpc-ambiguous-a = %+v, want Ambiguous=true", sideAmbiguous)
	}

	foundAmbiguousNote := false
	for _, n := range report.Notes {
		if n.Kind == NoteAmbiguousGroup {
			foundAmbiguousNote = true
		}
	}
	if !foundAmbiguousNote {
		t.Errorf("notes = %+v, want an ambiguous-group-assignment note", report.Notes)
	}
}

// TestNoMatrixEveryConflictUnknown is the same input with no matrix at all:
// every conflict is still listed, and every impact is unknown.
func TestNoMatrixEveryConflictUnknown(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.1.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.1.0.0/16", "networks.csv", 2),
	}}
	report := mustAssess(t, in, Options{})
	if len(report.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", report.Conflicts)
	}
	if report.Conflicts[0].Impact != ImpactUnknown {
		t.Errorf("Impact = %q, want unknown with no matrix supplied", report.Conflicts[0].Impact)
	}
	if report.Conflicts[0].MatrixRelation != RelationNotInMatrix {
		t.Errorf("MatrixRelation = %q, want not_in_matrix", report.Conflicts[0].MatrixRelation)
	}
}

// TestDecodeMatrixJSON exercises the JSON decoder against exactly the shape
// documented on Matrix.
func TestDecodeMatrixJSON(t *testing.T) {
	doc := `{
		"version": 1,
		"groups": [{"id": "prod-a", "members": ["111111111111", "vpc-0abc123", "widgets"]}],
		"must_communicate": [["prod-a", "prod-b"]],
		"must_stay_isolated": [["prod-a", "sandbox"]],
		"shared_services": ["shared-dns"]
	}`
	m, err := DecodeMatrix(bytes.NewBufferString(doc))
	if err != nil {
		t.Fatalf("DecodeMatrix: %v", err)
	}
	if m.Version != 1 || len(m.Groups) != 1 || m.Groups[0].ID != "prod-a" {
		t.Fatalf("decoded = %+v", m)
	}
}

// TestOwnershipVPCIDWinsOverAccount exercises the ADR's precedence: an
// entry naming this exact VPC id wins over one naming only the account.
func TestOwnershipVPCIDWinsOverAccount(t *testing.T) {
	ownership := Ownership{Entries: []OwnershipEntry{
		{AccountID: "111111111111", Product: "account-default", Environment: "prod", Owner: "team-account"},
		{AccountID: "111111111111", VPCID: "vpc-specific", Product: "specific-product", Environment: "prod", Owner: "team-specific"},
	}}
	product, _, owner := ownership.resolve("111111111111", "vpc-specific")
	if product != "specific-product" || owner != "team-specific" {
		t.Errorf("resolve(vpc-specific) = product %q owner %q, want specific-product/team-specific", product, owner)
	}
	product, _, owner = ownership.resolve("111111111111", "vpc-other")
	if product != "account-default" || owner != "team-account" {
		t.Errorf("resolve(vpc-other) = product %q owner %q, want account-default/team-account", product, owner)
	}
}

// TestOwnershipUnknownIsExplicitNeverEmpty.
func TestOwnershipUnknownIsExplicitNeverEmpty(t *testing.T) {
	var ownership Ownership
	product, environment, owner := ownership.resolve("999999999999", "vpc-x")
	if product != "unknown" || environment != "unknown" || owner != "unknown" {
		t.Errorf("resolve with no entries = %q/%q/%q, want unknown/unknown/unknown", product, environment, owner)
	}
}

// TestUnknownOwnershipSidesCounted checks the summary counts sides with
// unknown ownership, matching the JSON encoding round trip too.
func TestUnknownOwnershipSidesCounted(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.1.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.1.0.0/16", "networks.csv", 2),
	}}
	report := mustAssess(t, in, Options{})
	if report.Summary.UnknownOwnershipSides != 2 {
		t.Errorf("UnknownOwnershipSides = %d, want 2 (no ownership table supplied)", report.Summary.UnknownOwnershipSides)
	}

	var buf bytes.Buffer
	if err := WriteReportJSON(&buf, report); err != nil {
		t.Fatalf("WriteReportJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	conflicts, _ := decoded["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("decoded conflicts = %v", conflicts)
	}
	first := conflicts[0].(map[string]any)
	sides, _ := first["sides"].([]any)
	for _, s := range sides {
		side := s.(map[string]any)
		if side["owner"] != "unknown" {
			t.Errorf("side owner = %v, want literal \"unknown\"", side["owner"])
		}
	}
}
