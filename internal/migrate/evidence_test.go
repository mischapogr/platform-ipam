package migrate

import "testing"

func TestEvidenceIndexVisibleForTenant(t *testing.T) {
	idx := buildEvidenceIndex([]DerivedEvidenceFile{
		{Scope: "tenant-a", Path: "a.json"},
		{Scope: "tenant-b", Path: "b.json"},
	})
	if !idx.visibleForTenant("tenant-a") || !idx.visibleForTenant("tenant-b") {
		t.Fatal("both supplied tenant scopes must be visible")
	}
	if idx.visibleForTenant("tenant-c") {
		t.Fatal("an uncovered tenant must not be visible")
	}
}

func TestEvidenceIndexOperatorScopeCoversEveryTenant(t *testing.T) {
	idx := buildEvidenceIndex([]DerivedEvidenceFile{{Scope: "operator", Path: "op.json"}})
	for _, tenant := range []string{"tenant-a", "tenant-b", "anything"} {
		if !idx.visibleForTenant(tenant) {
			t.Fatalf("operator scope must cover %s", tenant)
		}
	}
}

// TestEvidenceScopeMissingCoversNothing is ADR 0015's rule verbatim: "An
// export whose scope field is absent is treated as covering nothing."
func TestEvidenceScopeMissingCoversNothing(t *testing.T) {
	idx := buildEvidenceIndex([]DerivedEvidenceFile{{Scope: "", Path: "no-scope.json", Allocations: []DerivedEvidenceAllocation{
		{TenantID: "tenant-a", AllocationKey: "vpc-a", State: StateActive},
	}}})
	if idx.visibleForTenant("tenant-a") {
		t.Fatal("an evidence file with no scope must cover no tenant, even though it names an allocation for one")
	}
	limits := idx.inputLimits(nil)
	found := false
	for _, l := range limits {
		found = found || l == LimitEvidenceScopeMissing
	}
	if !found {
		t.Fatalf("input_limits = %v, want %s", limits, LimitEvidenceScopeMissing)
	}
}

// TestEvidenceCompleteRule is ADR 0015's rule verbatim: "true if and only
// if the embedded assessment's coverage.complete is true, at least one
// allocation evidence file was supplied, and the union of those files'
// scopes covers every tenant the plan's targets name."
func TestEvidenceCompleteRule(t *testing.T) {
	tests := []struct {
		name             string
		coverageComplete bool
		evidence         []DerivedEvidenceFile
		targetTenants    []string
		want             bool
	}{
		{"coverage incomplete alone refuses", false, []DerivedEvidenceFile{{Scope: "operator", Path: "a"}}, []string{"tenant-a"}, false},
		{"no evidence at all refuses even with complete coverage", true, nil, nil, false},
		{"no evidence with no targets still refuses (rule has no target-count exception)", true, nil, nil, false},
		{"operator scope with complete coverage and a named tenant", true, []DerivedEvidenceFile{{Scope: "operator", Path: "a"}}, []string{"tenant-a"}, true},
		{"tenant scope covering exactly the named tenant", true, []DerivedEvidenceFile{{Scope: "tenant-a", Path: "a"}}, []string{"tenant-a"}, true},
		{"tenant scope missing one of two named tenants", true, []DerivedEvidenceFile{{Scope: "tenant-a", Path: "a"}}, []string{"tenant-a", "tenant-b"}, false},
		{"union of two tenant-scoped exports covers both", true, []DerivedEvidenceFile{{Scope: "tenant-a", Path: "a"}, {Scope: "tenant-b", Path: "b"}}, []string{"tenant-a", "tenant-b"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := buildEvidenceIndex(tc.evidence)
			got := idx.complete(tc.coverageComplete, tc.targetTenants)
			if got != tc.want {
				t.Fatalf("complete() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEvidenceIndexDeterministicAcrossFileOrder is part of "determinism
// across the order of moves, files, evidence files and assessment inputs"
// (the lead's own head property list). Two evidence files disagree about
// one key; buildEvidenceIndex resolves the same one regardless of slice
// order, because it sorts by Path first.
func TestEvidenceIndexDeterministicAcrossFileOrder(t *testing.T) {
	fileA := DerivedEvidenceFile{Scope: "tenant-a", Path: "a.json", Allocations: []DerivedEvidenceAllocation{{TenantID: "tenant-a", AllocationKey: "vpc-a", State: StateReserved}}}
	fileB := DerivedEvidenceFile{Scope: "tenant-a", Path: "b.json", Allocations: []DerivedEvidenceAllocation{{TenantID: "tenant-a", AllocationKey: "vpc-a", State: StateActive, VerifiedAt: strPtr("t")}}}

	idx1 := buildEvidenceIndex([]DerivedEvidenceFile{fileA, fileB})
	idx2 := buildEvidenceIndex([]DerivedEvidenceFile{fileB, fileA})
	a1, _ := idx1.lookup("tenant-a", "vpc-a")
	a2, _ := idx2.lookup("tenant-a", "vpc-a")
	if a1.State != a2.State {
		t.Fatalf("lookup depends on input order: %q vs %q", a1.State, a2.State)
	}
	// b.json sorts after a.json, so it must be the one that won.
	if a1.State != StateActive {
		t.Fatalf("got state %q, want the last-sorted file (b.json, ACTIVE) to win", a1.State)
	}
}

func TestEvidenceIndexSortedAccessorsAreDeterministic(t *testing.T) {
	files := []DerivedEvidenceFile{
		{Scope: "tenant-b", ReadAt: "2026-09-20T12:00:00Z", Path: "b.json", Content: []byte("b")},
		{Scope: "tenant-a", ReadAt: "2026-09-20T11:00:00Z", Path: "a.json", Content: []byte("a")},
		{Scope: "operator", ReadAt: "2026-09-20T11:00:00Z", Path: "op.json", Content: []byte("op")},
	}
	reversed := []DerivedEvidenceFile{files[2], files[1], files[0]}
	idx1, idx2 := buildEvidenceIndex(files), buildEvidenceIndex(reversed)

	if got1, got2 := idx1.sortedScopes(), idx2.sortedScopes(); !equalStrings(got1, got2) {
		t.Fatalf("sortedScopes differs by input order: %v vs %v", got1, got2)
	}
	if got1, got2 := idx1.sortedReadInstants(), idx2.sortedReadInstants(); !equalStrings(got1, got2) {
		t.Fatalf("sortedReadInstants differs by input order: %v vs %v", got1, got2)
	}
	d1, d2 := idx1.sortedDigests(), idx2.sortedDigests()
	if len(d1) != len(d2) {
		t.Fatalf("digest count differs: %d vs %d", len(d1), len(d2))
	}
	for i := range d1 {
		if d1[i] != d2[i] {
			t.Fatalf("digest[%d] differs: %+v vs %+v", i, d1[i], d2[i])
		}
	}
}

func equalStrings(a, b []string) bool {
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
