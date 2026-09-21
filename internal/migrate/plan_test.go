package migrate

import "testing"

// TestSubjectIdentityUnchangedAcrossEverythingButAccountRegionVPC is ADR
// 0015's own worked definition test: "A subject identity unchanged when a
// secondary association appears or disappears, and changed when the
// account or the region differs." This schema has no CIDR field on Subject
// at all (plan.go's own doc comment), so "a secondary association appears
// or disappears" cannot change a Subject value in the first place -- there
// is nothing on the type for it to touch. What this test actually shows is
// the part that IS representable: identity is a pure function of account,
// region and VPC id, and changes when, and only when, one of those three
// changes.
func TestSubjectIdentityUnchangedAcrossEverythingButAccountRegionVPC(t *testing.T) {
	a := Subject{AccountID: "000000000000", Region: "eu-central-1", VPCID: "vpc-1"}
	same := Subject{AccountID: "000000000000", Region: "eu-central-1", VPCID: "vpc-1"}
	if a.Identity() != same.Identity() {
		t.Errorf("Identity() differs for two equal subjects: %q vs %q", a.Identity(), same.Identity())
	}

	diffAccount := a
	diffAccount.AccountID = "111111111111"
	if a.Identity() == diffAccount.Identity() {
		t.Errorf("Identity() unchanged when AccountID differs")
	}

	diffRegion := a
	diffRegion.Region = "us-east-1"
	if a.Identity() == diffRegion.Identity() {
		t.Errorf("Identity() unchanged when Region differs")
	}

	diffVPC := a
	diffVPC.VPCID = "vpc-2"
	if a.Identity() == diffVPC.Identity() {
		t.Errorf("Identity() unchanged when VPCID differs")
	}
}

func TestSubjectIdentityFormat(t *testing.T) {
	s := Subject{AccountID: "000000000000", Region: "eu-central-1", VPCID: "vpc-0000000000000001"}
	want := "aws:000000000000:eu-central-1:vpc-0000000000000001"
	if got := s.Identity(); got != want {
		t.Errorf("Identity() = %q, want %q", got, want)
	}
}

// TestTargetKeyIgnoresEveryFieldButTenantAndAllocationKey documents that
// Target.key(), which validateDuplicateTargetKeys compares, is deliberately
// blind to every field but TenantID and AllocationKey: two targets that
// differ in region or prefix_length but agree on tenant and key still
// collide, because ADR 0015 defines the refusal as "two moves naming one
// tenant_id and allocation_key", not as two targets that differ.
func TestTargetKeyIgnoresEveryFieldButTenantAndAllocationKey(t *testing.T) {
	a := Target{TenantID: "t1", AllocationKey: "k1", Region: "eu-central-1"}
	b := Target{TenantID: "t1", AllocationKey: "k1", Region: "us-east-1"}
	if a.key() != b.key() {
		t.Errorf("key() differs for two targets sharing tenant and allocation key: %v vs %v", a.key(), b.key())
	}
}

func TestTargetOverspecified(t *testing.T) {
	n := 24
	cases := []struct {
		name string
		t    Target
		want bool
	}{
		{"minimal", Target{TenantID: "t1", AllocationKey: "k1"}, false},
		{"scope", Target{TenantID: "t1", AllocationKey: "k1", Scope: "vpc"}, true},
		{"environment", Target{TenantID: "t1", AllocationKey: "k1", Environment: "prod"}, true},
		{"region", Target{TenantID: "t1", AllocationKey: "k1", Region: "eu-central-1"}, true},
		{"account_id", Target{TenantID: "t1", AllocationKey: "k1", AccountID: "000000000000"}, true},
		{"prefix_length", Target{TenantID: "t1", AllocationKey: "k1", PrefixLength: &n}, true},
		{"parent_allocation_key", Target{TenantID: "t1", AllocationKey: "k1", ParentAllocationKey: "parent"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.t.overspecified(); got != tc.want {
				t.Errorf("overspecified() = %v, want %v", got, tc.want)
			}
		})
	}
}
