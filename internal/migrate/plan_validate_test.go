package migrate

import (
	"errors"
	"strings"
	"testing"
)

// --- helpers ---

func plansubj(vpc string) Subject {
	return Subject{AccountID: "000000000000", Region: "eu-central-1", VPCID: vpc}
}

func waveDoc(id, file string) Wave { return Wave{ID: id, Name: id, SourceFile: file} }

func refusalKind(t *testing.T, err error) RefusalKind {
	t.Helper()
	var se *StructuralError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *StructuralError", err)
	}
	return se.Kind
}

// --- duplicate subject ---

func TestValidateDuplicateSubjectWithinOneFile(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml"},
			{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml"},
		},
	}
	err := Validate(p)
	if err == nil {
		t.Fatal("Validate: want an error for a duplicate subject, got nil")
	}
	if k := refusalKind(t, err); k != RefusalDuplicateSubject {
		t.Errorf("Kind = %q, want %q", k, RefusalDuplicateSubject)
	}
}

func TestValidateDuplicateSubjectAcrossFiles(t *testing.T) {
	a := Plan{Waves: []Wave{waveDoc("w1", "a.yaml")}, Moves: []Move{
		{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml"},
	}}
	b := Plan{Waves: []Wave{waveDoc("w1", "b.yaml")}, Moves: []Move{
		{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "b.yaml"},
	}}
	merged := Merge(a, b)
	err := Validate(merged)
	if err == nil {
		t.Fatal("Validate: want an error for a subject duplicated across files, got nil")
	}
	se := err.(*StructuralError)
	if se.Kind != RefusalDuplicateSubject {
		t.Errorf("Kind = %q, want %q", se.Kind, RefusalDuplicateSubject)
	}
	if se.File != "b.yaml" {
		t.Errorf("File = %q, want b.yaml (the second occurrence)", se.File)
	}
}

func TestValidateDistinctSubjectsAcrossFilesAccepted(t *testing.T) {
	a := Plan{Waves: []Wave{waveDoc("w1", "a.yaml")}, Moves: []Move{
		{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml"},
	}}
	b := Plan{Waves: []Wave{waveDoc("w1", "b.yaml")}, Moves: []Move{
		{Subject: plansubj("vpc-2"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "b.yaml"},
	}}
	if err := Validate(Merge(a, b)); err != nil {
		t.Fatalf("Validate: %v, want no error for two distinct subjects across files", err)
	}
}

// --- duplicate target key ---

func TestValidateDuplicateTargetKey(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionReplace, Wave: "w1", SourceFile: "a.yaml",
				Target: &Target{TenantID: "t1", AllocationKey: "k1"}},
			{Subject: plansubj("vpc-2"), Disposition: DispositionReplace, Wave: "w1", SourceFile: "a.yaml",
				Target: &Target{TenantID: "t1", AllocationKey: "k1"}},
		},
	}
	err := Validate(p)
	if err == nil {
		t.Fatal("Validate: want an error for a reused target key, got nil")
	}
	if k := refusalKind(t, err); k != RefusalDuplicateTargetKey {
		t.Errorf("Kind = %q, want %q", k, RefusalDuplicateTargetKey)
	}
}

func TestValidateDistinctTargetKeysAccepted(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionReplace, Wave: "w1", SourceFile: "a.yaml",
				Target: &Target{TenantID: "t1", AllocationKey: "k1"}},
			{Subject: plansubj("vpc-2"), Disposition: DispositionReplace, Wave: "w1", SourceFile: "a.yaml",
				Target: &Target{TenantID: "t1", AllocationKey: "k2"}},
		},
	}
	if err := Validate(p); err != nil {
		t.Fatalf("Validate: %v, want no error for two distinct target keys", err)
	}
}

// --- undefined wave ---

func TestValidateUndefinedWave(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "does-not-exist", SourceFile: "a.yaml"},
		},
	}
	err := Validate(p)
	if err == nil {
		t.Fatal("Validate: want an error for an undefined wave, got nil")
	}
	if k := refusalKind(t, err); k != RefusalUndefinedWave {
		t.Errorf("Kind = %q, want %q", k, RefusalUndefinedWave)
	}
}

// --- undefined dependency ---

func TestValidateUndefinedDependency(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml",
				DependsOn: []Subject{plansubj("vpc-does-not-exist")}},
		},
	}
	err := Validate(p)
	if err == nil {
		t.Fatal("Validate: want an error for an undefined dependency, got nil")
	}
	if k := refusalKind(t, err); k != RefusalUndefinedDependency {
		t.Errorf("Kind = %q, want %q", k, RefusalUndefinedDependency)
	}
}

func TestValidateDefinedDependencyAccepted(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml",
				DependsOn: []Subject{plansubj("vpc-2")}},
			{Subject: plansubj("vpc-2"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml"},
		},
	}
	if err := Validate(p); err != nil {
		t.Fatalf("Validate: %v, want no error for a dependency on a defined subject", err)
	}
}

// --- dependency cycle ---

func TestValidateDependencyCycleSelfLoop(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml",
				DependsOn: []Subject{plansubj("vpc-1")}},
		},
	}
	err := Validate(p)
	if err == nil {
		t.Fatal("Validate: want an error for a self-referential depends_on, got nil")
	}
	if k := refusalKind(t, err); k != RefusalDependencyCycle {
		t.Errorf("Kind = %q, want %q", k, RefusalDependencyCycle)
	}
}

func TestValidateDependencyCycleTwoMoves(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml",
				DependsOn: []Subject{plansubj("vpc-2")}},
			{Subject: plansubj("vpc-2"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml",
				DependsOn: []Subject{plansubj("vpc-1")}},
		},
	}
	err := Validate(p)
	if err == nil {
		t.Fatal("Validate: want an error for a two-move depends_on cycle, got nil")
	}
	if k := refusalKind(t, err); k != RefusalDependencyCycle {
		t.Errorf("Kind = %q, want %q", k, RefusalDependencyCycle)
	}
}

func TestValidateAcyclicDependencyChainAccepted(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml",
				DependsOn: []Subject{plansubj("vpc-2")}},
			{Subject: plansubj("vpc-2"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml",
				DependsOn: []Subject{plansubj("vpc-3")}},
			{Subject: plansubj("vpc-3"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml"},
		},
	}
	if err := Validate(p); err != nil {
		t.Fatalf("Validate: %v, want no error for an acyclic chain", err)
	}
}

// --- replace without a target ---

func TestValidateReplaceWithoutTarget(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionReplace, Wave: "w1", SourceFile: "a.yaml"},
		},
	}
	err := Validate(p)
	if err == nil {
		t.Fatal("Validate: want an error for a replace move with no target, got nil")
	}
	if k := refusalKind(t, err); k != RefusalMissingTarget {
		t.Errorf("Kind = %q, want %q", k, RefusalMissingTarget)
	}
}

func TestValidateReplaceWithTargetAccepted(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionReplace, Wave: "w1", SourceFile: "a.yaml",
				Target: &Target{TenantID: "t1", AllocationKey: "k1"}},
		},
	}
	if err := Validate(p); err != nil {
		t.Fatalf("Validate: %v, want no error for a replace move with a target", err)
	}
}

// --- keep with an over-specified target ---

func TestValidateKeepOverspecifiedTargetPrefixLength(t *testing.T) {
	n := 16
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionKeep, Wave: "w1", SourceFile: "a.yaml",
				Target: &Target{TenantID: "t1", AllocationKey: "k1", PrefixLength: &n}},
		},
	}
	err := Validate(p)
	if err == nil {
		t.Fatal("Validate: want an error for a keep target naming a prefix_length, got nil")
	}
	if k := refusalKind(t, err); k != RefusalOverspecifiedTarget {
		t.Errorf("Kind = %q, want %q", k, RefusalOverspecifiedTarget)
	}
}

func TestValidateKeepOverspecifiedTargetEnvironment(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionKeep, Wave: "w1", SourceFile: "a.yaml",
				Target: &Target{TenantID: "t1", AllocationKey: "k1", Environment: "prod"}},
		},
	}
	err := Validate(p)
	if err == nil {
		t.Fatal("Validate: want an error for a keep target naming an environment, got nil")
	}
	if k := refusalKind(t, err); k != RefusalOverspecifiedTarget {
		t.Errorf("Kind = %q, want %q", k, RefusalOverspecifiedTarget)
	}
}

func TestValidateKeepMinimalTargetAccepted(t *testing.T) {
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionKeep, Wave: "w1", SourceFile: "a.yaml",
				Target: &Target{TenantID: "t1", AllocationKey: "k1"}},
		},
	}
	if err := Validate(p); err != nil {
		t.Fatalf("Validate: %v, want no error for a keep target naming only tenant and key", err)
	}
}

func TestValidateKeepWithNoTargetAtAllAccepted(t *testing.T) {
	// Not one of ADR 0015's listed refusals: a keep move with no target at
	// all is not structurally refused by this package. See the report to
	// the lead.
	p := Plan{
		Waves: []Wave{waveDoc("w1", "a.yaml")},
		Moves: []Move{
			{Subject: plansubj("vpc-1"), Disposition: DispositionKeep, Wave: "w1", SourceFile: "a.yaml"},
		},
	}
	if err := Validate(p); err != nil {
		t.Fatalf("Validate: %v, want no error", err)
	}
}

// --- a full, valid, decodable plan (also the synthetic migration.yaml the
// lead's brief asks for in the report) ---

const syntheticMigrationYAML = `
version: 1
plan_id: synthetic-demo
waves:
  - id: wave-1
    name: First wave
    window: "Saturday 02:00-06:00 UTC"
    owner: network-team
    approval:
      approved_by: cab-chair
      approved_at: "2026-09-01T00:00:00Z"
      approves: "wave 1 as scoped"
moves:
  - subject:
      account_id: "000000000000"
      region: eu-central-1
      vpc_id: vpc-0000000000000001
    disposition: replace
    wave: wave-1
    owner: team-a
    approval:
      approved_by: team-a-lead
      approved_at: "2026-09-02T00:00:00Z"
      approves: "renumber into tenant-a"
    blockers:
      - id: b1
        description: "waiting on DNS cutover plan"
    rollback: "revert route table entries within the 4-hour window"
    verification:
      - id: v1
        description: "ping test between app tier and database tier"
        ran_by: team-a
        ran_at: "2026-09-10T00:00:00Z"
        outcome: passed
    resolves:
      - c-0000000000000001
    notes: "primary CIDR renumber"
    target:
      tenant_id: tenant-a
      allocation_key: app-vpc
      scope: vpc
      environment: prod
      region: eu-central-1
      account_id: "111111111111"
      prefix_length: 16
  - subject:
      account_id: "000000000000"
      region: eu-central-1
      vpc_id: vpc-0000000000000002
    disposition: keep
    wave: wave-1
    owner: team-b
    target:
      tenant_id: tenant-b
      allocation_key: legacy-vpc
  - subject:
      account_id: "000000000000"
      region: eu-central-1
      vpc_id: vpc-0000000000000003
    disposition: retire
    wave: wave-1
    owner: team-c
    depends_on:
      - account_id: "000000000000"
        region: eu-central-1
        vpc_id: vpc-0000000000000001
    notes: "decommissioned once its replacement is live"
  - subject:
      account_id: "000000000000"
      region: eu-central-1
      vpc_id: vpc-0000000000000004
    disposition: undecided
    wave: wave-1
    notes: "owner has not yet chosen a disposition"
`

func TestDecodeAndValidateSyntheticMigrationYAML(t *testing.T) {
	p, err := DecodePlan(strings.NewReader(syntheticMigrationYAML), "migration.yaml")
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if len(p.Moves) != 4 {
		t.Fatalf("Moves = %d, want 4", len(p.Moves))
	}
	if err := Validate(p); err != nil {
		t.Fatalf("Validate: %v, want the synthetic plan to be clean", err)
	}
}

// Added in review: a document that does not say what its author meant is
// refused rather than derived as though it did.
func TestValidateRefusesWhatTheAuthorCannotHaveMeant(t *testing.T) {
	move := func(change func(*Move)) Plan {
		m := Move{Subject: plansubj("vpc-1"), Disposition: DispositionRetire, Wave: "w1", SourceFile: "a.yaml"}
		change(&m)
		return Plan{Waves: []Wave{waveDoc("w1", "a.yaml")}, Moves: []Move{m}}
	}
	twice := move(func(*Move) {})
	other := waveDoc("w1", "b.yaml")
	other.Owner = "somebody-else"
	twice.Waves = append(twice.Waves, other)
	cases := map[string]struct {
		plan Plan
		want RefusalKind
	}{
		"a mistyped disposition":     {move(func(m *Move) { m.Disposition = "replcae" }), RefusalUnknownDisposition},
		"no disposition at all":      {move(func(m *Move) { m.Disposition = "" }), RefusalUnknownDisposition},
		"a subject without a VPC":    {move(func(m *Move) { m.Subject.VPCID = "" }), RefusalIncompleteSubject},
		"a subject without account":  {move(func(m *Move) { m.Subject.AccountID = "" }), RefusalIncompleteSubject},
		"a wave defined differently": {twice, RefusalDuplicateWave},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(tc.plan)
			if err == nil {
				t.Fatalf("Validate accepted it")
			}
			if k := refusalKind(t, err); k != tc.want {
				t.Fatalf("Kind = %q, want %q", k, tc.want)
			}
		})
	}
	if err := Validate(move(func(*Move) {})); err != nil {
		t.Fatalf("the unchanged plan must validate: %v", err)
	}
}
