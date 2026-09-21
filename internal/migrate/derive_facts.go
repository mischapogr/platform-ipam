package migrate

import (
	"fmt"
	"sort"

	"github.com/mischapogr/platform-ipam/internal/assess"
)

// SubjectFact is the three-value fact ADR 0015 defines for a move's
// subject: "observed, not-observed or unknown, by the rules above, with the
// coverage entry that made it unknown named beside it."
type SubjectFact string

const (
	SubjectObserved    SubjectFact = "observed"
	SubjectNotObserved SubjectFact = "not-observed"
	SubjectUnknown     SubjectFact = "unknown"
)

// TargetFact is the six-value fact ADR 0015 defines for a move's target:
// "none, reserved, active, retired, mismatched or unknown."
type TargetFact string

const (
	TargetNone       TargetFact = "none"
	TargetReserved   TargetFact = "reserved"
	TargetActive     TargetFact = "active"
	TargetRetired    TargetFact = "retired"
	TargetMismatched TargetFact = "mismatched"
	TargetUnknown    TargetFact = "unknown"
)

// ResolveStatus is the per-conflict-id status ADR 0015's Decision section
// names for an entry of a move's `resolves` list. The record's Decision
// section lists three words -- "present, absent or stale" -- but its own
// evidence paragraph, which is the record's test specification, describes
// only two reachable outcomes: "a claimed conflict id absent from the
// assessment reported `stale`, against one still present reported
// `present`." This package implements exactly those two, reading "absent"
// in the Decision section's list as the plain-English description leading
// into "stale" rather than a third value with no rule of its own -- see the
// report to the lead, which flags this as one of the record's ambiguities.
type ResolveStatus string

const (
	ResolvePresent ResolveStatus = "present"
	ResolveStale   ResolveStatus = "stale"
)

// ResolveResult is one entry of a move's `resolves` list with its derived
// status.
type ResolveResult struct {
	ConflictID string        `json:"conflict_id"`
	Status     ResolveStatus `json:"status"`
}

// subjectFact implements ADR 0015's four-answer rule for what happens when
// an identity stops appearing, collapsed to the three values Subject may
// take (the fourth answer, `unmatched`, is a separate flag -- see
// MoveResult.Unmatched).
func subjectFact(s Subject, input assess.Input, coverage assess.Coverage, knownSubjects, knownAccounts map[string]bool) (SubjectFact, string, bool) {
	if knownSubjects[s.key()] {
		return SubjectObserved, "", false
	}

	unmatched := !knownAccounts[s.AccountID]
	if unmatched {
		return SubjectUnknown, "the account id does not appear in the observed account list at all; the report cannot tell a typo from a resource outside the scope that was read", true
	}

	if incomplete, reason := coverageIncompleteFor(coverage, s.AccountID, s.Region); incomplete {
		return SubjectUnknown, reason, false
	}
	return SubjectNotObserved, "", false
}

// coverageIncompleteFor reports whether coverage cannot make a complete
// statement about account/region, and names the coverage entry responsible.
// A positive observation (a matching row was read) never needs this check --
// only the negative "not-observed" statement does (ADR 0015: "an unreadable
// account is the cheapest way for a VPC to look retired").
func coverageIncompleteFor(coverage assess.Coverage, accountID, region string) (bool, string) {
	if coverage.RunMissing {
		return true, "run.json is missing; coverage cannot be determined for any account or region"
	}
	if coverage.AccountsMissing {
		return true, "accounts.json is missing; the expected account set is unknown"
	}
	for _, f := range coverage.Failed {
		if f.AccountID == accountID && (f.Region == "" || f.Region == region) {
			return true, fmt.Sprintf("account %s region %s: failed at stage %s (failures.csv)", f.AccountID, orWhole(f.Region), f.Stage)
		}
	}
	for _, p := range coverage.Partial {
		if p.AccountID == accountID && p.Region == region {
			return true, fmt.Sprintf("account %s region %s: partial (describe failed after some rows were written)", p.AccountID, p.Region)
		}
	}
	for _, n := range coverage.NotAttempted {
		if n.AccountID == accountID && (n.Region == "" || n.Region == region) {
			return true, fmt.Sprintf("account %s region %s: not attempted", n.AccountID, orWhole(n.Region))
		}
	}
	for _, m := range coverage.RowCountShort {
		if m.AccountID == accountID && m.Region == region {
			return true, fmt.Sprintf("account %s region %s: run.json recorded %d rows, only %d present (row_count_short)", m.AccountID, m.Region, m.Recorded, m.Present)
		}
	}
	return false, ""
}

func orWhole(region string) string {
	if region == "" {
		return "(whole account)"
	}
	return region
}

// targetFieldCheck names one immutable target field ADR 0015 lists, in a
// fixed order, so a mismatch report names differing fields deterministically
// regardless of map iteration order.
type targetFieldCheck struct {
	name string
	want string
	got  string
}

// targetFact implements ADR 0015's six-value target rule. idx is the index
// built from every supplied DerivedEvidenceFile.
func targetFact(m Move, idx *evidenceIndex) (TargetFact, []string) {
	if m.Target == nil {
		// retire and undecided moves carry no target at all (ADR 0015, "What
		// a reviewer types"): there is no key to look up, so the fact is
		// `none` by construction rather than a fourth undefined value.
		return TargetNone, nil
	}
	t := *m.Target
	if !idx.visibleForTenant(t.TenantID) {
		return TargetUnknown, nil
	}
	alloc, found := idx.lookup(t.TenantID, t.AllocationKey)
	if !found {
		return TargetNone, nil
	}
	if alloc.State == StateQuarantined || alloc.State == StateReleased {
		return TargetRetired, nil
	}

	// A keep move's target names only tenant_id and allocation_key
	// (Target's own doc comment: "adoption pins the CIDR the
	// VPC already has, so nothing else about it is a choice"), so there is
	// nothing else typed to compare -- a keep move is never `mismatched`.
	var mismatches []string
	if m.Disposition != DispositionKeep {
		checks := []targetFieldCheck{
			{"scope", t.Scope, alloc.Scope},
			{"environment", t.Environment, alloc.Environment},
			{"region", t.Region, alloc.Region},
			{"account_id", t.AccountID, alloc.AccountID},
			{"prefix_length", prefixLengthText(t.PrefixLength), itoa(alloc.PrefixLength)},
			{"parent_allocation_key", t.ParentAllocationKey, alloc.ParentAllocationKey},
		}
		for _, c := range checks {
			if c.want != c.got {
				mismatches = append(mismatches, c.name)
			}
		}
	}
	if len(mismatches) > 0 {
		sort.Strings(mismatches)
		return TargetMismatched, mismatches
	}

	if alloc.State == StateActive && alloc.VerifiedAt != nil && *alloc.VerifiedAt != "" {
		return TargetActive, nil
	}
	return TargetReserved, nil
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// moveConflictResults derives the per-move Resolves list and the unclaimed
// list (ADR 0015, "What is derived"): for each id in m.Resolves, present or
// stale against the current assessment's set of conflict ids; plus every
// conflict the current assessment reports touching this subject that the
// move does not claim, listed as unclaimed.
func moveConflictResults(m Move, touching map[string][]assess.Conflict, reportConflictIDs map[string]bool) ([]ResolveResult, []string) {
	claimed := map[string]bool{}
	// Sorted, deduplicated iteration keeps Resolves deterministic even if a
	// caller's plan lists one id twice.
	ids := append([]string(nil), m.Resolves...)
	sort.Strings(ids)
	resolves := make([]ResolveResult, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		claimed[id] = true
		status := ResolveStale
		if reportConflictIDs[id] {
			status = ResolvePresent
		}
		resolves = append(resolves, ResolveResult{ConflictID: id, Status: status})
	}

	var unclaimed []string
	for _, c := range touching[m.Subject.key()] {
		if !claimed[c.ID] {
			unclaimed = append(unclaimed, c.ID)
		}
	}
	sort.Strings(unclaimed)
	return resolves, unclaimed
}

// fullyEvidenced is ADR 0015's one aggregate: "target active, subject
// not-observed, every claimed conflict absent [i.e. stale -- see
// ResolveStatus's doc comment], and no unclaimed conflict." A move that
// claims no conflicts at all vacuously satisfies "every claimed conflict
// stale" -- this package's own reading, noted to the lead.
func fullyEvidenced(subject SubjectFact, target TargetFact, resolves []ResolveResult, unclaimed []string) bool {
	if subject != SubjectNotObserved || target != TargetActive || len(unclaimed) > 0 {
		return false
	}
	for _, r := range resolves {
		if r.Status != ResolveStale {
			return false
		}
	}
	return true
}

// deriveMove computes every derived field of one MoveResult, carrying every
// typed field into it verbatim alongside.
func deriveMove(m Move, report assess.Report, input assess.Input, idx *evidenceIndex, touching map[string][]assess.Conflict, knownSubjects, knownAccounts map[string]bool, reportConflictIDs map[string]bool) MoveResult {
	sFact, sReason, unmatched := subjectFact(m.Subject, input, report.Coverage, knownSubjects, knownAccounts)
	tFact, mismatchFields := targetFact(m, idx)
	resolves, unclaimed := moveConflictResults(m, touching, reportConflictIDs)
	fe := fullyEvidenced(sFact, tFact, resolves, unclaimed)

	keepBlocked := m.Disposition == DispositionKeep && len(touching[m.Subject.key()]) > 0

	var dependsOn []string
	for _, d := range m.DependsOn {
		dependsOn = append(dependsOn, d.Identity())
	}

	return MoveResult{
		Subject:               m.Subject.Identity(),
		AccountID:             m.Subject.AccountID,
		Region:                m.Subject.Region,
		VPCID:                 m.Subject.VPCID,
		Disposition:           string(m.Disposition),
		WaveID:                m.Wave,
		Owner:                 m.Owner,
		Approval:              toApprovalResult(m.Approval),
		DependsOn:             dependsOn,
		Blockers:              toBlockerResults(m.Blockers),
		Rollback:              m.Rollback,
		Verifications:         toVerificationResults(m.Verification),
		Notes:                 m.Notes,
		SubjectFact:           sFact,
		SubjectFactReason:     sReason,
		Unmatched:             unmatched,
		TargetFact:            tFact,
		TargetMismatchFields:  mismatchFields,
		Resolves:              resolves,
		Unclaimed:             unclaimed,
		FullyEvidenced:        fe,
		KeepBlockedByConflict: keepBlocked,
	}
}

func toApprovalResult(a *Approval) *ApprovalResult {
	if a == nil {
		return nil
	}
	return &ApprovalResult{ApprovedBy: a.ApprovedBy, ApprovedAt: a.ApprovedAt, Approves: a.Approves}
}

func toBlockerResults(bs []Blocker) []BlockerResult {
	if len(bs) == 0 {
		return nil
	}
	out := make([]BlockerResult, len(bs))
	for i, b := range bs {
		out[i] = BlockerResult{ID: b.ID, Description: b.Description}
	}
	return out
}

func toVerificationResults(vs []Verification) []VerificationResult {
	if len(vs) == 0 {
		return nil
	}
	out := make([]VerificationResult, len(vs))
	for i, v := range vs {
		out[i] = VerificationResult{ID: v.ID, Description: v.Description, RanBy: v.RanBy, RanAt: v.RanAt, Outcome: v.Outcome}
	}
	return out
}

// buildWaveRollups implements ADR 0015's "a wave rolls up the same way:
// counts per fact, side by side, each with its denominator."
func buildWaveRollups(waves []Wave, moves []MoveResult) []WaveRollup {
	byWave := map[string][]MoveResult{}
	for _, m := range moves {
		byWave[m.WaveID] = append(byWave[m.WaveID], m)
	}
	out := make([]WaveRollup, 0, len(waves))
	for _, w := range waves {
		out = append(out, rollupOne(w, byWave[w.ID]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func rollupOne(w Wave, moves []MoveResult) WaveRollup {
	r := WaveRollup{ID: w.ID, Name: w.Name, Owner: w.Owner, Approval: toApprovalResult(w.Approval), MovesTotal: len(moves)}
	bySubject := map[SubjectFact]int{}
	byTarget := map[TargetFact]int{}
	for _, m := range moves {
		bySubject[m.SubjectFact]++
		byTarget[m.TargetFact]++
		if m.FullyEvidenced {
			r.FullyEvidenced++
		}
	}
	r.BySubjectFact = subjectFactCounts(bySubject)
	r.ByTargetFact = targetFactCounts(byTarget)
	return r
}

func subjectFactCounts(m map[SubjectFact]int) []FactCount {
	return []FactCount{
		{Value: string(SubjectObserved), Count: m[SubjectObserved]},
		{Value: string(SubjectNotObserved), Count: m[SubjectNotObserved]},
		{Value: string(SubjectUnknown), Count: m[SubjectUnknown]},
	}
}

func targetFactCounts(m map[TargetFact]int) []FactCount {
	return []FactCount{
		{Value: string(TargetNone), Count: m[TargetNone]},
		{Value: string(TargetReserved), Count: m[TargetReserved]},
		{Value: string(TargetActive), Count: m[TargetActive]},
		{Value: string(TargetRetired), Count: m[TargetRetired]},
		{Value: string(TargetMismatched), Count: m[TargetMismatched]},
		{Value: string(TargetUnknown), Count: m[TargetUnknown]},
	}
}

// prefixLengthText renders a target's prefix length as the evidence renders
// its own, so a plan that states none compares unequal to any allocation.
func prefixLengthText(n *int) string {
	if n == nil {
		return ""
	}
	return itoa(*n)
}
