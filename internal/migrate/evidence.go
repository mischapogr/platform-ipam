package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/mischapogr/platform-ipam/internal/assess"
)

// Allocation state strings, mirrored from internal/domain's own constants
// (Reserved/Active/Quarantined/Released) rather than imported: this package
// may import internal/assess and the standard library only (derive.go's
// package doc), and internal/domain is reachable only through
// internal/service and internal/storage, exactly the packages ADR 0015
// requires this one to stay clear of. The four literal strings are the
// wire values internal/domain.Allocation.State always carries; a caller
// (package M3b3) is responsible for handing this package the same strings
// the ledger's own read returned, unchanged.
const (
	StateReserved    = "RESERVED"
	StateActive      = "ACTIVE"
	StateQuarantined = "QUARANTINED"
	StateReleased    = "RELEASED"
)

// DerivedEvidenceAllocation is this package's own minimal reading of one
// allocation row an authenticated `GET /v1/allocations` read returned (ADR
// 0015, "Tenancy, authentication and the allocation evidence"). Every field
// here is one this package's target-comparison actually uses: TenantID and
// AllocationKey select the row; Scope, Environment, Region, AccountID,
// PrefixLength and ParentAllocationKey are exactly Target's own
// comparison set; State and VerifiedAt drive reserved/active/retired.
// AddressFamily and AvailabilityZoneID (part of internal/service's own
// requestHash immutable set, ADR 0010) are deliberately absent because the
// plan's target schema never carries them either -- see Target's
// doc comment.
type DerivedEvidenceAllocation struct {
	TenantID            string
	AllocationKey       string
	Scope               string
	Environment         string
	Region              string
	AccountID           string
	PrefixLength        int
	ParentAllocationKey string
	// State is one of the four StateXxx constants above, verbatim from the
	// ledger read.
	State string
	// VerifiedAt is non-nil and non-empty exactly when the allocation's
	// binding carries a verified_at (internal/domain.Binding.VerifiedAt !=
	// nil): "the platform's own verification, not a claim" (ADR 0015).
	VerifiedAt *string
}

// DerivedEvidenceFile is one allocation evidence export (ADR 0015, "Tenancy,
// authentication and the allocation evidence"): "a JSON document carrying
// the allocations that read returned, the instant of the read, and ... the
// scope of the principal that produced it: the literal operator, or the
// tenant id." Path and Content are InputFile's own shape, inlined here
// rather than reusing assess.InputFile because a caller building this value
// also needs to decode Content into Allocations first, which
// assess.InputFile's own two fields cannot carry alongside the decoded
// result without a second parallel slice -- see the report to the lead.
type DerivedEvidenceFile struct {
	// Scope is the literal "operator", or a tenant id, exactly as ADR 0015
	// requires: "the scope rule is mechanical." An empty Scope is package
	// M3b3's job to reject with a decode error before this package ever
	// sees it; if one arrives here anyway (a caller that skipped that
	// check), it is treated as ADR 0015 requires -- "an export whose scope
	// field is absent is treated as covering nothing" -- via
	// visibleForTenant and evidenceIndex.complete below, never as covering
	// every tenant and never as a refusal this package raises itself.
	Scope string
	// ReadAt is the instant the export was produced, verbatim from the
	// file; this package never calls time.Now and never invents one.
	ReadAt      string
	Allocations []DerivedEvidenceAllocation
	// Path and Content are the file's own path (as the caller named it) and
	// exact bytes, for Report.Evidence.Digests -- the same purpose
	// assess.InputFile serves for assess.Report.Inputs.
	Path    string
	Content []byte
}

const scopeOperator = "operator"

// evidenceIndex is the package-private structure buildEvidenceIndex builds
// once per Derive call: a lookup from (tenant id, allocation key) to the
// allocation evidence for it, the union of every file's scope, and the
// degradations InputLimits names.
type evidenceIndex struct {
	files           []DerivedEvidenceFile
	scopes          map[string]bool // non-empty scopes only
	hasOperator     bool
	anyScopeMissing bool
	byKey           map[[2]string]DerivedEvidenceAllocation
}

// buildEvidenceIndex sorts files by Path (the same determinism rule
// assess.buildDigests uses for InputDigest) before folding them in, so the
// index -- and therefore every derived fact -- is identical regardless of
// the order files arrived in Derive's evidence slice. Where two files name
// the same (tenant, key) with different content, the file that sorts LAST
// by Path wins: the record does not discuss this case, and this package
// picks a deterministic rule over the ambiguity rather than refusing --
// noted to the lead.
func buildEvidenceIndex(files []DerivedEvidenceFile) *evidenceIndex {
	sorted := make([]DerivedEvidenceFile, len(files))
	copy(sorted, files)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	idx := &evidenceIndex{
		files:  sorted,
		scopes: map[string]bool{},
		byKey:  map[[2]string]DerivedEvidenceAllocation{},
	}
	for _, f := range sorted {
		if f.Scope == "" {
			idx.anyScopeMissing = true
		} else {
			idx.scopes[f.Scope] = true
			if f.Scope == scopeOperator {
				idx.hasOperator = true
			}
		}
		for _, a := range f.Allocations {
			idx.byKey[[2]string{a.TenantID, a.AllocationKey}] = a
		}
	}
	return idx
}

// visibleForTenant implements ADR 0015's scope rule: "An operator export
// covers every tenant ... A tenant export covers exactly that tenant, and
// every move whose target names another tenant is unknown -- never none.
// An export whose scope field is absent is treated as covering nothing."
func (idx *evidenceIndex) visibleForTenant(tenantID string) bool {
	if idx.hasOperator {
		return true
	}
	return idx.scopes[tenantID]
}

func (idx *evidenceIndex) lookup(tenantID, allocationKey string) (DerivedEvidenceAllocation, bool) {
	a, ok := idx.byKey[[2]string{tenantID, allocationKey}]
	return a, ok
}

// complete implements ADR 0015's evidence.complete rule verbatim: "true if
// and only if the embedded assessment's coverage.complete is true, at least
// one allocation evidence file was supplied, and the union of those files'
// scopes covers every tenant the plan's targets name."
func (idx *evidenceIndex) complete(assessmentCoverageComplete bool, targetTenants []string) bool {
	if !assessmentCoverageComplete {
		return false
	}
	if len(idx.files) == 0 {
		return false
	}
	for _, t := range targetTenants {
		if !idx.visibleForTenant(t) {
			return false
		}
	}
	return true
}

func (idx *evidenceIndex) sortedScopes() []string {
	out := make([]string, 0, len(idx.scopes))
	for s := range idx.scopes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (idx *evidenceIndex) sortedReadInstants() []string {
	set := map[string]bool{}
	for _, f := range idx.files {
		if f.ReadAt != "" {
			set[f.ReadAt] = true
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func (idx *evidenceIndex) sortedDigests() []assess.InputDigest {
	out := make([]assess.InputDigest, 0, len(idx.files))
	for _, f := range idx.files {
		sum := sha256.Sum256(f.Content)
		out = append(out, assess.InputDigest{Path: f.Path, SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Limit names this package adds to Report.InputLimits, analogous to
// internal/assess's own LimitXxx constants (assess.go). ADR 0015 does not
// name a fixed list for the progress command's own degradations the way ADR
// 0014 does for the assessment; this package infers the narrowest reading
// that lets a reader see, without reading every move, why evidence.complete
// might be false for a reason other than the embedded assessment's own
// coverage -- see the report to the lead.
const (
	// LimitNoEvidenceSupplied: zero allocation evidence files were given at
	// all.
	LimitNoEvidenceSupplied = "no-evidence-supplied"
	// LimitEvidenceScopeMissing: at least one supplied evidence file's scope
	// field was empty, "treated as covering nothing" (ADR 0015).
	LimitEvidenceScopeMissing = "evidence-scope-missing"
	// LimitEvidenceScopeNotCovered: the union of every supplied evidence
	// file's scope does not cover every tenant this plan's targets name.
	// Named to avoid the substring "complete" (forbiddenPhrases in
	// report_templates.go), which "incomplete" would otherwise contain.
	LimitEvidenceScopeNotCovered = "evidence-scope-not-covered"
)

// inputLimits names every degradation this package's own evidence handling
// contributes to Report.InputLimits, independent of the embedded
// assessment's own InputLimits (which Derive carries verbatim under
// Report.Assessment.InputLimits). targetTenants is the plan's own set of
// named target tenants (derive.go's targetTenantSet), used to detect
// LimitEvidenceScopeIncomplete.
func (idx *evidenceIndex) inputLimits(targetTenants []string) []string {
	var out []string
	if len(idx.files) == 0 {
		out = append(out, LimitNoEvidenceSupplied)
	}
	if idx.anyScopeMissing {
		out = append(out, LimitEvidenceScopeMissing)
	}
	if len(evidenceScopeIncompleteFor(idx, targetTenants)) > 0 {
		out = append(out, LimitEvidenceScopeNotCovered)
	}
	sort.Strings(out)
	return out
}

// evidenceScopeIncompleteFor reports which of targetTenants idx's evidence,
// as supplied, fails to cover -- used by report_templates.go to name the
// tenants an incomplete-evidence sentence should list.
func evidenceScopeIncompleteFor(idx *evidenceIndex, targetTenants []string) []string {
	var out []string
	for _, t := range targetTenants {
		if !idx.visibleForTenant(t) {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
