package migrate

// This file is the allocation evidence file's own type (ADR 0015,
// "Tenancy, authentication and the allocation evidence", assigned to this
// package's M3b1 cut). It is a JSON document produced by an ordinary
// authenticated read -- the evidence export M3b4 adds to the consumer CLI --
// and handed to this package exactly as a plan file is: DecodeEvidence
// performs no I/O beyond reading r.
//
// The type here is deliberately the RAW decoded shape of that file, not a
// derivation-ready abstraction: "the field that makes the rest safe" is
// Scope, carried verbatim, and every comparison of an allocation's immutable
// fields against a plan Target's is M3b2's job, not this package's. M3b2's
// own record entry says it defines "its own input types" for the
// derivation (docs/WORK_PLAN.md, M3b2) -- a narrower shape purpose-built for
// computing the three per-move facts -- and a later command (M3b3) maps this
// package's EvidenceFile onto whatever M3b2 needs, exactly as
// internal/onboardcmd already maps internal/onboard.NetworkRow onto
// internal/assess.ResourceRecord field by field rather than sharing a type
// across the two packages (ADR 0014, M1b1/M1b2). This is a decision the
// record's text does not spell out to this level; see the report to the
// lead.

// EvidenceScope is the literal value ADR 0015 gives an operator's export:
// "the literal operator, or the tenant id." Package-level constant only for
// the one non-tenant-id value; a tenant id is otherwise any non-empty
// string and is not itself an enumerated type here, since this package does
// not carry a table of known tenants to validate against.
const EvidenceScopeOperator = "operator"

// EvidenceAllocation is one row of the allocations an evidence export's read
// returned -- the fields a target comparison needs: tenant, key, the
// immutable request fields ADR 0015's Target also carries, state, and
// whether the binding was verified. It mirrors domain.Allocation field by
// field (TenantID, AllocationKey ... State, Binding.VerifiedAt) rather than
// importing internal/domain, for the same reason ResourceRecord does not
// import internal/onboard's NetworkRow (ADR 0014): this package's import
// graph must never reach internal/domain, which every adapter package
// depends on transitively enough that ADR 0015's source-parsing test would
// rather this package define its own shape than risk widening what it
// imports later by accident.
type EvidenceAllocation struct {
	TenantID            string `json:"tenant_id"`
	AllocationKey       string `json:"allocation_key"`
	Scope               string `json:"scope,omitempty"`
	Environment         string `json:"environment,omitempty"`
	Region              string `json:"region,omitempty"`
	AccountID           string `json:"account_id,omitempty"`
	PrefixLength        *int   `json:"prefix_length,omitempty"`
	ParentAllocationKey string `json:"parent_allocation_key,omitempty"`
	// State is one of domain.Reserved/Active/Quarantined/Released
	// ("RESERVED"/"ACTIVE"/"QUARANTINED"/"RELEASED"), carried verbatim; this
	// package does not import internal/domain's constants and does not
	// validate the value against them (M3b2's derivation is the reader that
	// cares what the word means).
	State string `json:"state"`
	// BindingVerifiedAt is non-empty exactly when domain.Binding.VerifiedAt
	// was set on the read allocation -- ADR 0015's "active means the same
	// allocation is ACTIVE and its binding carries a verified_at -- the
	// platform's own verification, not a claim." Carried as the raw RFC 3339
	// string the export wrote, or empty when there is no verification; this
	// package parses no time value.
	BindingVerifiedAt string `json:"binding_verified_at,omitempty"`
}

// EvidenceFile is the decoded shape of one allocation evidence export (ADR
// 0015, "Tenancy, authentication and the allocation evidence"): "the
// allocations that read returned, the instant of the read, and ... the
// scope of the principal that produced it." ReadAt is the read instant as
// the export recorded it (an RFC 3339 string; this package parses no time
// value, exactly as it carries every other instant in this schema
// verbatim). Scope is EvidenceScopeOperator or a tenant id; DecodeEvidence
// does not refuse an empty Scope -- ADR 0015 says an export "whose scope
// field is absent is treated as covering nothing", which is a fact for a
// later derivation to act on, not a decode-time refusal, so an empty-scope
// file still decodes and simply names nothing in Scope.
type EvidenceFile struct {
	ReadAt      string               `json:"read_at"`
	Scope       string               `json:"scope"`
	Allocations []EvidenceAllocation `json:"allocations"`

	// SourceFile names the file DecodeEvidence read this from; see
	// Move.SourceFile's doc comment for why it is not a JSON field.
	SourceFile string `json:"-"`
}
