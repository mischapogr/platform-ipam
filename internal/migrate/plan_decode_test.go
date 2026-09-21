package migrate

import (
	"errors"
	"strings"
	"testing"
)

const minimalPlanYAML = `
version: 1
plan_id: demo-plan
waves:
  - id: wave-1
    name: First wave
    owner: alice
moves:
  - subject:
      account_id: "000000000000"
      region: eu-central-1
      vpc_id: vpc-0000000000000001
    disposition: replace
    wave: wave-1
    owner: alice
    target:
      tenant_id: tenant-a
      allocation_key: key-a
      scope: vpc
      environment: prod
      region: eu-central-1
      account_id: "111111111111"
      prefix_length: 16
`

const minimalPlanJSON = `{
  "version": 1,
  "plan_id": "demo-plan",
  "waves": [{"id": "wave-1", "name": "First wave", "owner": "alice"}],
  "moves": [{
    "subject": {"account_id": "000000000000", "region": "eu-central-1", "vpc_id": "vpc-0000000000000001"},
    "disposition": "replace",
    "wave": "wave-1",
    "target": {"tenant_id": "tenant-a", "allocation_key": "key-a", "prefix_length": 16}
  }]
}`

func TestDecodePlanAcceptsMinimalYAML(t *testing.T) {
	p, err := DecodePlan(strings.NewReader(minimalPlanYAML), "plan.yaml")
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if p.Version != 1 || p.PlanID != "demo-plan" {
		t.Errorf("Version/PlanID = %d/%q, want 1/demo-plan", p.Version, p.PlanID)
	}
	if len(p.Waves) != 1 || p.Waves[0].ID != "wave-1" {
		t.Fatalf("Waves = %+v", p.Waves)
	}
	if len(p.Moves) != 1 {
		t.Fatalf("Moves = %+v, want 1", p.Moves)
	}
	m := p.Moves[0]
	if m.Subject.Identity() != "aws:000000000000:eu-central-1:vpc-0000000000000001" {
		t.Errorf("Subject.Identity() = %q", m.Subject.Identity())
	}
	if m.Target == nil || m.Target.TenantID != "tenant-a" || m.Target.AllocationKey != "key-a" {
		t.Fatalf("Target = %+v", m.Target)
	}
	if m.Target.PrefixLength == nil || *m.Target.PrefixLength != 16 {
		t.Fatalf("Target.PrefixLength = %v, want 16", m.Target.PrefixLength)
	}
}

func TestDecodePlanAcceptsMinimalJSON(t *testing.T) {
	p, err := DecodePlan(strings.NewReader(minimalPlanJSON), "plan.json")
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if len(p.Moves) != 1 || p.Moves[0].Disposition != DispositionReplace {
		t.Fatalf("Moves = %+v", p.Moves)
	}
}

// TestDecodePlanStampsSourceFileOnWavesAndMoves: SourceFile is set by
// DecodePlan itself, never read from the document (it has no JSON tag).
func TestDecodePlanStampsSourceFileOnWavesAndMoves(t *testing.T) {
	p, err := DecodePlan(strings.NewReader(minimalPlanYAML), "customer-plan.yaml")
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if p.Waves[0].SourceFile != "customer-plan.yaml" {
		t.Errorf("Waves[0].SourceFile = %q, want customer-plan.yaml", p.Waves[0].SourceFile)
	}
	if p.Moves[0].SourceFile != "customer-plan.yaml" {
		t.Errorf("Moves[0].SourceFile = %q, want customer-plan.yaml", p.Moves[0].SourceFile)
	}
}

// TestDecodePlanRejectsUnknownTopLevelField: the strict decode refuses a
// field this schema does not define, at the top level.
func TestDecodePlanRejectsUnknownTopLevelField(t *testing.T) {
	doc := `{"version": 1, "waves": [], "moves": [], "unexpected_field": true}`
	_, err := DecodePlan(strings.NewReader(doc), "plan.json")
	if err == nil {
		t.Fatal("DecodePlan: want an error for an unknown top-level field, got nil")
	}
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("error = %v, want a *DecodeError", err)
	}
	if de.File != "plan.json" {
		t.Errorf("DecodeError.File = %q, want plan.json", de.File)
	}
	if !strings.Contains(err.Error(), "unexpected_field") {
		t.Errorf("error = %v, want it to name the offending field", err)
	}
}

// TestDecodePlanRejectsCIDRFieldOnTarget is ADR 0015's own worked example
// ("the strict decoder refuses an unknown field ... so an operator who
// writes cidr: under a target gets an error naming the field"): the schema
// has no cidr field anywhere, so writing one under a target -- the single
// place a reviewer would most want to -- is refused exactly like any other
// unknown field, and the error names it.
func TestDecodePlanRejectsCIDRFieldOnTarget(t *testing.T) {
	doc := `{
  "version": 1,
  "waves": [{"id": "wave-1", "name": "w"}],
  "moves": [{
    "subject": {"account_id": "000000000000", "region": "eu-central-1", "vpc_id": "vpc-1"},
    "disposition": "replace",
    "wave": "wave-1",
    "target": {"tenant_id": "t", "allocation_key": "k", "cidr": "10.0.0.0/16"}
  }]
}`
	_, err := DecodePlan(strings.NewReader(doc), "plan.json")
	if err == nil {
		t.Fatal("DecodePlan: want an error for a cidr key under target, got nil")
	}
	if !strings.Contains(err.Error(), "cidr") {
		t.Errorf("error = %v, want it to name the cidr field", err)
	}
}

// TestDecodePlanRejectsUnquotedAccountID is the same "YAML-integer trap"
// internal/onboardcmd already documents and tests for the reviewed
// matrix/ownership inputs (TestAssessMatrixRejectsUnquotedLeadingZeroAccountID):
// an unquoted, all-digit account_id decodes as a YAML/JSON number, and
// AccountID is a Go string field, so encoding/json's strict decode refuses
// it by ordinary type-checking rather than by any account-id-specific code
// in this package. The fixture below reuses this package's own synthetic
// all-zero account id, which is also the "leading zero" case some YAML
// resolvers additionally read as octal (internal/onboardcmd's own
// ownershipUnquotedLeadingZeroYAML uses the identical id for the identical
// reason): whichever numeric value an unquoted "000000000000" resolves to,
// it is refused for being a JSON number in a string field at all, not for
// its particular numeric interpretation, so one fixture demonstrates both
// concerns.
func TestDecodePlanRejectsUnquotedAccountID(t *testing.T) {
	doc := `
version: 1
waves:
  - id: wave-1
    name: w
moves:
  - subject:
      account_id: 000000000000
      region: eu-central-1
      vpc_id: vpc-1
    disposition: retire
    wave: wave-1
`
	_, err := DecodePlan(strings.NewReader(doc), "plan.yaml")
	if err == nil {
		t.Fatal("DecodePlan: want an error for an unquoted account_id, got nil")
	}
}

// TestDecodePlanAcceptsQuotedAccountID is the positive half of the trap
// above: the SAME account id, quoted, must decode successfully.
func TestDecodePlanAcceptsQuotedAccountID(t *testing.T) {
	doc := `
version: 1
waves:
  - id: wave-1
    name: w
moves:
  - subject:
      account_id: "000000000000"
      region: eu-central-1
      vpc_id: vpc-1
    disposition: retire
    wave: wave-1
`
	p, err := DecodePlan(strings.NewReader(doc), "plan.yaml")
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if p.Moves[0].Subject.AccountID != "000000000000" {
		t.Errorf("AccountID = %q", p.Moves[0].Subject.AccountID)
	}
}

func TestDecodePlanRejectsMalformedYAML(t *testing.T) {
	doc := "version: [1\n"
	_, err := DecodePlan(strings.NewReader(doc), "plan.yaml")
	if err == nil {
		t.Fatal("DecodePlan: want an error for malformed YAML, got nil")
	}
}

func TestDecodePlanRejectsEmptyDocument(t *testing.T) {
	_, err := DecodePlan(strings.NewReader(""), "empty.yaml")
	if err == nil {
		t.Fatal("DecodePlan: want an error for an empty document, got nil")
	}
}
