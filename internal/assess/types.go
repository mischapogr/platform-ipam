package assess

import "fmt"

// Tri is a three-valued flag: ADR 0014 forbids ever guessing a VPC CIDR
// association's primary/secondary status from its position in a file or
// from its prefix length, so "we do not know" is a value, not an omission.
type Tri string

const (
	TriTrue    Tri = "true"
	TriFalse   Tri = "false"
	TriUnknown Tri = "unknown"
)

// ResourceType distinguishes a VPC CIDR association from a subnet. Only
// TypeVPC records are ever compared for a conflict; a TypeSubnet record is
// read for two checks only (see notes.go) and attributes a VPC and nothing
// more, exactly as the ADR's "The relationships, exactly" section says.
type ResourceType string

const (
	TypeVPC    ResourceType = "vpc"
	TypeSubnet ResourceType = "subnet"
)

// ResourceRecord is one row of the organization inventory's networks table:
// the unit of analysis the ADR names is one observed VPC CIDR association
// (account, region, VPC id, CIDR, association id), plus the subnet rows read
// for attribution. A caller mapping internal/onboard.NetworkRow (extended by
// package M1b1 with AssociationID, ObservedAt and SourceFile) onto this type
// does so field by field; this package defines its own type rather than
// importing onboard's so the two packages can be built and reviewed
// independently (ADR 0014, M1b1 and M1b2 "are independent and run in
// parallel").
//
// AssociationID and ObservedAt are pointers: a nil pointer is the "this
// column was never recorded" null the ADR requires to be explicit, distinct
// from an empty string, which would be indistinguishable from a value a
// spreadsheet merely left blank. AssociationID is nil by construction for
// every TypeSubnet record ("empty for subnet rows" is the collector's own
// stated behaviour, not a degradation).
type ResourceRecord struct {
	AccountID     string
	AccountName   string
	Region        string
	Type          ResourceType
	ResourceID    string // VPC id for a TypeVPC row; subnet id for a TypeSubnet row
	CIDR          string
	AssociationID *string
	// Primary is the row's own "true"/"false"/"unknown" reading. The empty
	// string is treated identically to TriUnknown by every function in this
	// package (see (ResourceRecord).primary); a caller may leave it unset
	// rather than having to spell out TriUnknown for every subnet row, where
	// the field is meaningless.
	Primary    Tri
	ParentID   string // subnet's VPC id; empty for a TypeVPC row
	AZID       string
	Name       string // the AWS "Name" tag, carried as evidence only -- never used to infer ownership (ADR 0014, "Ownership")
	State      string
	ObservedAt *string // RFC 3339 UTC instant; nil = not recorded
	SourceFile string
	SourceRow  int
}

// primary normalizes the empty string to TriUnknown.
func (r ResourceRecord) primary() Tri {
	if r.Primary == "" {
		return TriUnknown
	}
	return r.Primary
}

// vpcKey is the "different VPCs" identity ADR 0014 defines: a different
// account, region and VPC id triple. It is deliberately not exported: only
// this package's own comparisons use it, and a caller has no reason to
// depend on its internal separator.
func (r ResourceRecord) vpcKey() string {
	vpc := r.ResourceID
	if r.Type == TypeSubnet {
		vpc = r.ParentID
	}
	return r.AccountID + "\x1f" + r.Region + "\x1f" + vpc
}

// Identity returns the resource identity string ADR 0014 defines:
// "aws:<account>:<region>:<vpc>:<cidr>" with ":<association>" appended when
// the association id is known. It is meaningful for a TypeVPC record; a
// TypeSubnet record is never compared and never appears in a Conflict, so
// callers should not rely on this method for subnet rows.
func (r ResourceRecord) Identity() string {
	id := fmt.Sprintf("aws:%s:%s:%s:%s", r.AccountID, r.Region, r.ResourceID, r.CIDR)
	if r.AssociationID != nil && *r.AssociationID != "" {
		id += ":" + *r.AssociationID
	}
	return id
}

// AccountRecord is one row of accounts.json: the collector's raw
// `aws organizations list-accounts` response, as ADR 0014 describes it.
// Coverage's expected-account set is every AccountRecord whose Status is
// "ACTIVE", mirroring the collector's own scan loop.
type AccountRecord struct {
	AccountID string
	Name      string
	Status    string
}

// FailureStage names the four stages scan_region/scan_account can fail at
// (ADR 0014, "Context"): assume-role and identity and describe-regions lose
// a whole account (Region is empty); describe loses one region of one
// account.
type FailureStage string

const (
	StageAssumeRole      FailureStage = "assume-role"
	StageIdentity        FailureStage = "identity"
	StageDescribeRegions FailureStage = "describe-regions"
	StageDescribe        FailureStage = "describe"
)

// FailureRow is one row of failures.csv.
type FailureRow struct {
	AccountID   string
	AccountName string
	Region      string // empty means the whole account was lost
	Stage       FailureStage
	Error       string
	SourceFile  string
	SourceRow   int
}

// AttemptOutcome is run.json's per account-region outcome: "attempted and
// succeeded, failed at which stage, or not attempted" (ADR 0014, "The
// input").
type AttemptOutcome string

const (
	AttemptSucceeded AttemptOutcome = "succeeded"
	AttemptFailed    AttemptOutcome = "failed"
	// AttemptPartial is the collector's word for a region whose VPC call
	// succeeded and whose subnet call failed: its rows stay, and the region is
	// not complete.
	AttemptPartial      AttemptOutcome = "partial"
	AttemptNotAttempted AttemptOutcome = "not_attempted"
)

// RunAttempt is one account-region entry of run.json.
type RunAttempt struct {
	AccountID string
	Region    string
	Outcome   AttemptOutcome
	Stage     string // the stage that failed; set only when Outcome == AttemptFailed
	// RowCount is run.json's own row_count for a succeeded or partial
	// attempt: how many networks.csv rows the collector itself believed it
	// wrote for this account and region (package M1c,
	// docs/WORK_PLAN.md: "the one known hole in ADR 0014's coverage
	// guarantee"). Nil means either the attempt is not succeeded/partial
	// (row_count is not meaningful for failed or not_attempted) or the
	// run.json that produced this attempt predates row_count entirely
	// (script_version 1) -- the two are indistinguishable from this field
	// alone, which is why computeInputLimits inspects Outcome as well before
	// naming LimitRowCountMissing. See coverage.go's rowCountMismatches,
	// which is the only reader of this field.
	RowCount *int
}

// RunRecord is run.json: the fourth file ADR 0014 adds to the collector's
// output, the only way "read and empty" can be distinguished from "not
// read". A nil *RunRecord on Input means run.json was missing, which the
// coverage model turns into coverage.complete == false unconditionally.
type RunRecord struct {
	StartedAt             string
	FinishedAt            string
	RoleName              string
	ManagementCredentials bool
	ConfiguredRegions     []string
	ScriptVersion         string
	Attempts              []RunAttempt
	SourceFile            string
}

// Input is everything Assess reads: the organization inventory's original
// per-resource records, the account list, the failure rows and the run
// record. Assess performs no I/O to obtain any of it.
type Input struct {
	Records  []ResourceRecord
	Accounts []AccountRecord
	Failures []FailureRow
	// Run is nil when run.json was not supplied; see RunRecord.
	Run *RunRecord
}

// MissingFieldError is returned by Validate (and by Assess, which calls it
// first) when a ResourceRecord is missing a field ADR 0014 says makes the
// report impossible to attribute: cidr, account_id, region, type or
// resource_id. Package M1b3's command turns this into exit code 4.
type MissingFieldError struct {
	SourceFile string
	SourceRow  int
	Field      string
}

func (e *MissingFieldError) Error() string {
	return fmt.Sprintf("%s:%d: missing required field %q; a conflict cannot be attributed to a resource without it", e.SourceFile, e.SourceRow, e.Field)
}

// Validate reports the first ResourceRecord missing a field that ADR 0014
// says refuses the whole report -- cidr, account_id, region, type or
// resource_id -- as a *MissingFieldError, in input order (a permutation of
// the input therefore may report a different first offender, but Assess
// never produces a report at all when Validate fails, so this does not
// affect determinism of any Report that is actually produced). Every other
// missing value degrades instead of refusing; see Report.InputLimits.
func Validate(in Input) error {
	for _, r := range in.Records {
		for _, f := range []struct {
			name string
			val  string
		}{
			{"cidr", r.CIDR},
			{"account_id", r.AccountID},
			{"region", r.Region},
			{"type", string(r.Type)},
			{"resource_id", r.ResourceID},
		} {
			if f.val == "" {
				return &MissingFieldError{SourceFile: r.SourceFile, SourceRow: r.SourceRow, Field: f.name}
			}
		}
	}
	return nil
}
