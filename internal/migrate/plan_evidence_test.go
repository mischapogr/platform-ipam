package migrate

import (
	"strings"
	"testing"
)

const minimalEvidenceJSON = `{
  "read_at": "2026-09-22T00:00:00Z",
  "scope": "tenant-a",
  "allocations": [
    {"tenant_id": "tenant-a", "allocation_key": "key-a", "scope": "vpc", "environment": "prod",
     "region": "eu-central-1", "account_id": "111111111111", "prefix_length": 16,
     "state": "ACTIVE", "binding_verified_at": "2026-09-22T00:05:00Z"}
  ]
}`

func TestDecodeEvidenceAcceptsMinimalDocument(t *testing.T) {
	ev, err := DecodeEvidence(strings.NewReader(minimalEvidenceJSON), "evidence.json")
	if err != nil {
		t.Fatalf("DecodeEvidence: %v", err)
	}
	if ev.Scope != "tenant-a" {
		t.Errorf("Scope = %q, want tenant-a", ev.Scope)
	}
	if ev.SourceFile != "evidence.json" {
		t.Errorf("SourceFile = %q, want evidence.json", ev.SourceFile)
	}
	if len(ev.Allocations) != 1 || ev.Allocations[0].AllocationKey != "key-a" {
		t.Fatalf("Allocations = %+v", ev.Allocations)
	}
}

// TestDecodeEvidenceAcceptsOperatorScope: EvidenceScopeOperator is the
// literal ADR 0015 names for an operator export ("the literal operator, or
// the tenant id").
func TestDecodeEvidenceAcceptsOperatorScope(t *testing.T) {
	doc := `{"read_at": "2026-09-22T00:00:00Z", "scope": "` + EvidenceScopeOperator + `", "allocations": []}`
	ev, err := DecodeEvidence(strings.NewReader(doc), "evidence.json")
	if err != nil {
		t.Fatalf("DecodeEvidence: %v", err)
	}
	if ev.Scope != "operator" {
		t.Errorf("Scope = %q, want operator", ev.Scope)
	}
}

// TestDecodeEvidenceAcceptsEmptyScope: ADR 0015 says an export "whose scope
// field is absent is treated as covering nothing" -- a fact for a later
// derivation, not a decode-time refusal, so this package's decoder accepts
// it.
func TestDecodeEvidenceAcceptsEmptyScope(t *testing.T) {
	doc := `{"read_at": "2026-09-22T00:00:00Z", "allocations": []}`
	ev, err := DecodeEvidence(strings.NewReader(doc), "evidence.json")
	if err != nil {
		t.Fatalf("DecodeEvidence: %v", err)
	}
	if ev.Scope != "" {
		t.Errorf("Scope = %q, want empty", ev.Scope)
	}
}

func TestDecodeEvidenceRejectsUnknownField(t *testing.T) {
	doc := `{"read_at": "x", "scope": "operator", "allocations": [], "unexpected": true}`
	_, err := DecodeEvidence(strings.NewReader(doc), "evidence.json")
	if err == nil {
		t.Fatal("DecodeEvidence: want an error for an unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected") {
		t.Errorf("error = %v, want it to name the field", err)
	}
}

// TestDecodeEvidenceRejectsCIDRField: the evidence schema, like the plan
// schema, has no cidr field anywhere -- an allocation's own CIDR is
// deliberately not part of what a plan or its evidence records structurally
// (the derivation compares immutable REQUEST fields, never the issued
// address).
func TestDecodeEvidenceRejectsCIDRField(t *testing.T) {
	doc := `{"read_at": "x", "scope": "operator", "allocations": [
		{"tenant_id": "t", "allocation_key": "k", "state": "RESERVED", "cidr": "10.0.0.0/16"}
	]}`
	_, err := DecodeEvidence(strings.NewReader(doc), "evidence.json")
	if err == nil {
		t.Fatal("DecodeEvidence: want an error for a cidr key on an allocation, got nil")
	}
	if !strings.Contains(err.Error(), "cidr") {
		t.Errorf("error = %v, want it to name the cidr field", err)
	}
}

func TestDecodeEvidenceRejectsUnquotedAccountID(t *testing.T) {
	doc := `{"read_at": "x", "scope": "operator", "allocations": [
		{"tenant_id": "t", "allocation_key": "k", "account_id": 111111111111, "state": "RESERVED"}
	]}`
	_, err := DecodeEvidence(strings.NewReader(doc), "evidence.json")
	if err == nil {
		t.Fatal("DecodeEvidence: want an error for an unquoted account_id, got nil")
	}
}

func TestDecodeEvidenceRejectsEmptyDocument(t *testing.T) {
	_, err := DecodeEvidence(strings.NewReader(""), "evidence.json")
	if err == nil {
		t.Fatal("DecodeEvidence: want an error for an empty document, got nil")
	}
}
