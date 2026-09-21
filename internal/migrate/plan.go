package migrate

// This file defines the plan schema ADR 0015's "What a reviewer types"
// section describes: a Plan is one document with a version, an optional
// plan_id, a list of waves and a list of moves. Every field here is typed by
// a person, is carried into a later progress report verbatim, and is never
// treated as evidence (that derivation is M3b2's, not this package's cut of
// it).

// Plan is the decoded shape of one migration.yaml file, or of several merged
// by Merge. SourceFile (set by DecodePlan) is not itself a JSON field: it is
// how a structural refusal after Merge can name which file a duplicate or
// undefined reference came from.
type Plan struct {
	Version int    `json:"version"`
	PlanID  string `json:"plan_id,omitempty"`
	Waves   []Wave `json:"waves"`
	Moves   []Move `json:"moves"`
}

// Approval is the shape ADR 0015 gives both a Wave's and a Move's approval:
// "approved_by, approved_at and approves, the last being what was approved
// in the approver's own words." The platform cannot verify that the named
// approver had authority (ADR 0015, "What a reviewer types"); this package
// carries the value and never turns it into a decision.
type Approval struct {
	ApprovedBy string `json:"approved_by,omitempty"`
	ApprovedAt string `json:"approved_at,omitempty"`
	Approves   string `json:"approves,omitempty"`
}

// Wave has "an id, a name, an optional window (free text -- a maintenance
// window is a sentence, not a schema), an owner, and an approval" (ADR
// 0015).
type Wave struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Window   string    `json:"window,omitempty"`
	Owner    string    `json:"owner,omitempty"`
	Approval *Approval `json:"approval,omitempty"`

	// SourceFile names the file DecodePlan read this wave from. Not a JSON
	// field: DecodePlan sets it after decoding, so it is never something a
	// plan document itself can forge or omit.
	SourceFile string `json:"-"`
}

// Subject is a move's identity: "aws:<account>:<region>:<vpc>", ADR 0014's
// resource identity truncated before the CIDR (ADR 0015, "The unit is a
// move, and its identity is the VPC that moves"). The CIDR is excluded on
// purpose -- a secondary association coming or going must not change a
// move's identity -- and a VPC id is not globally unique, which is why the
// account and region stay in it.
type Subject struct {
	AccountID string `json:"account_id"`
	Region    string `json:"region"`
	VPCID     string `json:"vpc_id"`
}

// Identity returns the subject identity string ADR 0015 defines. It is the
// one thing this package exports specifically so a later derivation package
// can key its own maps and its coverage lookups by the same string this
// package already uses for the duplicate-subject and depends_on refusals
// (ADR 0015, "The unit is a move, and its identity is the VPC that moves").
func (s Subject) Identity() string {
	return "aws:" + s.AccountID + ":" + s.Region + ":" + s.VPCID
}

// Disposition is exactly the four values ADR 0015 defines, "the fourth
// being the honest one": replace, keep, retire, undecided. A fifth value for
// a secondary-range move is deliberately not defined (gap M7).
type Disposition string

const (
	DispositionReplace   Disposition = "replace"
	DispositionKeep      Disposition = "keep"
	DispositionRetire    Disposition = "retire"
	DispositionUndecided Disposition = "undecided"
)

// Target is "the immutable half of the request the owning team will
// eventually make" (ADR 0015, "What a reviewer types"): tenant_id,
// allocation_key, scope, environment, region, account_id, prefix_length, and
// parent_allocation_key for a subnet -- exactly the fields the record names,
// which are the fields internal/service's requestHash covers once
// description and labels are zeroed. A keep move's target carries only
// TenantID and AllocationKey; every other field must be left unset (Validate
// refuses an over-specified keep target).
//
// There is deliberately no CIDR field anywhere on this type, or on any type
// in this package: that omission, enforced by the strict decoder refusing
// any "cidr" key it did not expect, is the whole of "a draft never reserves
// space" expressed as a schema (ADR 0015, "The plan has nowhere to write a
// CIDR").
type Target struct {
	TenantID            string `json:"tenant_id"`
	AllocationKey       string `json:"allocation_key"`
	Scope               string `json:"scope,omitempty"`
	Environment         string `json:"environment,omitempty"`
	Region              string `json:"region,omitempty"`
	AccountID           string `json:"account_id,omitempty"`
	PrefixLength        *int   `json:"prefix_length,omitempty"`
	ParentAllocationKey string `json:"parent_allocation_key,omitempty"`
}

// key returns the (tenant_id, allocation_key) pair Validate uses to refuse
// two moves naming one target key (ADR 0015, "Failure modes": "A target key
// is reused. Two moves naming one tenant_id and allocation_key is a
// structural refusal").
func (t Target) key() [2]string { return [2]string{t.TenantID, t.AllocationKey} }

// overspecified reports whether t names anything beyond a tenant and a key
// -- the check Validate applies to a keep move's target, since "adoption
// pins the CIDR the VPC already has, so nothing else about it is a choice"
// (ADR 0015, "What a reviewer types").
func (t Target) overspecified() bool {
	return t.Scope != "" || t.Environment != "" || t.Region != "" || t.AccountID != "" ||
		t.PrefixLength != nil || t.ParentAllocationKey != ""
}

// Blocker is one entry of a move's blockers list: "{id, description}" (ADR
// 0015).
type Blocker struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// Verification is one entry of a move's verification list: "{id,
// description, ran_by, ran_at, outcome}" (ADR 0015). Verifications are
// typed, including their outcome, "because no offline tool can run a
// traffic test and this one must never look as though it had" -- this
// package never reads Outcome as anything but a string to carry forward.
type Verification struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	RanBy       string `json:"ran_by,omitempty"`
	RanAt       string `json:"ran_at,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
}

// Move is "one old VPC, what is to become of it, and -- where something is
// to replace it -- the target that will" (ADR 0015, "The unit is a move").
type Move struct {
	Subject      Subject        `json:"subject"`
	Disposition  Disposition    `json:"disposition"`
	Wave         string         `json:"wave"`
	Owner        string         `json:"owner,omitempty"`
	Approval     *Approval      `json:"approval,omitempty"`
	DependsOn    []Subject      `json:"depends_on,omitempty"`
	Blockers     []Blocker      `json:"blockers,omitempty"`
	Rollback     string         `json:"rollback,omitempty"`
	Verification []Verification `json:"verification,omitempty"`
	Resolves     []string       `json:"resolves,omitempty"`
	Notes        string         `json:"notes,omitempty"`
	Target       *Target        `json:"target,omitempty"`

	// SourceFile names the file DecodePlan read this move from; see Wave's
	// field of the same name and purpose.
	SourceFile string `json:"-"`
}
