// Package migrate is the pure, offline derivation engine described by
// docs/decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md
// (work-plan package M3b2). Given a decoded migration plan, the assessment's
// own Report and the Input it was computed from (internal/assess), and zero
// or more allocation evidence files, it derives -- per planned move -- the
// three independent facts the record defines (subject, target, conflicts),
// the wave roll-ups, the unplanned and stale lists, the two count blocks
// that are never added together, and the one evidence.complete boolean that
// selects between exactly two summary templates.
//
// Like internal/assess (package doc.go), this package does no I/O beyond
// decoding the values its callers hand it: it never opens a file, never
// calls NetBox, AWS or the ledger, never reads an environment variable, and
// never calls time.Now -- every instant in a produced Report came out of an
// input, and the archival --stamp is the one exception, supplied by the
// caller through Options.Stamp exactly as assess.Options.Stamp works. It
// imports internal/assess and nothing else beyond the Go standard library
// from this module (see TestNonTestSourceImportsAssessAndStandardLibraryOnly
// in derive_imports_test.go) -- which is what makes "a draft never reserves
// space" a property of the import graph rather than of care: this package
// cannot reach internal/netbox, internal/storage, internal/service,
// internal/cloud, internal/transport or the AWS SDK because nothing here
// imports a path that could get there.
//
// This package defines its own, unexported input types for what a decoded
// migration.yaml plan carries (Plan, Move, ...)
// rather than importing package M3b1's exported Plan/Wave/Move/Target types.
// The two packages are built in parallel in the same internal/migrate
// package (ADR 0015: "M3b1 and M3b2 may run in parallel if M3b2 defines the
// input types it needs and M3b1 maps onto them, exactly as M1b1 and M1b2
// did"), and this package's own report to the lead maps each of these types
// field by field onto the record's schema so the two can be unified at
// review.
//
// Entry point: Derive builds a Report from a decoded plan, an assess.Report,
// its assess.Input, a slice of DerivedEvidenceFile, and Options. The two head
// properties every test in this package answers to, named in ADR 0015's
// evidence paragraph, are: no move is reported as fully evidenced on typed
// input alone (see TestNoMoveIsFullyEvidencedOnTypedInputAlone and
// TestMutatingTypedFieldsNeverChangesADerivedFact in derive_test.go), and a
// draft never reserves space -- which for this package, which allocates
// nothing and touches no store, is the import-graph property above plus the
// determinism and forbidden-phrase guards in report_test.go.
package migrate

import (
	"sort"

	"github.com/mischapogr/platform-ipam/internal/assess"
)

// key is the map key a subject is grouped under, distinct from Identity()
// only in that it cannot be confused with a rendered string.
func (s Subject) key() string {
	return s.AccountID + "\x1f" + s.Region + "\x1f" + s.VPCID
}

// The derivation reads the plan's own types (plan.go): there is no second
// schema to map onto. A move's `Wave` is the wave's id, a target's
// `PrefixLength` is nil where the plan did not state one, and a subject's
// identity is Subject.Identity().

// Options is everything besides the plan, the assessment and the evidence
// that shapes a Report: the operator-supplied archival stamp (ADR 0015:
// "the --stamp flag is the only place an archival label enters, exactly as
// in ADR 0014, and it is the only field in the report that did not come out
// of an input file") and the exact bytes of every --plan input file, so
// Report.Inputs can name what produced the report without this package
// doing any I/O of its own -- the same InputFile pattern
// internal/assess.Options already uses, reused here rather than duplicated
// because it is an exported type of a package this one already imports.
type Options struct {
	Stamp     string
	PlanFiles []assess.InputFile
}

// Derive builds a Report from plan, the assessment report and the Input it
// was computed from, evidence and opts. It performs no I/O, calls neither
// time.Now nor any source of randomness, and never returns an error: this
// package's own refusal conditions belong to M3b1's structural validation,
// which has already run by the time a plan reaches here, and every
// remaining condition (missing evidence, evidence whose scope excludes a
// tenant, a stale conflict id, an unmatched subject) is a fact this
// function names in the Report rather than a reason to produce none, in the
// same spirit as assess.Assess degrading rather than refusing for every
// condition but the five ADR 0014 makes a refusal.
func Derive(plan Plan, report assess.Report, input assess.Input, evidence []DerivedEvidenceFile, opts Options) Report {
	idx := buildEvidenceIndex(evidence)
	touching := touchingConflicts(report.Conflicts)
	knownSubjects := knownSubjectSet(input)
	knownAccounts := knownAccountSet(input)
	reportConflictIDs := conflictIDSet(report.Conflicts)

	moves := make([]MoveResult, 0, len(plan.Moves))
	for _, m := range plan.Moves {
		moves = append(moves, deriveMove(m, report, input, idx, touching, knownSubjects, knownAccounts, reportConflictIDs))
	}
	sortMoves(moves)

	unplanned := unplannedConflictIDs(report.Conflicts, plan.Moves)

	targetTenants := targetTenantSet(plan.Moves)
	evComplete := idx.complete(report.Coverage.Complete, targetTenants)

	waves := buildWaveRollups(plan.Waves, moves)

	summary := buildSummary(moves, waves, unplanned, evComplete, report, idx, targetTenants, opts.Stamp)

	inputs := buildTopLevelDigests(opts.PlanFiles, idx.files)

	return Report{
		ReportVersion: reportVersion,
		Stamp:         opts.Stamp,
		Inputs:        inputs,
		InputLimits:   idx.inputLimits(targetTenants),
		Assessment: EmbeddedAssessment{
			Inputs:      report.Inputs,
			InputLimits: report.InputLimits,
			Coverage:    report.Coverage,
			Summary:     report.Summary,
		},
		Evidence: EvidenceReport{
			Complete:     evComplete,
			Scopes:       idx.sortedScopes(),
			ReadInstants: idx.sortedReadInstants(),
			Digests:      idx.sortedDigests(),
		},
		Summary:   summary,
		Waves:     waves,
		Moves:     moves,
		Unplanned: unplanned,
		Notes:     disclaimerNotes(report),
	}
}

func sortMoves(moves []MoveResult) {
	sort.SliceStable(moves, func(i, j int) bool {
		if moves[i].WaveID != moves[j].WaveID {
			return moves[i].WaveID < moves[j].WaveID
		}
		return moves[i].Subject < moves[j].Subject
	})
}

func knownSubjectSet(input assess.Input) map[string]bool {
	out := map[string]bool{}
	for _, r := range input.Records {
		if r.Type != assess.TypeVPC {
			continue
		}
		out[Subject{AccountID: r.AccountID, Region: r.Region, VPCID: r.ResourceID}.key()] = true
	}
	return out
}

func knownAccountSet(input assess.Input) map[string]bool {
	out := map[string]bool{}
	for _, a := range input.Accounts {
		out[a.AccountID] = true
	}
	return out
}

func conflictIDSet(conflicts []assess.Conflict) map[string]bool {
	out := map[string]bool{}
	for _, c := range conflicts {
		out[c.ID] = true
	}
	return out
}

// touchingConflicts indexes every non-fixed side of every conflict by its
// subject key, so a move's unclaimed list and a keep move's
// not-adoptable check can find every conflict touching a subject without a
// linear scan per move.
func touchingConflicts(conflicts []assess.Conflict) map[string][]assess.Conflict {
	out := map[string][]assess.Conflict{}
	for _, c := range conflicts {
		seen := map[string]bool{}
		for _, s := range c.Sides {
			if s.Fixed {
				continue
			}
			key := Subject{AccountID: s.AccountID, Region: s.Region, VPCID: s.VPCID}.key()
			if seen[key] {
				continue
			}
			seen[key] = true
			out[key] = append(out[key], c)
		}
	}
	return out
}

func targetTenantSet(moves []Move) []string {
	set := map[string]bool{}
	for _, m := range moves {
		if m.Target != nil && m.Target.TenantID != "" {
			set[m.Target.TenantID] = true
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// unplannedConflictIDs returns, sorted, the id of every conflict the
// current assessment reports for which NEITHER side's subject is named by
// any move in the plan at all (ADR 0015: "A conflict the assessment reports
// on a subject no move mentions is unplanned"). A conflict with one side
// that IS a move's subject but which that move did not list in `resolves`
// is `unclaimed` for that move (derive_facts.go), not `unplanned`: this
// package reads "a subject no move mentions" as "neither side's subject
// appears anywhere in the plan", the narrower of two readings the record
// does not fully disambiguate -- see the report to the lead.
func unplannedConflictIDs(conflicts []assess.Conflict, moves []Move) []string {
	planned := map[string]bool{}
	for _, m := range moves {
		planned[m.Subject.key()] = true
	}
	var out []string
	for _, c := range conflicts {
		anyPlanned := false
		for _, s := range c.Sides {
			if s.Fixed {
				continue
			}
			if planned[(Subject{AccountID: s.AccountID, Region: s.Region, VPCID: s.VPCID}).key()] {
				anyPlanned = true
				break
			}
		}
		if !anyPlanned {
			out = append(out, c.ID)
		}
	}
	sort.Strings(out)
	return out
}
